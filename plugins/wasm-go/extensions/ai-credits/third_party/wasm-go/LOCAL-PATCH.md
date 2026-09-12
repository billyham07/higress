# 本地补丁说明

`third_party/wasm-go` 是 `github.com/higress-group/wasm-go`
**v1.0.10-0.20260120033417-1c84f010156d** 的副本，加上一个本地补丁。

`go.mod` 的 `replace` 指向这里，仅仅是为了携带这一个补丁。

## 唯一的本地改动：`pkg/wrapper/redis_wrapper.go`

`RedisInit` 把 buffering / database 选项作为查询参数拼进 hostcall 的
cluster 标识。派发命令时若只用 `C.ClusterName()` 这个基础名，重连并改变
这些选项之后，host 侧仍会沿用旧的客户端。补丁保存 `Init` 产出的**完整**
标识（`clusterName` 字段 + `redisDispatchCluster`），每条命令都用它派发。

ai-credits 的结算 outbox 依赖这个行为。

## 升级步骤

升级 wasm-go 时，把本目录整体替换为新版本，再重新应用上面这一个文件的
改动。如果新版本已含该修复，删除整个 `third_party/` 与 `go.mod` 里的
`replace`，改用普通版本化依赖——其余插件都是那样依赖它的。

v1.0.7 → v1.0.10 期间，原先随这棵树携带的另外两个补丁
（`log_wrapper.go` 的 `get_log_level`、`plugin_wrapper.go` 的
`WithMaxRequestsPerIoCycle`）已进入上游，本次升级后不再是本地改动。
