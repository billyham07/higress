---
title: AI 输入早拒
keywords: [ AI网关, 上下文窗口, 请求体大小 ]
description: 在缓冲和分词之前拒绝注定失败的超大 AI 请求
---

## 功能说明

`ai-input-guard` 在请求进入其他插件缓冲、到达模型服务分词之前，拒绝两类注定失败的请求：

1. **请求体过大**：`Content-Length` 超过 `maxBodyBytes` 时在请求头阶段直接返回 413，不读请求体。chunked 请求由 Envoy 缓冲上限兜底返回 413。
2. **输入远超上下文**：对 JSON 请求体里所有字符串（跳过 data URI 和长 base64，即图片/音频/文件）按字符数估算 token **下界**，下界都超过该模型上下文窗口时返回 400 `context_length_exceeded`。

估算不做真分词：CJK 字符按 3 字符/token、其他字符按 8 字符/token，连续相同字符只计一次。2026-10-29 用 GLM-5.3-Flash、DeepSeek-V4-Flash、Qwen3.8 的真实分词器校准：自然文本（中英文档、代码、JSON、日志、base64）的估算值不超过真实值的 0.54 倍。因此被拒的请求，后端分词后也必然拒绝；放行的请求由后端做精确校验。

`/embeddings`、`/rerank` 的输入是批量、按条限制，总量和单个上下文窗口不可比，只做第 1 项。

请求体不超过「最小上下文 × 3」字节的请求不会被读取（生产 99.9% 的请求低于 4.3MB）。

动机见 `infra/models/glm-5.3-flash/glm-incident-20261029/FINDINGS.md`：一条 81MB / 3172 万 token 的请求让 GLM 唯一的 PD 对分词约两分钟，期间健康检查超时，网关返回 503。

## 运行属性

插件执行阶段：`认证阶段`（AUTHN）
插件执行优先级：`1100`，必须排在 model-router（900）之前——model-router 会在请求头阶段停住并缓冲整个请求体，排在它后面的请求头检查就失去了意义。

## 配置字段

| 名称 | 数据类型 | 默认值 | 描述 |
|------|---------|--------|------|
| `maxBodyBytes` | int | 33554432 | 请求体上限（字节），0 为不限制 |
| `defaultMaxContextTokens` | int | 1048576 | 未单独配置的模型的上下文窗口，0 为不做文本估算 |
| `modelMaxContextTokens` | map | {} | 按请求体 `model` 字段（客户端原始名）覆盖上下文窗口，0 为该模型不做估算 |
| `dryRun` | bool | false | 观察模式：只把判定写进 ai_log，不拦截。chunked 请求的缓冲上限放到 100MB，避免 Envoy 自己返回 413 |
| `enableOnPathSuffix` | array | `/completions` `/embeddings` `/rerank` `/messages` `/responses` | 只对这些路径后缀生效 |

默认值取 fleet 中最大的上下文窗口：未知模型名会落到 default 路由（DeepSeek，1M），legacy 8000 端口不论模型名都进 GLM（1M）。只给上下文更小的自建模型单独配置。

## 返回示例

路径以 `/messages` 结尾时返回 Anthropic 格式，否则返回 OpenAI 格式：

```json
{"error":{"type":"invalid_request_error","code":"context_length_exceeded","param":null,
 "message":"This model's maximum context length is 1048576 tokens, but the request input is at least 1048577 tokens. Please reduce the length of the messages."}}
```

两种模式都会往 ai_log 写 `input_guard`（`body_too_large` / `context_length_exceeded`，观察模式加 `dry_run:` 前缀）和 `input_guard_tokens`（估算下界，扫到超限即停，所以通常是上限 + 1）。SLS 里对应字段为 `ai_loginput_guard`、`ai_loginput_guard_tokens`，这两个字段没有建索引：用 `bytes_received >= 3145728` 筛出候选请求后看这两个字段，见 `artifacts/ai-input-guard-20261029/observe.py`。观察模式下请求照常往下走，ai-statistics 会把 consumer、model 合并进同一个 ai_log。

拦截时访问日志的 `response_code_details` 为 `via_wasm::<命名空间>.ai-input-guard::ai-input-guard.body_too_large` 或 `...::ai-input-guard.context_length_exceeded`。注意：
- 413 的 `bytes_received` 为 0（请求体还没读就拒了）。
- `route_name` 是 model-router 改路由之前的路由，通常是 `ai-route-default.internal`。
- 插件排在 key-auth 之前，被拒请求没有 consumer，要用 `x_forwarded_for` / `user_agent` 定位调用方。
- 不带 Content-Length 的 chunked 请求超限时，由 Envoy 自己返回纯文本 `Payload Too Large`，不是上面的 JSON。

## 构建

```bash
GOTOOLCHAIN=go1.24.4 GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o main.wasm .
```
