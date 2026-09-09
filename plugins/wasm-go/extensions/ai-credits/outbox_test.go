package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/proxytest"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/resp"
)

// verifierRedisClient is intentionally tiny: embedding the production
// interface keeps the test focused on the HSET/WAITAOF state machine while
// overriding only the calls the verifier makes. It lets the test distinguish
// a synchronous host dispatch error from an asynchronous callback error
// without relying on a real Redis process.
type verifierRedisClient struct {
	wrapper.RedisClient
	ready            bool
	hsetErr          error
	commandErr       error
	hsetCallbacks    []wrapper.RedisResponseCallback
	commandCallbacks []wrapper.RedisResponseCallback
	calls            []string
}

func (c *verifierRedisClient) Ready() bool { return c.ready }

func (c *verifierRedisClient) HSet(_ string, _ string, _ interface{}, callback wrapper.RedisResponseCallback) error {
	c.calls = append(c.calls, "hset")
	if c.hsetErr != nil {
		return c.hsetErr
	}
	c.hsetCallbacks = append(c.hsetCallbacks, callback)
	return nil
}

func (c *verifierRedisClient) Command(cmds []interface{}, callback wrapper.RedisResponseCallback) error {
	command := "command"
	if len(cmds) > 0 {
		if name, ok := cmds[0].(string); ok {
			command = strings.ToLower(name)
		}
	}
	c.calls = append(c.calls, command)
	if c.commandErr != nil {
		return c.commandErr
	}
	c.commandCallbacks = append(c.commandCallbacks, callback)
	return nil
}

func outboxConfig(mode string) json.RawMessage {
	var parsed map[string]interface{}
	_ = json.Unmarshal(creditsConfig(mode), &parsed)
	parsed["settlementOutbox"] = map[string]interface{}{
		"service_name": "redis.static",
		"service_port": 6379,
		"timeout":      1000,
		"database":     0,
		"key":          DefaultOutboxKey,
	}
	data, _ := json.Marshal(parsed)
	return data
}

