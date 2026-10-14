---
title: AI Quota Management
keywords: [ AI Gateway, AI Quota ]
description: AI quota management plugin configuration reference
---
## Function Description
The `ai-quota` plugin admits and charges token usage against an account wallet. A key (consumer) holds no quota of its own; it points at a wallet:

- `quota_key:<consumer>` (hash): `owner`, the wallet id (`u:<user>`, `g:<project>` or `p:<consumer>`); the key's own cap `mode` (`none`, `period` or `once`) and `limit`; and the key's counters `period`, `period_used`, `once_used`.
- `quota_wallet:<owner>` (hash): `period`, `allowance`, `period_left` (may go negative; the next refresh forgives the overshoot) and `unlimited` (`"1"` admits whatever is left while still counting usage).

Admission reads the key, then its wallet, and answers 403 when the wallet has nothing left (and is not unlimited) or the key reached its own cap. A missing record or a Redis error denies (fail-closed). After the response a Lua script charges the total tokens to both hashes atomically.

The admin console creates wallets, sets allowances, refreshes periods and writes key caps; the plugin only reads and charges. The retired per-key `chat_quota:` counter and the `admin_path` query/refresh/delta endpoints are gone.

## Runtime Properties
Plugin execution phase: `default phase`
Plugin execution priority: `750`

## Configuration Description
| Name                 | Data Type        | Required | Default Value | Description |
|---------------------|------------------|----------|---------------|-------------|
| `enable_path_suffixes` | []string      | Optional | ["/v1/chat/completions", "/v1/messages"] | Path suffixes the quota gate admits and charges |
| `redis`             | object           | Yes      |               | Redis configuration |

`redis_key_prefix`, `admin_consumer` and `admin_path` are no longer read; a configuration that still carries them loads unchanged.

Explanation of each configuration field in `redis`
| Configuration Item | Type   | Required | Default Value                                           | Explanation                                                                                             |
|--------------------|--------|----------|---------------------------------------------------------|---------------------------------------------------------------------------------------------------------|
| service_name       | string | Required | -                                                       | Redis service name, full FQDN name with service type, e.g., my-redis.dns, redis.my-ns.svc.cluster.local |
| service_port       | int    | No       | Default value for static service is 80; others are 6379 | Service port for the redis service                                                                      |
| username           | string | No       | -                                                       | Redis username                                                                                          |
| password           | string | No       | -                                                       | Redis password                                                                                          |
| timeout            | int    | No       | 1000                                                    | Redis connection timeout in milliseconds                                                                |
| database           | int    | No       | 0                                                       | The database ID used, for example, configured as 1, corresponds to `SELECT 1`.                          |

## Configuration Example
```yaml
enable_path_suffixes: ["/v1/chat/completions", "/v1/messages", "/v1/responses"]
redis:
  service_name: redis-service.default.svc.cluster.local
  service_port: 6379
  timeout: 2000
```
