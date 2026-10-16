package main

// The settlement outbox is the durable half of settlement delivery.
//
// A settlement is dispatched over HTTP while the request is still alive, and
// that call is the fast path. It has one property no retry count can fix: the
// answer arrives on a callback, and Envoy destroys the stream context as soon
// as the response is finished. A settlement whose ACK never arrived is not a
// settlement that is known to have failed -- it is one whose outcome this
// process cannot know, and usage that lives only in Envoy's worker memory dies
// with the worker.
//
// So the payload is written to Redis BEFORE the HTTP call and the plugin waits
// for both the HSET response and Redis 7.2's local WAITAOF response. An HSET
// response only means that Redis accepted the command; it is not evidence that
// the bytes survived a process or node failure. A live 2xx callback can
// compare-delete the exact wrapper value, but the backend receives only the
// inner settle request and therefore leaves a real Redis record for the
// value-aware replay pass. An unconditional HDEL there could erase a newer
// value for the same field. The acknowledgement callback below was measured
// not to run in a real gateway because Envoy destroys the stream context first.
// Whatever is still in the hash is replayed through the same idempotent settle
// path; a duplicate answer then compare-deletes the exact value and moves no
// money.
//
// Redis is a separate process with its own persistence, which is the point: the
// payload outlives a gateway pod that is killed a millisecond later. What it
// cannot outlive is a kill BEFORE the write. Usage destroyed in that window is
// gone, and the reservation stays quarantined in pending_verify rather than
// being guessed at.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/resp"
)

// DefaultOutboxKey is the Redis hash the backend drains. The field is the
// reservation id, which is what makes the record idempotent on both ends: a
// retry of the same reservation overwrites its own field instead of queueing a
// second copy.
const DefaultOutboxKey = "v2:credits:settle:outbox"

const outboxForgetIfMatchScript = `
local current = redis.call('HGET', KEYS[1], ARGV[1])
if not current then
  return 0
end
if current ~= ARGV[2] then
  return -1
end
redis.call('HDEL', KEYS[1], ARGV[1])
return 1
`

// Outbox causes, carried so the backend can tell an undelivered settlement
// apart from one that never got the chance to be sent.
const (
	OutboxCauseDispatch   = "dispatch"
	OutboxCauseDisconnect = "disconnect"
)

// SettlementOutbox is the configured durable channel. A nil outbox is the
// documented degraded mode: delivery falls back to the log line, which survives
// the pod only if the log is collected.
type SettlementOutbox struct {
	Client wrapper.RedisClient
	Key    string
	// Timeout is also used as the WAITAOF deadline in milliseconds.
	Timeout int64

	// The proxy Redis hostcall reports a connection failure through the
	// callback, while the wrapper leaves its ready bit set and does not
	// recreate the host-side connection. Keep the original init parameters so
	// the root verifier can explicitly re-initialize the same logical client
	// before retrying the complete HSET -> WAITAOF pair.
	serviceName        string
	servicePort        int64
	username           string
	password           string
	database           int
	reconnectMu        sync.Mutex
	lastReconnect      time.Time
	reconnectEpoch     uint64
	lastReconnectTrace time.Time
}

// OutboxEntry is the frozen wire form of one durable settlement record. It
// mirrors internal/domain/credits.OutboxEntry in v2 field for field.
type OutboxEntry struct {
	ContractVersion  string          `json:"contractVersion"`
	EnqueuedAtUnixMs int64           `json:"enqueuedAtUnixMs"`
	Cause            string          `json:"cause"`
	Settle           json.RawMessage `json:"settle"`
}

func parseSettlementOutbox(config gjson.Result) (*SettlementOutbox, error) {
	node := config.Get("settlementOutbox")
	if !node.Exists() {
		return nil, nil
	}
	serviceName := strings.TrimSpace(node.Get("service_name").String())
	if serviceName == "" {
		serviceName = strings.TrimSpace(node.Get("service.name").String())
	}
	if serviceName == "" {
		return nil, errors.New("settlementOutbox.service_name is required")
	}
	servicePort := int(node.Get("service_port").Int())
	if servicePort == 0 {
		servicePort = int(node.Get("service.port").Int())
	}
	if servicePort == 0 {
		servicePort = 6379
	}
	key := strings.TrimSpace(node.Get("key").String())
	if key == "" {
		key = DefaultOutboxKey
	}
	timeout := node.Get("timeout").Int()
	if timeout <= 0 {
		timeout = 1000
	}
	cluster := wrapper.FQDNCluster{
		FQDN: serviceName,
		Port: int64(servicePort),
	}
	client := wrapper.NewRedisClusterClient(cluster)
	// Buffering exists to save round trips on hot paths. This is not one: a
	// record still sitting in the plugin's buffer when the worker dies is a
	// record that was never durable, which is the exact failure the outbox
	// exists to prevent.
	username := node.Get("username").String()
	password := node.Get("password").String()
	database := int(node.Get("database").Int())
	if err := client.Init(username, password, timeout,
		wrapper.WithDataBase(database), wrapper.WithDisableBuffer()); err != nil {
		return nil, err
	}
	return &SettlementOutbox{
		Client:      client,
		Key:         key,
		Timeout:     timeout,
		serviceName: serviceName,
		servicePort: int64(servicePort),
		username:    username,
		password:    password,
		database:    database,
	}, nil
}