// admitStreamingRequest drives one admitted request up to the point where the
// response body starts arriving.
func admitStreamingRequest(t *testing.T, host test.TestHost) {
	t.Helper()
	host.CallOnHttpRequestHeaders([][2]string{
		{":authority", "aigw.example.com"},
		{":path", "/v1/chat/completions"},
		{":method", "POST"},
		{"content-type", "application/json"},
		{"x-mse-consumer", "u-alice"},
	})
	host.CallOnHttpRequestBody([]byte(`{"model":"qwen3.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	host.CallOnHttpCall([][2]string{{":status", "200"}},
		[]byte(`{"decision":"allow","reservationId":"res-1","canonicalModel":"h20/qwen3.5"}`))
	host.CallOnHttpResponseHeaders([][2]string{{":status", "200"}})
}

// redisQueries returns every Redis command this VM has issued, oldest first.
//
// proxytest keeps callouts per context and hands out context IDs from a
// process-global counter, so the sweep starts at zero rather than at the
// current request's id.
func redisQueries(host test.TestHost) []string {
	queries := []string{}
	for contextID := uint32(0); contextID < 1<<16; contextID++ {
		for _, callout := range host.GetRedisCalloutAttributesFromContext(contextID) {
			queries = append(queries, string(callout.Query))
		}
	}
	return queries
}

func redisCallouts(host test.TestHost) []proxytest.RedisCalloutAttribute {
	callouts := []proxytest.RedisCalloutAttribute{}
	for contextID := uint32(0); contextID < 1<<16; contextID++ {
		callouts = append(callouts, host.GetRedisCalloutAttributesFromContext(contextID)...)
	}
	return callouts
}

func latestRedisCallout(t *testing.T, host test.TestHost, needle string) proxytest.RedisCalloutAttribute {
	t.Helper()
	needle = strings.ToLower(needle)
	var found proxytest.RedisCalloutAttribute
	foundOK := false
	for _, callout := range redisCallouts(host) {
		if strings.Contains(strings.ToLower(string(callout.Query)), needle) &&
			(!foundOK || callout.CalloutID > found.CalloutID) {
			found = callout
			foundOK = true
		}
	}
	require.True(t, foundOK, "missing Redis callout containing %q", needle)
	return found
}

func tickUntilRedisCallout(host test.TestHost, needle string) (proxytest.RedisCalloutAttribute, bool) {
	needle = strings.ToLower(needle)
	for attempt := 0; attempt < 10; attempt++ {
		var found proxytest.RedisCalloutAttribute
		foundOK := false
		for _, callout := range redisCallouts(host) {
			if strings.Contains(strings.ToLower(string(callout.Query)), needle) &&
				(!foundOK || callout.CalloutID > found.CalloutID) {
				found = callout
				foundOK = true
			}
		}
		if foundOK {
			return found, true
		}
		// The wrapper's root scheduler is configured for a 100 ms period.
		// Sleeping between explicit test ticks exercises that contract in both
		// Go and compiled-WASM hosts instead of assuming a manual Tick bypasses
		// the scheduler's period gate.
		time.Sleep(110 * time.Millisecond)
		host.Tick()
	}
	return proxytest.RedisCalloutAttribute{}, false
}

func httpCallouts(host test.TestHost) []proxytest.HttpCalloutAttribute {
	callouts := []proxytest.HttpCalloutAttribute{}
	for contextID := uint32(0); contextID < 1<<16; contextID++ {
		callouts = append(callouts, host.GetCalloutAttributesFromContext(contextID)...)
	}
	return callouts
}

func latestSettleCallout(t *testing.T, host test.TestHost) proxytest.HttpCalloutAttribute {
	t.Helper()
	var found proxytest.HttpCalloutAttribute
	foundOK := false
	for _, callout := range httpCallouts(host) {
		for _, header := range callout.Headers {
			if strings.EqualFold(header[0], ":path") && strings.HasSuffix(header[1], "/internal/ai-credits/v1/settle") {
				if !foundOK || callout.CalloutID > found.CalloutID {
					found = callout
					foundOK = true
				}
			}
		}
	}
	require.True(t, foundOK, "missing settlement HTTP callout")
	return found
}

func settleCallout(t *testing.T, host test.TestHost) (uint32, string) {
	t.Helper()
	callout := latestSettleCallout(t, host)
	return callout.CalloutID, string(callout.Body)
}

func redisHSetValue(t *testing.T, query string) string {
	t.Helper()
	value, _, err := resp.NewReader(bytes.NewReader([]byte(query))).ReadValue()
	require.NoError(t, err)
	parts := value.Array()
	require.Len(t, parts, 4)
	return parts[3].String()
}

func testRespValue(t *testing.T, encoded []byte) resp.Value {
	t.Helper()
	value, _, err := resp.NewReader(bytes.NewReader(encoded)).ReadValue()
	require.NoError(t, err)
	return value
}

// The settlement is durable before it is sent, and stops being durable only
// when the ledger says it has it.
func TestSettlementIsRecordedBeforeDispatchAndDroppedOnAck(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(outboxConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		admitStreamingRequest(t, host)

		host.CallOnHttpStreamingResponseBody(
			[]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}\n\n"), true)
		// The record exists before the answer, which is the whole point: a
		// worker killed here still leaves the usage somewhere the backend can
		// read it.
		recorded := redisQueries(host)
		require.Len(t, recorded, 1)
		require.Contains(t, recorded[0], "hset")
		require.Contains(t, recorded[0], DefaultOutboxKey)
		require.Contains(t, recorded[0], "res-1")
		require.Contains(t, recorded[0], `"status":"settled"`)
		require.Contains(t, recorded[0], `"outputTokens":20`)
		require.Contains(t, recorded[0], "\"cause\":\""+OutboxCauseDispatch+"\"")
		// The HSET call has only been submitted. No usage-bearing HTTP callout
		// may be created before Redis acknowledges HSET and then WAITAOF.
		require.Empty(t, host.GetHttpCalloutAttributes())

		// The first HSET acknowledgement only lets the root verifier dispatch
		// the exact-write barrier pair. The settlement callout must not exist
		// until the barrier HSET and its connection-local WAITAOF both acknowledge.
		initialHSet := latestRedisCallout(t, host, "hset")
		// RedisInit carries the flush options in the hostcall cluster identity.
		// Dispatch must use that same qualified identity; otherwise a reconnect
		// that changes the options silently keeps using the stale base client.
		require.Contains(t, initialHSet.Upstream, "buffer_flush_timeout=0")
		require.Contains(t, initialHSet.Upstream, "max_buffer_size_before_flush=0")
		host.CallOnRedisCallResponse(initialHSet.CalloutID, 0, test.CreateRedisRespString("OK"))
		host.Tick()
		require.Len(t, redisQueries(host), 2)
		require.Contains(t, strings.ToLower(strings.Join(redisQueries(host), "\n")), "waitaof")
		barrierHSet := latestRedisCallout(t, host, "hset")
		host.CallOnRedisCallResponse(barrierHSet.CalloutID, 0, test.CreateRedisRespString("OK"))
		// Neither HSET acknowledgement is the persistence boundary. The HTTP
		// settlement starts only after Redis confirms a local AOF fsync for the
		// immediately preceding barrier write on that same connection.
		barrierWait := latestRedisCallout(t, host, "waitaof")
		host.CallOnRedisCallResponse(barrierWait.CalloutID, 0, test.CreateRedisRespArray([]interface{}{1, 0}))
		host.Tick()
		calloutID, body := settleCallout(t, host)
		require.Contains(t, body, `"reservationId":"res-1"`)
		host.CallOnHttpCallResponse(calloutID, [][2]string{{":status", "200"}}, nil,
			[]byte(`{"reservationId":"res-1","processed":true,"creditsMicros":120}`))

		acknowledged := redisQueries(host)
		require.Len(t, acknowledged, 1)
		require.Contains(t, strings.ToLower(acknowledged[0]), "hdel")
		require.Contains(t, acknowledged[0], "res-1")
		host.CompleteHttp()

		// Stream done adds nothing: this settlement was dispatched, and its
		// record was already resolved by the acknowledgement.
		require.Len(t, redisQueries(host), 1)
	})
}

// A settlement the backend refuses stays in the outbox for the replay job.
func TestRefusedSettlementKeepsItsOutboxRecord(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(outboxConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		admitStreamingRequest(t, host)

		host.CallOnHttpStreamingResponseBody(
			[]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}\n\n"), true)
		initialHSet := latestRedisCallout(t, host, "hset")
		host.CallOnRedisCallResponse(initialHSet.CalloutID, 0, test.CreateRedisRespString("OK"))
		host.Tick()
		barrierHSet := latestRedisCallout(t, host, "hset")
		host.CallOnRedisCallResponse(barrierHSet.CalloutID, 0, test.CreateRedisRespString("OK"))
		barrierWait := latestRedisCallout(t, host, "waitaof")
		host.CallOnRedisCallResponse(barrierWait.CalloutID, 0, test.CreateRedisRespArray([]interface{}{1, 0}))
		host.Tick()
		calloutID, _ := settleCallout(t, host)
		host.CallOnHttpCallResponse(calloutID, [][2]string{{":status", "400"}}, nil,
			[]byte(`{"error":{"code":"invalid_request","message":"nope"}}`))
		host.CompleteHttp()

		queries := redisQueries(host)
		require.Empty(t, queries)
		for _, query := range queries {
			require.NotContains(t, query, "hdel")
		}
	})
}

// A Redis response error is different from a DispatchRedisCall submission
// error: it proves the durable write was not accepted. The plugin leaves the
// verifier pending for a root retry of the exact barrier and never claims that
// HSET was durable.
func TestOutboxWriteErrorDoesNotPretendToBeDurable(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(outboxConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		admitStreamingRequest(t, host)
		host.CallOnHttpStreamingResponseBody(
			[]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}\n\n"), true)
		initialHSet := latestRedisCallout(t, host, "hset")
		host.CallOnRedisCallResponse(initialHSet.CalloutID, 0, test.CreateRedisRespError("LOADING redis is not ready"))
		require.Empty(t, host.GetHttpCalloutAttributes())
		// The root verifier retries the exact HSET -> WAITAOF pair after the
		// connection error; no unprotected direct settlement is allowed.
		host.Tick()
		barrierHSet := latestRedisCallout(t, host, "hset")
		host.CallOnRedisCallResponse(barrierHSet.CalloutID, 0,
			test.CreateRedisRespString("OK"))
		barrierWait := latestRedisCallout(t, host, "waitaof")
		host.CallOnRedisCallResponse(barrierWait.CalloutID, 0,
			test.CreateRedisRespArray([]interface{}{1, 0}))
		host.Tick()
		callout := latestSettleCallout(t, host)
		require.Contains(t, string(callout.Body), `"reservationId":"res-1"`)
		host.CallOnHttpCallResponse(callout.CalloutID, [][2]string{{":status", "200"}}, nil,
			[]byte(`{"reservationId":"res-1","processed":true}`))
		host.CompleteHttp()
		forget := latestRedisCallout(t, host, "eval")
		host.CallOnRedisCallResponse(forget.CalloutID, 0, test.CreateRedisRespInt(1))
		require.Empty(t, redisQueries(host))
	})
}

// A Redis command acknowledgement is not enough when the local fsync wait
// fails. The exact HSET remains available for replay, and the complete barrier
// pair is retried; no code treats the failed WAITAOF as durable delivery.
func TestOutboxAOFWaitErrorKeepsTheEvidenceForReplay(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(outboxConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		admitStreamingRequest(t, host)
		host.CallOnHttpStreamingResponseBody(
			[]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}\n\n"), true)
		hsetQueries := redisQueries(host)
		require.Len(t, hsetQueries, 1)
		require.Contains(t, strings.ToLower(hsetQueries[0]), "hset")
		initialHSet := latestRedisCallout(t, host, "hset")
		host.CallOnRedisCallResponse(initialHSet.CalloutID, 0, test.CreateRedisRespString("OK"))
		host.Tick()
		waitQueries := redisQueries(host)
		require.Len(t, waitQueries, 2)
		require.Contains(t, strings.ToLower(waitQueries[0]), "hset")
		require.Contains(t, strings.ToLower(waitQueries[1]), "waitaof")
		barrierHSet := latestRedisCallout(t, host, "hset")
		host.CallOnRedisCallResponse(barrierHSet.CalloutID, 0, test.CreateRedisRespString("OK"))
		barrierWait := latestRedisCallout(t, host, "waitaof")
		host.CallOnRedisCallResponse(barrierWait.CalloutID, 0, test.CreateRedisRespError("ERR WAITAOF is unavailable"))
		require.Empty(t, host.GetHttpCalloutAttributes())
		// A failed wait leaves the exact outbox record and retries the complete
		// pair. Only this second pair can release settlement.
		// The production tick period is 100 ms; allow the wrapper's scheduler
		// to advance instead of turning this test into a same-millisecond
		// implementation assumption.
		retryHSet, found := tickUntilRedisCallout(host, "hset")
		require.True(t, found, "missing Redis HSET callout after retry")
		host.CallOnRedisCallResponse(retryHSet.CalloutID, 0,
			test.CreateRedisRespString("OK"))
		retryWait := latestRedisCallout(t, host, "waitaof")
		host.CallOnRedisCallResponse(retryWait.CalloutID, 0,
			test.CreateRedisRespArray([]interface{}{1, 0}))
		host.Tick()
		callout := latestSettleCallout(t, host)
		host.CallOnHttpCallResponse(callout.CalloutID, [][2]string{{":status", "400"}}, nil,
			[]byte(`{"error":{"code":"invalid_request","message":"retry later"}}`))
		for _, query := range redisQueries(host) {
			require.NotContains(t, strings.ToLower(query), "hdel")
		}
		host.CompleteHttp()
	})
}

// When Envoy destroys the HTTP context before Redis answers, the root
// PluginContext still issues the exact-write barrier pair and then continues
// the settlement from the root context. This is deliberately exercised
// without delivering the initial HSET callback: the fallback must not depend
// on that callback or on the HTTP stream remaining alive.
func TestRootVerifierFinishesAfterHttpContextTeardown(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(outboxConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		admitStreamingRequest(t, host)

		host.CallOnHttpStreamingResponseBody(
			[]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}\n\n"), true)
		queries := redisQueries(host)
		require.Len(t, queries, 1)

		// Tear down the HTTP request before the HSET callback. The root tick
		// should issue a complete exact-write barrier pair from the long-lived
		// PluginContext.
		host.CompleteHttp()
		host.Tick()
		queries = redisQueries(host)
		require.Len(t, queries, 3)
		require.Contains(t, strings.ToLower(string(latestRedisCallout(t, host, "hset").Query)), "hset")
		require.Contains(t, strings.ToLower(string(latestRedisCallout(t, host, "waitaof").Query)), "waitaof")
		barrierHSet := latestRedisCallout(t, host, "hset")
		host.CallOnRedisCallResponse(barrierHSet.CalloutID, 0, test.CreateRedisRespString("OK"))
		barrierWait := latestRedisCallout(t, host, "waitaof")
		host.CallOnRedisCallResponse(barrierWait.CalloutID, 0,
			test.CreateRedisRespArray([]interface{}{1, 0}))
		host.Tick()

		// The durable pair completed after the HTTP context was destroyed, so
		// settlement is now a root-context HTTP callout rather than a callback
		// tied to the dead stream.
		settle := latestSettleCallout(t, host)
		require.Contains(t, string(settle.Body), `"reservationId":"res-1"`)
		host.CallOnHttpCallResponse(settle.CalloutID, [][2]string{{":status", "200"}}, nil,
			[]byte(`{"reservationId":"res-1","processed":true}`))
		forget := latestRedisCallout(t, host, "eval")
		host.CallOnRedisCallResponse(forget.CalloutID, 0, test.CreateRedisRespInt(1))
	})
}

// A backend can be reachable through Envoy while its own Redis dependency is
// still recovering. Detached delivery must therefore retry on later root
// ticks instead of recursively exhausting all configured attempts in the first
// HTTP callback turn. The outbox is removed only after the later 2xx response.
func TestDetachedSettlementRetriesAfterTemporaryBackendFailure(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(outboxConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		admitStreamingRequest(t, host)

		host.CallOnHttpStreamingResponseBody(
			[]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}\n\n"), true)
		initialHSet := latestRedisCallout(t, host, "hset")
		host.CallOnRedisCallResponse(initialHSet.CalloutID, 0, test.CreateRedisRespString("OK"))
		host.Tick()
		barrierHSet := latestRedisCallout(t, host, "hset")
		host.CallOnRedisCallResponse(barrierHSet.CalloutID, 0, test.CreateRedisRespString("OK"))
		barrierWait := latestRedisCallout(t, host, "waitaof")
		host.CallOnRedisCallResponse(barrierWait.CalloutID, 0,
			test.CreateRedisRespArray([]interface{}{1, 0}))
		host.Tick()

		first := latestSettleCallout(t, host)
		settlesBefore := 0
		for _, callout := range httpCallouts(host) {
			for _, header := range callout.Headers {
				if strings.EqualFold(header[0], ":path") && strings.HasSuffix(header[1], "/internal/ai-credits/v1/settle") {
					settlesBefore++
					break
				}
			}
		}
		host.CallOnHttpCallResponse(first.CalloutID, [][2]string{{":status", "503"}}, nil,
			[]byte(`{"error":"temporarily unavailable"}`))
		settlesAfterFailure := 0
		for _, callout := range httpCallouts(host) {
			for _, header := range callout.Headers {
				if strings.EqualFold(header[0], ":path") && strings.HasSuffix(header[1], "/internal/ai-credits/v1/settle") {
					settlesAfterFailure++
					break
				}
			}
		}
		require.Equal(t, settlesBefore, settlesAfterFailure,
			"temporary detached failure must not recurse in the response callback")

		var second proxytest.HttpCalloutAttribute
		found := false
		for attempt := 0; attempt < 10 && !found; attempt++ {
			time.Sleep(110 * time.Millisecond)
			host.Tick()
			for _, callout := range httpCallouts(host) {
				if callout.CalloutID <= first.CalloutID {
					continue
				}
				for _, header := range callout.Headers {
					if strings.EqualFold(header[0], ":path") && strings.HasSuffix(header[1], "/internal/ai-credits/v1/settle") {
						second = callout
						found = true
						break
					}
				}
				if found {
					break
				}
			}
		}
		require.True(t, found, "missing detached retry on a later root tick")
		host.CallOnHttpCallResponse(second.CalloutID, [][2]string{{":status", "200"}}, nil,
			[]byte(`{"reservationId":"res-1","processed":true}`))
		forget := latestRedisCallout(t, host, "eval")
		host.CallOnRedisCallResponse(forget.CalloutID, 0, test.CreateRedisRespInt(1))
	})
}

// A client that hangs up mid-stream never reaches a terminal chunk. Before the
// stream-done hook that request produced no settlement at all; now it produces
// a durable pending_verify carrying whatever usage did arrive.
func TestDisconnectBeforeAnyTerminalChunkIsRecorded(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(outboxConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		admitStreamingRequest(t, host)

		host.CallOnHttpStreamingResponseBody([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n"), false)
		require.Empty(t, redisQueries(host))

		host.CompleteHttp()

		queries := redisQueries(host)
		require.Len(t, queries, 1)
		require.Contains(t, queries[0], "hset")
		require.Contains(t, queries[0], "res-1")
		require.Contains(t, queries[0], `"status":"`+SettlePendingVerify+`"`)
		require.Contains(t, queries[0], `"reason":"`+ReasonDisconnect+`"`)
		require.Contains(t, queries[0], "\"cause\":\""+OutboxCauseDisconnect+"\"")
	})
}

// A stream that has emitted a usage-only chunk but has not emitted its
// terminal marker is still vulnerable to client disconnect. Keep the exact
// usage quarantined as pending_verify instead of treating the chunk as a
// completed bill.
func TestDisconnectAfterUsageBeforeTerminalMarkerStaysPendingVerify(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(outboxConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		admitStreamingRequest(t, host)
		host.CallOnHttpStreamingResponseBody(
			[]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}\n\n`), false)
		host.CallOnHttpStreamingResponseBody(
			[]byte(`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}\n\n`), false)
		require.Empty(t, redisQueries(host))

		host.CompleteHttp()
		queries := redisQueries(host)
		require.Len(t, queries, 1)
		require.Contains(t, queries[0], `"status":"`+SettlePendingVerify+`"`)
		require.Contains(t, queries[0], `"reason":"`+ReasonDisconnect+`"`)
		require.Contains(t, queries[0], `"outputTokens":20`)
	})
}

