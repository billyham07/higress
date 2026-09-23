---
title: AI 配额管理
keywords: [ AI网关, AI配额 ]
description: AI 配额管理插件配置参考
---

## 功能说明

`ai-quota` 插件按「账户积分钱包」做准入与扣费：每个 consumer（一把 Key）指向一个钱包，请求前检查钱包和 Key 自己的上限，响应后按价格从钱包扣减。钱包与 Key 记录由管理后台（higress-ai-key-admin-v2）写入，插件只读和扣减，不提供管理接口。

`ai-quota` 插件需要配合 认证插件比如 `key-auth`、`jwt-auth` 等插件获取认证身份的 consumer 名称，同时需要配合 `ai-statistics` 插件获取 AI Token 统计信息。

## 运行属性

插件执行阶段：`默认阶段`
插件执行优先级：`750`

## 配置说明

| 名称                 | 数据类型            | 填写要求                                 | 默认值 | 描述                                         |
|--------------------|-----------------|--------------------------------------| ---- |--------------------------------------------|
| `enable_path_suffixes` | []string     | 选填                                   |  ["/v1/chat/completions", "/v1/messages"] | 启用积分校验与扣费的请求路径后缀 |
| `redis`            | object          | 是                                    |      | redis相关配置                                  |

`redis`中每一项的配置字段说明

| 配置项       | 类型   | 必填 | 默认值                                                     | 说明                                                                                         |
| ------------ | ------ | ---- | ---------------------------------------------------------- | ---------------------------                                                                  |
| service_name | string | 必填 | -                                                          | redis 服务名称，带服务类型的完整 FQDN 名称，例如 my-redis.dns、redis.my-ns.svc.cluster.local |
| service_port | int    | 否   | 服务类型为固定地址（static service）默认值为80，其他为6379 | 输入redis服务的服务端口                                                                      |
| username     | string | 否   | -                                                          | redis用户名                                                                                  |
| password     | string | 否   | -                                                          | redis密码                                                                                    |
| timeout      | int    | 否   | 1000                                                       | redis连接超时时间，单位毫秒                                                                  |
| database     | int    | 否   | 0                                                          | 使用的数据库id，例如配置为1，对应`SELECT 1`                                                  |


旧版的 `redis_key_prefix`、`admin_consumer`、`admin_path` 已不再读取：它们操作的
`chat_quota:<consumer>` 计数器没有任何组件再读，保留管理接口只会让人误以为改了额度。

## 积分钱包（Redis 契约）

所有金额单位都是**毫积分**（千分之一积分）。

| Key | 字段 | 含义 |
|---|---|---|
| `credit_key:<consumer>` | `owner` | 这把 Key 扣哪个钱包：`u:<用户>`、`g:<项目>`，或未归属的 consumer 自己的 `p:<consumer>` |
| | `mode` | Key 自己的上限：`none` 不限 / `period` 每个刷新周期 / `once` 一次性 |
| | `limit` | 上限金额，`mode=none` 时不看 |
| | `period` / `period_used` | 这把 Key 在哪个周期、用了多少；周期变了自动从 0 算 |
| | `once_used` | 自一次性上限设置以来用了多少 |
| `credit_wallet:<owner>` | `period` | 当前刷新周期，由管理后台推进 |
| | `allowance` | 每个周期发放的积分 |
| | `period_left` | 本周期还剩多少，可为负（最后一次请求的超额） |
| | `extra_left` | 额外积分（一次性，不随周期刷新） |
| | `unlimited` | `1` 表示不限额（项目默认如此），没有这个字段就是限额 |

准入：Key 记录或钱包缺失 → 403 `ai-quota.no_account`；`max(period_left,0)+max(extra_left,0) <= 0`
→ 403 `ai-quota.noquota`（不限额的钱包跳过这一条）；Key 自己的上限用尽 → 403 `ai-quota.key_limit`
（不限额的钱包也照样检查）。Redis 出错一律拒绝。

扣费：先扣本周期额度，再扣额外积分；两者都不够的部分记在 `period_left` 上成为负数，
由下一次周期刷新抵消，不会吃掉以后发放的额外积分。不限额的钱包全额记在 `period_left` 上、
不动额外积分，所以「本周期已用」照样能算，改回限额时额外积分还在。Key 的计数总是按全额累加——
Key 的上限只决定它能不能继续花钱包里的积分，Key 本身不持有积分。