// reconnect re-runs RedisInit after a host-side connection failure. The
// wasm-go Redis wrapper only marks a client not-ready for authentication
// failures; transport failures otherwise leave a stale logical client ready,
// so retrying HSET alone can fail forever after a Redis Pod replacement.
// Reinitialization is serialized because the verifier can tick every 100 ms
// while Redis is unavailable. It is called only after a failed barrier, never
// between the HSET and WAITAOF commands of one attempt.
func (o *SettlementOutbox) reconnect() error {
	if o == nil || o.Client == nil || o.serviceName == "" || o.servicePort == 0 {
		return errors.New("redis outbox reconnect is not configured")
	}
	o.reconnectMu.Lock()
	defer o.reconnectMu.Unlock()
	if !o.lastReconnect.IsZero() && time.Since(o.lastReconnect) < time.Second {
		return nil
	}
	o.lastReconnect = time.Now()
	// RedisInit is keyed by the complete hostcall configuration. On the
	// gateway version used by the test environment, repeating the same
	// configuration can return success while retaining the dead async client
	// created before a Redis Pod replacement. Alternate a harmless supported
	// flush timeout while keeping the maximum buffer size at zero. This changes
	// the host configuration and forces a new async connection, while both
	// variants still flush every command immediately; the HSET -> WAITAOF pair
	// remains ordered on that fresh connection.
	o.reconnectEpoch++
	if o.lastReconnectTrace.IsZero() || time.Since(o.lastReconnectTrace) >= time.Second {
		log.Warnf("ai-credits real verifier build=%s event=reconnect_start epoch=%d old_ready=%t",
			realEnvVerifierBuildID, o.reconnectEpoch, o.Client.Ready())
		o.lastReconnectTrace = time.Now()
	}
	// Create a new wrapper as well as a new hostcall identity. Re-running Init
	// on the old wrapper leaves its ready state and any host-side client cache
	// attached to the failed object; a fresh object makes the replacement
	// explicit and lets the next barrier pair capture the new client pointer.
	client := wrapper.NewRedisClusterClient(wrapper.FQDNCluster{
		FQDN: o.serviceName,
		Port: o.servicePort,
	})
	flushTimeout := int64(0)
	if o.reconnectEpoch%2 == 1 {
		flushTimeout = 1
		if err := client.Init(o.username, o.password, o.Timeout,
			wrapper.WithDataBase(o.database),
			wrapper.WithBufferFlushTimeout(time.Millisecond),
			wrapper.WithMaxBufferSizeBeforeFlush(0)); err != nil {
			return err
		}
	} else if err := client.Init(o.username, o.password, o.Timeout,
		wrapper.WithDataBase(o.database),
		wrapper.WithBufferFlushTimeout(0),
		wrapper.WithMaxBufferSizeBeforeFlush(0)); err != nil {
		return err
	}
	if o.lastReconnectTrace.IsZero() || time.Since(o.lastReconnectTrace) >= time.Second {
		log.Warnf("ai-credits real verifier build=%s event=reconnect_init_complete epoch=%d new_ready=%t",
			realEnvVerifierBuildID, o.reconnectEpoch, client.Ready())
		o.lastReconnectTrace = time.Now()
	}
	// RedisClusterClient.Init intentionally returns nil when the hostcall
	// cannot create the connection, leaving Ready() false so normal callers
	// can retry later. A reconnect has a stronger ownership rule: never swap
	// the live client for an object that cannot dispatch commands. Otherwise
	// every verifier tick fails synchronously before reaching Redis, and the
	// next retry has no chance to initialize after the Pod returns.
	if !client.Ready() {
		return errors.New("redis outbox reconnect client is not ready")
	}
	o.Client = client
	log.Warnf("ai-credits real verifier build=%s event=reconnect_swap epoch=%d flushConfig=%dms/0 ready=%t",
		realEnvVerifierBuildID, o.reconnectEpoch, flushTimeout, o.Client.Ready())
	return nil
}

// outboxRecord writes one settlement to the durable channel before it is sent.
// The callback is part of the ordering contract: the HTTP settlement is only
// dispatched after Redis has acknowledged HSET. Calling HSet merely schedules
// a Redis callout; treating that return as persistence creates the exact loss
// window this outbox is meant to close.
//
// The return value says whether an outbox call was scheduled. A nil outbox
// keeps the documented log-only compatibility mode, so callers send directly.
func outboxRecord(config CreditsConfig, request SettleRequest, cause string, done func(error)) bool {
	return outboxRecordWithRaw(config, request, cause, nil, done)
}