// A request that was never admitted has no reservation, so there is nothing to
// settle and nothing to record.
func TestUnadmittedRequestRecordsNothing(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(outboxConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "aigw.example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"content-type", "application/json"},
			{"x-mse-consumer", "u-alice"},
		})
		host.CallOnHttpRequestBody([]byte(`{"model":"qwen3.5"}`))
		host.CallOnHttpCall([][2]string{{":status", "402"}},
			[]byte(`{"decision":"deny","error":{"code":"insufficient_credits","message":"no budget"}}`))
		host.CompleteHttp()
		require.Empty(t, redisQueries(host))
	})
}

// Without an outbox the plugin keeps its previous behaviour exactly: the log
// line is the only durable channel, and no Redis dependency is introduced.
func TestWithoutAnOutboxNothingIsWrittenToRedis(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(creditsConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		admitStreamingRequest(t, host)
		host.CallOnHttpStreamingResponseBody(
			[]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}\n\n"), true)
		_, body := settleCallout(t, host)
		require.Contains(t, body, `"status":"settled"`)
		host.CompleteHttp()
		require.Empty(t, redisQueries(host))
	})
}

func TestParseConfigRejectsAnOutboxWithoutAService(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		var parsed map[string]interface{}
		_ = json.Unmarshal(creditsConfig(ModeEnforce), &parsed)
		parsed["settlementOutbox"] = map[string]interface{}{"key": DefaultOutboxKey}
		data, _ := json.Marshal(parsed)
		_, status := test.NewTestHost(data)
		require.NotEqual(t, types.OnPluginStartStatusOK, status)
	})
}

