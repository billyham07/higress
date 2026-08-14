---
title: AI 模型清单
keywords: [higress, ai, models]
description: 按调用方身份返回其可用的模型清单，兼容 OpenAI `/v1/models`
---

## 功能说明

`ai-model-catalog` 在网关侧直接应答 OpenAI 兼容的 `GET /v1/models`，返回**当前调用方有权使用的模型**，不回源上游。

插件本身不解析凭证：它读取认证插件（如 `key-auth`）注入的 `X-Mse-Consumer` 请求头，拿到调用方名称后查配置里的授权表。因此使用本插件的路由**必须挂载认证插件**，否则所有请求都会被判为未认证。

授权表由控制面（如 higress-ai-key-admin）生成并写入插件配置，插件运行期不做任何外部调用。

## 运行属性

插件执行阶段：`认证阶段`
插件执行优先级：`300`

必须低于所在路由上认证插件的优先级（`key-auth` 为 `310`），以保证 `X-Mse-Consumer` 已经注入。

## 配置字段

| 名称             | 数据类型        | 填写要求 | 默认值           | 描述                                                        |
| ---------------- | --------------- | -------- | ---------------- | ----------------------------------------------------------- |
| `models`         | array of object | 必填     | -                | 模型全集，数组顺序即响应中的返回顺序                        |
| `consumers`      | object          | 选填     | `{}`             | 调用方名称到模型 id 列表的映射                              |
| `path`           | string          | 选填     | `/v1/models`     | 生效路径，非该路径的请求原样放行                            |
| `consumerHeader` | string          | 选填     | `x-mse-consumer` | 读取调用方名称的请求头                                      |
| `created`        | number          | 选填     | 插件加载时间     | 响应中 `created` 字段的默认值，秒级时间戳                   |

`models` 数组每项：

| 名称      | 数据类型 | 填写要求 | 默认值       | 描述                        |
| --------- | -------- | -------- | ------------ | --------------------------- |
| `id`      | string   | 必填     | -            | 模型 id，需全局唯一         |
| `ownedBy` | string   | 选填     | `higress`    | 响应中的 `owned_by` 字段    |
| `created` | number   | 选填     | 顶层 `created` | 该模型单独的 `created` 值 |

`consumers` 中引用了 `models` 里不存在的模型 id 时，该条目被忽略并打印 warning，不会导致配置加载失败——授权表由控制面同步生成，一处失配不应让整条路由不可用。

## 配置示例

```yaml
created: 1700000000
models:
  - id: qwen3.5
    ownedBy: yuexiu-private
  - id: deepseek-v4-flash
    ownedBy: yuexiu-private
  - id: PaddleOCR-VL-1.6
    ownedBy: yuexiu-private
consumers:
  u-alice-default-aaaaa:
    - qwen3.5
    - deepseek-v4-flash
  u-bob-default-bbbbb:
    - PaddleOCR-VL-1.6
```

## 行为说明

| 请求                                          | 响应                                                       |
| --------------------------------------------- | ---------------------------------------------------------- |
| `GET /v1/models`                              | `200`，`{"object":"list","data":[...]}`，仅含授权模型       |
| `GET /v1/models/<id>`，已授权                 | `200`，单个 model 对象                                     |
| `GET /v1/models/<id>`，未授权或不存在         | `404`，两种情况响应完全一致，避免泄露私有模型是否存在      |
| 调用方已认证但授权表中无记录                  | `200`，空列表（新建 Key 或同步未追上，不是错误）           |
| 缺少 `X-Mse-Consumer`                         | `401`，不返回任何模型信息                                  |
| 非 `GET`/`HEAD` 方法                          | `405`                                                      |
| 路径不匹配 `path`                             | 放行，交由后续插件处理                                     |

## 构建

```bash
PLUGIN_NAME=ai-model-catalog make build
```