func outboxRecordWithRaw(config CreditsConfig, request SettleRequest, cause string, raw []byte, done func(error)) bool {
	return outboxRecordWithRawLifecycle(config, request, cause, raw, done)
}

func outboxRecordWithRawLifecycle(config CreditsConfig, request SettleRequest, cause string, raw []byte, done func(error)) bool {
	if config.Outbox == nil || config.Outbox.Client == nil {
		return false
	}
	if request.ReservationID == "" {
		// Nothing was reserved, so there is no ledger row to settle and
		// nothing for a replay to resolve.
		return false
	}
	if len(raw) == 0 {
		raw = marshalJSON(OutboxEntry{
			ContractVersion:  CreditsContractVersion,
			EnqueuedAtUnixMs: time.Now().UnixMilli(),
			Cause:            cause,
			Settle:           json.RawMessage(marshalJSON(request)),
		})
	}
	if done == nil {
		done = func(error) {}
	}
	var callbackOnce sync.Once
	finish := func(err error) { callbackOnce.Do(func() { done(err) }) }
	verification := enqueueOutboxVerification(config.Outbox, request.ReservationID, raw, finish)
	callback := func(response resp.Value) {
		// The first HSET is only the enqueue. The barrier is deliberately not
		// dispatched or logged from this callback: it belongs to the HTTP
		// context and Envoy may destroy that context as soon as the stream ends.
		// The long-lived root tick dispatches the complete same-client HSET +
		// WAITAOF pair, including when this callback is dropped. The verifier
		// owns all error handling, so this callback only consumes the response.
		_ = response
		_ = verification
	}
	if err := config.Outbox.Client.HSet(config.Outbox.Key, request.ReservationID, string(raw), callback); err != nil {
		// A synchronous dispatch error leaves the verifier pending so its root
		// tick can retry after Redis reconnects. It is never converted into an
		// unprotected direct settlement. The root verifier records the branch on
		// its next tick, keeping host logging out of the HTTP callback path.
	}
	return true
}

// waitForAOF waits for one local append-only-file fsync. This is intentionally
// separate from HSET: a command acknowledgement means the Redis process
// accepted the write, while WAITAOF is the explicit persistence acknowledgement
// required before a gateway request is allowed to proceed to settlement.
func waitForAOF(client wrapper.RedisClient, timeout int64, done func(error)) error {
	if timeout <= 0 {
		timeout = 1000
	}
	return client.Command([]interface{}{"WAITAOF", 1, 0, fmt.Sprint(timeout)}, func(response resp.Value) {
		if err := response.Error(); err != nil {
			done(err)
			return
		}
		// Redis 7.2 replies with [local_fsync_count, replica_fsync_count].
		// Keep accepting a scalar integer for older test doubles, but never
		// interpret the array's textual rendering as an integer: that would
		// turn a valid Redis acknowledgement into a false write failure.
		localFsyncs := response.Integer()
		if values := response.Array(); len(values) > 0 {
			localFsyncs = values[0].Integer()
		}
		if localFsyncs < 1 {
			done(errors.New("redis did not confirm a local AOF fsync"))
			return
		}
		done(nil)
	})
}

// outboxForget drops the record once the backend has acknowledged the
// settlement.
//
// It is a best-effort shortcut, not the mechanism: this callback frequently
// never runs, because Envoy tears the stream context down before the settle
// call's answer arrives. A real Redis backend leaves the record for a
// value-aware replay pass because it does not receive the wrapper bytes.
// Compare-and-delete is required here because a retry may have replaced the
// same reservation field while this callback was in flight.
func outboxForget(config CreditsConfig, reservationID, raw string) {
	if config.Outbox == nil || config.Outbox.Client == nil || reservationID == "" {
		return
	}
	if raw == "" {
		log.Warnf("ai-credits settle outbox delete skipped reservation=%s: missing raw record", reservationID)
		return
	}
	if err := config.Outbox.Client.Eval(outboxForgetIfMatchScript, 1,
		[]interface{}{config.Outbox.Key}, []interface{}{reservationID, raw}, func(response resp.Value) {
			if response.Error() != nil {
				log.Warnf("ai-credits settle outbox compare-and-delete failed reservation=%s error=%v", reservationID, response.Error())
				return
			}
			if response.Integer() < 0 {
				// A newer record replaced this field. Leave it for replay rather than
				// deleting the evidence for that newer attempt.
				log.Warnf("ai-credits settle outbox record changed before delete reservation=%s", reservationID)
			}
		}); err != nil {
		// A record left behind is replayed once and answered `processed:false`.
		// That is noise, not a balance change, so it is logged and not retried.
		log.Warnf("ai-credits settle outbox compare-and-delete dispatch failed reservation=%s error=%v", reservationID, err)
	}
}