func TestOutboxEntryCarriesTheFrozenContract(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		entry := OutboxEntry{
			ContractVersion:  CreditsContractVersion,
			EnqueuedAtUnixMs: 1757000000000,
			Cause:            OutboxCauseDispatch,
			Settle:           json.RawMessage(`{"reservationId":"res-1"}`),
		}
		encoded := string(marshalJSON(entry))
		require.True(t, strings.Contains(encoded, `"contractVersion":"ai-credits.v1"`))
		require.True(t, strings.Contains(encoded, `"settle":{"reservationId":"res-1"}`))
	})
}

func TestVerifierSyncDispatchErrorDefersReconnectToRootTick(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		resetOutboxVerifier()
		defer resetOutboxVerifier()
		oldReconnect := outboxReconnect
		oldLog := verifierLogWarnf
		defer func() {
			outboxReconnect = oldReconnect
			verifierLogWarnf = oldLog
		}()
		verifierLogWarnf = func(string, ...interface{}) {}

		reconnects := 0
		outboxReconnect = func(*SettlementOutbox) error {
			reconnects++
			return nil
		}
		client := &verifierRedisClient{
			ready:   true,
			hsetErr: errors.New("no Envoy Redis host"),
		}
		outbox := &SettlementOutbox{Client: client, Key: DefaultOutboxKey, Timeout: 1000}
		finished := false
		v := enqueueOutboxVerification(outbox, "sync-dispatch", []byte(`{"reservationId":"sync-dispatch"}`), func(err error) {
			require.NoError(t, err)
			finished = true
		})
		require.NotNil(t, v)

		dispatchOutboxBarrier(v)
		require.Equal(t, []string{"hset"}, client.calls)
		require.Zero(t, reconnects, "a synchronous dispatch failure must not reconnect inline")
		require.True(t, v.reconnectPending)
		require.False(t, v.barrierInFlight)

		// Tick one performs the deferred reconnect; the following tick is the
		// first one allowed to dispatch a fresh pair.
		processOutboxVerifications()
		require.Equal(t, 1, reconnects)
		require.Empty(t, client.hsetCallbacks)
		client.hsetErr = nil
		processOutboxVerifications()
		require.Equal(t, []string{"hset", "hset", "waitaof"}, client.calls)
		require.Len(t, client.hsetCallbacks, 1)
		require.Len(t, client.commandCallbacks, 1)
		require.False(t, finished)

		client.hsetCallbacks[0](testRespValue(t, test.CreateRedisRespString("OK")))
		client.commandCallbacks[0](testRespValue(t, test.CreateRedisRespArray([]interface{}{1, 0})))
		// Completion is delivered on the next root tick, after both callbacks
		// have only recorded state.
		processOutboxVerifications()
		require.True(t, finished)
	})
}

