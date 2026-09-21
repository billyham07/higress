---
title: AI 配额管理
keywords: [ AI网关, AI配额 ]
description: AI 配额管理插件配置参考
---

## 功能说明

`ai-quota` 插件实现给特定 consumer 根据分配固定的 quota 进行 quota 策略限流，同时支持 quota 管理能力，包括查询 quota 、刷新 quota、增减 quota。

`ai-quota` 插件需要配合 认证插件比如 `key-auth`、`jwt-auth` 等插件获取认证身份的 consumer 名称，同时需要配合 `ai-statistics` 插件获取 AI Token 统计信息。

## 运行属性

插件执行阶段：`默认阶段`
插件执行优先级：`750`

## 配置说明

| 名称                 | 数据类型            | 填写要求                                 | 默认值 | 描述                                         |
|--------------------|-----------------|--------------------------------------| ---- |--------------------------------------------|
| `redis_key_prefix` | string          |  选填                                     |   chat_quota:   | qutoa redis key 前缀                         |
| `admin_consumer`   | string          | 必填                                   |      | 管理 quota 管理身份的 consumer 名称                 |
| `admin_path`       | string          | 选填                                   |   /quota   | 管理 quota 请求 path 前缀                        |
| `enable_path_suffixes` | []string     | 选填                                   |  ["/v1/chat/completions", "/v1/messages"] | 启用配额校验的请求路径后缀（仅用于 completion 请求，不影响管理接口路径） |
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


## 积分计费（可选）

Redis 里的计数器一直只有一个数字，请求把它减掉。积分改变的只是这个数字怎么算出来：不再是「一个 token 一个单位」，而是本次用量乘以该模型配置的价格。

**不配 `default_price` 也不配 `model_prices` 时，行为和以前完全一致**（一个 token 扣一个单位）。因此这个版本可以先发布、后配价，没改过配置的路由不会有任何变化。

模型名取自请求头 `x-higress-llm-model`，由 `model-router` 插件在 AUTHN 阶段写入，早于本插件。因此 completion 路径仍然不读请求体。

| 名称 | 数据类型 | 填写要求 | 默认值 | 描述 |
| --- | --- | --- | --- | --- |
| `default_price` | object | 选填 | - | 本路由上所有没有单独定价的模型使用的价格 |
| `model_prices` | object | 选填 | - | 以模型名为键的价格表，优先于 `default_price` |

价格对象的字段：

| 配置项 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `unit` | string | `tokens` | 计量单位：`tokens` 或 `requests` |
| `per` | int | 1 | 一份费率覆盖多少个计量单位。按千 token 报价填 `1000` |
| `input_micros` | int | 0 | 输入 token 费率，单位微积分（1e6 微积分 = 1 积分） |
| `output_micros` | int | 0 | 输出 token 费率 |
| `cache_read_micros` | int | 0 | 缓存命中 token 费率 |
| `cache_write_micros` | int | 0 | 缓存写入 token 费率 |
| `request_micros` | int | 0 | `unit: requests` 时，一次请求的费率 |
| `min_charge` | int | 0 | 算下来不足 1 积分时的下限。默认 0，即费率为 0 就真的免费 |

费率是**整数**的微积分数，配置和运算里都不出现浮点；小数费率会被拒绝而不是被截断成 0。四个 token 分量按互不重叠处理后相加，这与插件原有的口径一致（Anthropic 把 cache read / cache creation 报在 `input_tokens` 之外）。

`unit: requests` 是给响应里根本没有用量的接口用的——按次计费，不需要把它硬掰成 token。`unit: tokens` 的模型如果拿不到用量，本次不扣，也不会扣一个猜出来的数。

### 示例：百炼按模型定价，其余模型走路由默认价

```yaml
redis_key_prefix: "chat_quota:"
admin_consumer: consumer3
admin_path: /quota
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

### 识别请求参数 apikey，进行区别限流
```yaml
redis_key_prefix: "chat_quota:"
admin_consumer: consumer3
admin_path: /quota
redis:
  service_name: redis-service.default.svc.cluster.local
  service_port: 6379
  timeout: 2000
```


###  刷新 quota

如果当前请求 url 的后缀符合 admin_path，例如插件在 example.com/v1/chat/completions 这个路由上生效，那么更新 quota 可以通过
curl https://example.com/v1/chat/completions/quota/refresh -H "Authorization: Bearer credential3" -d "consumer=consumer1&quota=10000" 

Redis 中 key 为 chat_quota:consumer1 的值就会被刷新为 10000

### 查询 quota

查询特定用户的 quota 可以通过 curl https://example.com/v1/chat/completions/quota?consumer=consumer1 -H "Authorization: Bearer credential3"
将返回： {"quota": 10000, "consumer": "consumer1"}

### 增减 quota 

增减特定用户的 quota 可以通过 curl https://example.com/v1/chat/completions/quota/delta -d "consumer=consumer1&value=100" -H "Authorization: Bearer credential3"
这样 Redis 中 Key 为 chat_quota:consumer1 的值就会增加100，可以支持负数，则减去对应值。

