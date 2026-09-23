---
title: AI Quota Management
keywords: [ AI Gateway, AI Quota ]
description: AI quota management plugin configuration reference
---
## Function Description
The `ai-quota` plugin admits and charges requests against an account credit wallet. Every consumer (one key) points at a wallet; before a request the plugin checks the wallet and the key's own cap, and after the response it charges the wallet by price. The admin console (higress-ai-key-admin-v2) writes the wallet and key records; this plugin only reads and spends them and has no management endpoints. It works with an authentication plugin such as `key-auth` for the consumer name. See the Chinese README for the Redis contract.

## Runtime Properties
Plugin execution phase: `default phase`
Plugin execution priority: `750`

## Configuration Description
| Name                 | Data Type        | Required Conditions                         | Default Value | Description                                       |
|---------------------|------------------|--------------------------------------------|---------------|---------------------------------------------------|
| `enable_path_suffixes` | []string      | Optional                                   | ["/v1/chat/completions", "/v1/messages"] | Path suffixes that are admitted and charged against credits |
| `redis`             | object           | Yes                                        |               | Redis related configuration                        |
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
redis:
  service_name: redis-service.default.svc.cluster.local
  service_port: 6379
  timeout: 2000
```