func TestVerifierAsyncCallbackErrorDefersReconnectToRootTick(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		resetOutboxVerifier()
		defer resetOutboxVerifier()
		oldReconnect := outboxReconnect
		oldLog := verifierLogWarnf
		defer func() {
			outboxReconnect = oldReconnect
			verifierLogWarnf = oldLog
		}()
		verifierLogWarnf = func(string, ...interface{}) {}

		reconnects := 0
		outboxReconnect = func(*SettlementOutbox) error {
			reconnects++
			return nil
		}
		client := &verifierRedisClient{ready: true}
		outbox := &SettlementOutbox{Client: client, Key: DefaultOutboxKey, Timeout: 1000}
		v := enqueueOutboxVerification(outbox, "async-callback", []byte(`{"reservationId":"async-callback"}`), nil)
		require.NotNil(t, v)

		processOutboxVerifications()
		require.Equal(t, []string{"hset", "waitaof"}, client.calls)
		client.hsetCallbacks[0](testRespValue(t, test.CreateRedisRespError("connection reset")))
		client.commandCallbacks[0](testRespValue(t, test.CreateRedisRespError("connection reset")))
		require.Zero(t, reconnects, "a Redis callback must only record state")
		require.True(t, v.reconnectPending)
		require.False(t, v.barrierInFlight)

		processOutboxVerifications()
		require.Equal(t, 1, reconnects, "the next root tick performs the reconnect")
	})
}
