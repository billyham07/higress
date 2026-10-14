---
title: AI 配额管理
keywords: [ AI网关, AI配额 ]
description: AI 配额管理插件配置参考
---

## 功能说明

`ai-quota` 插件按「账户钱包」做 Token 额度准入和扣减。每把 Key（consumer）不再各自持有额度，而是指向一个账户钱包：

- `quota_key:<consumer>`（hash）：`owner` 指向的钱包 id（`u:<用户>` / `g:<项目>` / `p:<consumer>`），以及 Key 自己的上限 `mode`（`none` 不限 / `period` 每周期 / `once` 一次性）、`limit`，和 Key 的用量计数 `period`、`period_used`、`once_used`。
- `quota_wallet:<owner>`（hash）：`period` 当前周期、`allowance` 每周期额度、`period_left` 本周期剩余（可为负，表示超用，下次刷新抹平）、`unlimited`（`"1"` 表示不限额，照常计数但不拦截）。

准入：先读 Key 再读钱包；钱包剩余 ≤ 0（且未标记不限额）或 Key 达到自身上限时返回 403。任何一步读不到记录或 Redis 出错都拒绝（fail-closed）。

扣减：响应结束后用一段 Lua 在 Key 和钱包上原子扣除本次 total token；Key 记录的周期落后于钱包时，它的本周期计数在扣减时归零。

钱包的创建、额度、周期刷新、Key 上限都由管理控制台写入，插件只读和扣减。旧的按 Key 计数器 `chat_quota:` 以及 `admin_path` 查询/刷新/增减接口已移除。

`ai-quota` 插件需要配合认证插件（如 `key-auth`、`jwt-auth`）获取 consumer 名称。

## 运行属性

插件执行阶段：`默认阶段`
插件执行优先级：`750`

## 配置说明

| 名称                 | 数据类型            | 填写要求 | 默认值 | 描述 |
|--------------------|-----------------|------| ---- |----|
| `enable_path_suffixes` | []string     | 选填 | ["/v1/chat/completions", "/v1/messages"] | 启用额度准入和扣减的请求路径后缀 |
| `redis`            | object          | 是  |      | redis 相关配置 |

`redis_key_prefix`、`admin_consumer`、`admin_path` 已不再读取，旧配置里保留这些字段不影响加载。

`redis`中每一项的配置字段说明

| 配置项       | 类型   | 必填 | 默认值                                                     | 说明                                                                                         |
| ------------ | ------ | ---- | ---------------------------------------------------------- | ---------------------------                                                                  |
| service_name | string | 必填 | -                                                          | redis 服务名称，带服务类型的完整 FQDN 名称，例如 my-redis.dns、redis.my-ns.svc.cluster.local |
| service_port | int    | 否   | 服务类型为固定地址（static service）默认值为80，其他为6379 | 输入redis服务的服务端口                                                                      |
| username     | string | 否   | -                                                          | redis用户名                                                                                  |
| password     | string | 否   | -                                                          | redis密码                                                                                    |
| timeout      | int    | 否   | 1000                                                       | redis连接超时时间，单位毫秒                                                                  |
| database     | int    | 否   | 0                                                          | 使用的数据库id，例如配置为1，对应`SELECT 1`                                                  |


## 配置示例

```yaml
enable_path_suffixes: ["/v1/chat/completions", "/v1/messages", "/v1/responses"]
redis:
  service_name: redis-service.default.svc.cluster.local
  service_port: 6379
  timeout: 2000
```
