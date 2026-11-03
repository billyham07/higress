# ai-credits

Independent Higress WASM plugin for unified-model credit admission and settlement.

It does **not** mutate `chat_quota:*`. MySQL is the durable ledger (v2); Redis is rebuildable. This plugin only calls v2's private atomic APIs with a service key.

## Frozen contract (`ai-credits.v1`)

v2 must implement these defaults. The WasmPlugin template already points at them.

| Item | Default |
| --- | --- |
| Admit | `POST /internal/ai-credits/v1/admit` |
| Settle | `POST /internal/ai-credits/v1/settle` |
| Service | `higress-ai-key-admin-v2.higress-system.svc.cluster.local:80` |
| Auth | header `X-AI-Credits-Service-Key` |
| Identity | `x-mse-consumer` after key-auth; client `X-AI-Credits-*` headers are stripped |

Client-forged budget subject, reservation id, or service key never determine the payer. The consumer comes from key-auth.

## Modes

| mode | Behaviour |
| --- | --- |
| `off` | No-op. Existing Token quota plugins keep working. |
| `shadow` | Admit/settle are called; denials are logged; the request continues. |
| `enforce` | Fail closed on deny, unknown price, or admission service errors. |

Missing usage, stream disconnect, or upstream 5xx with incomplete usage settle as `pending_verify`, never as zero consumption.

## Settlement delivery

Settlement is the only message carrying a request's real cost, and the plugin cannot prove one arrived: the answer comes back on a callback, and Envoy destroys the stream context as soon as the response is finished.

So delivery has two tiers:

1. **Send.** `POST settlePath`, retried `settleRetries` times on 5xx/429/408. A 4xx other than 429 is a contract refusal and is not retried.
2. **Durable record.** Before the first attempt the payload is written to Redis as `HSET <key> <reservationId> <entry>`, and the plugin waits for Redis 7.2 `WAITAOF 1 0 <timeout>` to confirm a local AOF fsync. An HSET response alone means only that Redis accepted the command; it is not a persistence acknowledgement. A live 2xx callback compare-deletes the exact wrapper value when it arrives. The backend leaves a real Redis record for the value-aware replay pass because it receives only the inner settle request and an unconditional `HDEL` could erase a newer value for the same field. Whatever remains in the hash is replayed through the same idempotent settle path; a duplicate answer then compare-deletes the exact value and moves no money.

A stream that ends without any terminal chunk -- the client hung up -- records a `pending_verify` / `disconnect` settlement from the stream-done hook. That request previously produced no settlement at all and left its reservation for v2's recovery sweep to quarantine.

| Key | Meaning |
| --- | --- |
| `settlementOutbox.service_name` / `service_port` | The Redis v2 reads, named by its **McpBridge registry entry** (`redis.dns`), not by a DNS name. A cluster the registry does not carry is refused with `bad argument` and the channel silently stays unready. Required to enable the channel. |
| `settlementOutbox.key` | Hash name, default `v2:credits:settle:outbox`. It must match v2's `AI_CREDITS_OUTBOX_KEY`. |
| `settlementOutbox.username` / `password` / `database` / `timeout` | Connection settings, as in the other Redis-backed plugins. |

The Redis instance must be Redis 7.2 or newer with `appendonly yes`. AOF
`appendfsync everysec` is acceptable because `WAITAOF` waits for the local fsync
after each HSET; an RDB-only instance or a command acknowledgement alone does
not satisfy this contract. If HSET or WAITAOF fails, the exact payload is
logged and the live request makes one direct idempotent attempt; a failed direct
attempt remains recoverable only if the HSET record was accepted, otherwise the
log is the evidence for an operator-led replay. Enforce deployments must also
make v2's outbox readiness check pass before creating a paid reservation.

Without `settlementOutbox` the plugin behaves exactly as before: the undelivered payload goes to the gateway log as one line of JSON and survives only if that log is collected.

**What this does not close:** usage that exists only in Envoy worker memory when the process is killed -- between a token arriving and the outbox write -- is gone. The reservation then stays quarantined in `pending_verify`; it is never zeroed and never charged an estimate.

**The record's timestamp crosses a clock boundary.** `enqueuedAtUnixMs` is stamped here and compared against the replay job's clock on another host. v2 treats a timestamp in the future as immediately replayable rather than deferring it, because a clock it does not control must not be able to postpone a charge -- the cluster this was built on runs 63 seconds of skew between the two nodes involved.

## Build

```bash
cd higress/plugins/wasm-go
PLUGIN_NAME=ai-credits make local-build
# artifact: extensions/ai-credits/main.wasm
```

For an artifact whose sha256 is worth recording, build it reproducibly:

```bash
GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false   -buildmode=c-shared -o plugin.wasm .
```

Without `-trimpath` the binary embeds its own build directory, so two builds of
identical source differ and the hash identifies a build rather than a source.
With it, any directory reproduces the same bytes.

Host tests:

```bash
cd higress/plugins/wasm-go/extensions/ai-credits
go test ./...
```