## 积分计费（可选）

Redis 里的计数器一直只有一个数字，请求把它减掉。积分改变的只是这个数字怎么算出来：不再是「一个 token 一个单位」，而是本次用量乘以该模型配置的价格。

**账本单位是毫积分（千分之一积分）。** 计数器里存的、日志字段 `credit_millis` 里写的、网关计数器 `route_upstream_model_consumer_metric_credit_millis` 里加的，都是毫积分。原因是算术上的：价格按百万 token 报，而整数积分装不下这么细的金额 —— 按 8 积分/百万输入算，一次 2000 token 的请求值 0.016 积分，四舍五入到整数积分就是 0，配额一分不扣。用千分位记账，同样这笔扣 16 毫积分，误差上限 0.0005 积分且无偏。

**不配 `default_price` 也不配 `model_prices` 时，行为和以前完全一致**（一个 token 扣一个整积分，即 1000 毫积分）。因此这个版本可以先发布、后配价，没改过配置的路由不会有任何变化。

模型名取自请求头 `x-higress-llm-model`，由 `model-router` 插件在 AUTHN 阶段写入，早于本插件。因此 completion 路径仍然不读请求体。

| 名称 | 数据类型 | 填写要求 | 默认值 | 描述 |
| --- | --- | --- | --- | --- |
| `default_price` | object | 选填 | - | 本路由上所有没有单独定价的模型使用的价格 |
| `model_prices` | object | 选填 | - | 以模型名为键的价格表，优先于 `default_price` |

价格对象的字段：

| 配置项 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `unit` | string | `tokens` | 计量单位：`tokens` 或 `requests` |
| `per` | int | 1 | 一份费率覆盖多少个计量单位。按千 token 报价填 `1000`，按百万填 `1000000`。**它只是刻度**：`per` 同时缩放费率和除数，同一个价换任何刻度写都扣一样的钱 |
| `input_micros` | int | 0 | 输入 token 费率，单位微积分（1e6 微积分 = 1 积分） |
| `output_micros` | int | 0 | 输出 token 费率 |
| `cache_read_micros` | int | 0 | 缓存命中 token 费率 |
| `cache_write_micros` | int | 0 | 缓存写入 token 费率 |
| `request_micros` | int | 0 | `unit: requests` 时，一次请求的费率 |
| `min_charge_millis` | int | 0 | 算下来不足 1 毫积分时的下限。默认 0，即费率为 0 就真的免费 |

费率是**整数**的微积分数，配置和运算里都不出现浮点；小数费率会被拒绝而不是被截断成 0。四个 token 分量按互不重叠处理后相加，这与插件原有的口径一致（Anthropic 把 cache read / cache creation 报在 `input_tokens` 之外）。

`unit: requests` 是给响应里根本没有用量的接口用的——按次计费，不需要把它硬掰成 token。`unit: tokens` 的模型如果拿不到用量，本次不扣，也不会扣一个猜出来的数。

### 示例：百炼按模型定价，其余模型走路由默认价

```yaml
redis:
  service_name: redis.dns
  service_port: 6379
  timeout: 2000
default_price:
  unit: tokens
  per: 1000
  input_micros: 1000000      # 1 积分 / 1000 输入 token
  output_micros: 4000000     # 4 积分 / 1000 输出 token
model_prices:
  qwen3.7-max:
    unit: tokens
    per: 1000
    input_micros: 2000000
    output_micros: 8000000
    cache_read_micros: 200000
    cache_write_micros: 2500000
```

### 示例：自建算力，费率为零

```yaml
default_price:
  unit: tokens
  per: 1
  # 四个费率都不填即为 0：这条路由上的模型不计费。
  # 这是把意图写下来，而不是靠「不给这条路由挂插件」来表达。
```

## 配置示例

```yaml
enable_path_suffixes:
  - /v1/chat/completions
  - /v1/messages
redis:
  service_name: redis-service.default.svc.cluster.local
  service_port: 6379
  timeout: 2000
```
