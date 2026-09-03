# vllm-compat

把客户端发来的 **vLLM 方言** guided decoding 字段，翻译成后端（SGLang / OpenAI 兼容服务）真正认识的标准字段。

背景：`aigw.yuexiuproperty.cn` 后面的 `qwen3.5` 等模型跑在 SGLang 上，SGLang 对 vLLM 专有的 `guided_json` / `guided_choice` 等字段是**静默忽略**的——不报错，直接返回自由文本。客户端（尤其是按 vLLM 写的 SaaS）拿到散文去 `json.Unmarshal`，报 `invalid character 'æ' looking for beginning of value`。

## 字段映射

| 客户端发的（vLLM） | 改写成（后端认识的） |
|---|---|
| `guided_json`（对象或 JSON 字符串） | `response_format = {"type":"json_schema","json_schema":{"name":…,"schema":…}}` |
| `guided_choice: ["a","b"]` | `regex = "(a\|b)"`（元字符已转义） |
| `guided_regex` | `regex` |
| `guided_grammar` | `ebnf` |
| `guided_decoding_backend`、`guided_whitespace_pattern` | 删除（只在有 guided_* 时才跟着删） |

`guided_regex` 比 `guided_choice` 更具体，两者同时出现时前者优先，后者原样保留以便上游发现冲突。

## 安全边界

插件被设计成「**没有 guided_\* 就完全不碰**」：

- 只在 `POST` 且路径后缀命中 `enableOnPathSuffix` 时读 body；不命中直接 `DontReadRequestBody()`。
- 只有 top-level 出现 `guided_*` 才改写；否则 body 一个字节都不动（`transformBody` 返回 `applied` 为空，不调 `ReplaceHttpRequestBody`）。
- 改写用 sjson 原地打补丁，**不做反序列化再序列化**，其余字段（`chat_template_kwargs`、`top_k`、自定义扩展等）原样保留。
- `overwriteExisting: false`（默认）时，客户端已经显式写了 `response_format` / `regex` / `ebnf` 就不覆盖——标准字段代表客户端的真实意图，vLLM 字段多半是 SDK 模板带出来的。
- body 不是合法 JSON 时直接放行，让上游去报错，不越权拦截。

## 配置

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `consumers` | []string | `[]` | 只对这些 consumer 生效；**空 = 该路由上所有调用方**。匹配 key-auth 注入的 `x-mse-consumer` |
| `consumerHeader` | string | `x-mse-consumer` | consumer 来源 header |
| `enableOnPathSuffix` | []string | `["/chat/completions","/completions"]` | 命中才处理 |
| `overwriteExisting` | bool | `false` | 是否覆盖客户端已写的标准字段 |
| `stripSource` | bool | `true` | 翻译完是否删掉原 `guided_*` 字段 |
| `schemaName` | string | `guided_json` | 生成的 `json_schema.name` |
| `guidedJson` / `guidedChoice` / `guidedRegex` / `guidedGrammar` | bool | `true` | 单独开关每条映射 |

> ⚠️ `guided_choice`/`guided_grammar` 映射到的 `regex`/`ebnf` 是 **SGLang 原生参数**。只把这个插件挂在 SGLang 后端的路由上；挂到真 vLLM 后端反而会报错。

## 构建与发布

```bash
cd higress/plugins/wasm-go
make build-push PLUGIN_NAME=vllm-compat PLUGIN_VERSION=1.0.0 \
  REGISTRY=yxdc-registry.cn-shenzhen.cr.aliyuncs.com/dockerhub/higress-wasm/
```

## 部署

见 `deploy/wasmplugin.yaml`。挂 `AUTHZ` phase，保证在 key-auth（AUTHN，才能拿到 `x-mse-consumer`）之后、ai-proxy（默认 phase，最后执行）之前。

灰度顺序建议：

1. 先按 `consumers` 白名单只放开这个 SaaS 的 consumer，观察一两天；
2. 确认没有副作用后，把 `consumers` 清空（改配置即可，不用重新构建），对该路由全量生效。
