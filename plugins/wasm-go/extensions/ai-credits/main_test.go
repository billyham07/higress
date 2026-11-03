package main

import (
	"encoding/json"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

func creditsConfig(mode string) json.RawMessage {
	data, _ := json.Marshal(map[string]interface{}{
		"mode":       mode,
		"serviceKey": "test-service-key",
		"service": map[string]interface{}{
			"name": "higress-ai-key-admin-v2.higress-system.svc.cluster.local",
			"port": 80,
		},
		"registry": map[string]interface{}{
			"version":         "unified-model.v1",
			"defaultGroupKey": "h20",
			"consumers": map[string][]string{
				"u-alice": {"h20/qwen3.5"},
			},
			"models": []map[string]interface{}{
				{
					"canonicalId":   "h20/qwen3.5",
					"groupKey":      "h20",
					"upstreamModel": "qwen3.5",
					"aliases":       []string{"qwen3.5"},
					"capabilities": map[string]bool{
						"openai.chat.completions": true,
						"openai.responses":        true,
					},
				},
			},
		},
	})
	return data
}

func TestParseConfigDefaults(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(creditsConfig(ModeOff))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		cfg, err := host.GetMatchConfig()
		require.NoError(t, err)
		parsed := cfg.(*CreditsConfig)
		require.Equal(t, ModeOff, parsed.Mode)
		require.Equal(t, DefaultAdmitPath, parsed.AdmitPath)
		require.Equal(t, DefaultSettlePath, parsed.SettlePath)
	})
}

func TestOffModeDoesNotCallAdmit(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(creditsConfig(ModeOff))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		action := host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "aigw.example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"x-mse-consumer", "u-alice"},
		})
		require.Equal(t, types.ActionContinue, action)
		require.Empty(t, host.GetHttpCalloutAttributes())
	})
}

func TestEnforceStripsForgedBudgetHeadersAndAdmits(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(creditsConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		action := host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "aigw.example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"content-type", "application/json"},
			{"x-mse-consumer", "u-alice"},
			{"x-ai-credits-budget-subject", "forged-user"},
			{"x-ai-credits-reservation-id", "forged-res"},
			{"X-AI-Credits-Service-Key", "stolen"},
		})
		require.Equal(t, types.HeaderStopIteration, action)
		headers := host.GetRequestHeaders()
		_, found := getHeader(headers, "x-ai-credits-budget-subject")
		require.False(t, found)
		_, found = getHeader(headers, "x-ai-credits-reservation-id")
		require.False(t, found)

		action = host.CallOnHttpRequestBody([]byte(`{"model":"qwen3.5","messages":[{"role":"user","content":"hi"}]}`))
		require.Equal(t, types.ActionPause, action)
		callouts := host.GetHttpCalloutAttributes()
		require.NotEmpty(t, callouts)
		require.Contains(t, string(callouts[0].Body), `"canonicalModel":"h20/qwen3.5"`)
		require.Contains(t, string(callouts[0].Body), `"consumer":"u-alice"`)
		require.NotContains(t, string(callouts[0].Body), "forged-user")

		host.CallOnHttpCall([][2]string{{":status", "200"}, {"content-type", "application/json"}},
			[]byte(`{"decision":"allow","reservationId":"res-1","canonicalModel":"h20/qwen3.5","maxOutputTokens":64,"strictCap":true}`))
		require.Equal(t, types.ActionContinue, host.GetHttpStreamAction())
		host.CompleteHttp()
	})
}

func TestEnforceDeniesWhenAdmitDenies(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(creditsConfig(ModeEnforce))
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
		resp := host.GetLocalResponse()
		require.NotNil(t, resp)
		require.Equal(t, uint32(402), resp.StatusCode)
		require.Contains(t, string(resp.Data), "insufficient_credits")
		host.CompleteHttp()
	})
}

func TestEnforceRejectsEmptyAdmitDecision(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(creditsConfig(ModeEnforce))
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
		host.CallOnHttpCall([][2]string{{":status", "200"}, {"content-type", "application/json"}},
			[]byte(`{"reservationId":"res-1"}`))
		resp := host.GetLocalResponse()
		require.NotNil(t, resp)
		require.Equal(t, uint32(503), resp.StatusCode)
		require.Contains(t, string(resp.Data), "credits_unavailable")
		host.CompleteHttp()
	})
}

func TestEnforceRejectsStringMicrosAdmit(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(creditsConfig(ModeEnforce))
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
		host.CallOnHttpCall([][2]string{{":status", "200"}, {"content-type", "application/json"}},
			[]byte(`{"decision":"allow","reservationId":"res-1","reservedCreditsMicros":"123456","maxBillableCreditsMicros":"1"}`))
		resp := host.GetLocalResponse()
		require.NotNil(t, resp)
		require.Equal(t, uint32(503), resp.StatusCode)
		require.Contains(t, string(resp.Data), "credits_unavailable")
		host.CompleteHttp()
	})
}

func TestMissingUsageSettlesPendingVerify(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(creditsConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "aigw.example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"content-type", "application/json"},
			{"x-mse-consumer", "u-alice"},
		})
		host.CallOnHttpRequestBody([]byte(`{"model":"qwen3.5","stream":true}`))
		host.CallOnHttpCall([][2]string{{":status", "200"}},
			[]byte(`{"decision":"allow","reservationId":"res-1"}`))
		host.CallOnHttpResponseHeaders([][2]string{{":status", "200"}})
		host.CallOnHttpStreamingResponseBody([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"), true)
		callouts := host.GetHttpCalloutAttributes()
		require.NotEmpty(t, callouts)
		require.Contains(t, string(callouts[len(callouts)-1].Body), `"status":"pending_verify"`)
		require.Contains(t, string(callouts[len(callouts)-1].Body), `"reason":"missing_usage"`)
		host.CompleteHttp()
	})
}

// OpenAI-compatible providers are allowed to send finish_reason and the
// usage-only chunk separately. The usage chunk is not enough by itself: a
// client can disconnect before [DONE]. A normal terminal marker then releases
// the exact usage for settlement.
func TestUsageChunkWaitsForTerminalBoundary(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(creditsConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "aigw.example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"content-type", "application/json"},
			{"x-mse-consumer", "u-alice"},
		})
		host.CallOnHttpRequestBody([]byte(`{"model":"qwen3.5","stream":true}`))
		host.CallOnHttpCall([][2]string{{":status", "200"}},
			[]byte(`{"decision":"allow","reservationId":"res-1"}`))
		host.CallOnHttpResponseHeaders([][2]string{{":status", "200"}})

		host.CallOnHttpStreamingResponseBody(
			[]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}\n\n`), false)
		require.Empty(t, host.GetHttpCalloutAttributes())
		host.CallOnHttpStreamingResponseBody(
			[]byte(`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}\n\n`), false)
		require.Empty(t, host.GetHttpCalloutAttributes())

		host.CallOnHttpStreamingResponseBody([]byte("data: [DONE]\n\n"), false)
		callouts := host.GetHttpCalloutAttributes()
		require.NotEmpty(t, callouts)
		require.Contains(t, string(callouts[len(callouts)-1].Body), `"status":"settled"`)
		require.Contains(t, string(callouts[len(callouts)-1].Body), `"outputTokens":20`)
		host.CompleteHttp()
	})
}

func TestResponsesSemanticTerminalSettlesNestedUsage(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(creditsConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "aigw.example.com"},
			{":path", "/v1/responses"},
			{":method", "POST"},
			{"content-type", "application/json"},
			{"x-mse-consumer", "u-alice"},
		})
		host.CallOnHttpRequestBody([]byte(`{"model":"qwen3.5","stream":true,"input":"hi"}`))
		host.CallOnHttpCall([][2]string{{":status", "200"}},
			[]byte(`{"decision":"allow","reservationId":"res-responses"}`))
		host.CallOnHttpResponseHeaders([][2]string{{":status", "200"}})
		host.CallOnHttpStreamingResponseBody([]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"ok"}

`), false)
		host.CallOnHttpStreamingResponseBody([]byte(`event: response.completed
data: {"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}

`), false)
		callouts := host.GetHttpCalloutAttributes()
		require.NotEmpty(t, callouts)
		require.Contains(t, string(callouts[len(callouts)-1].Body), `"status":"settled"`)
		require.Contains(t, string(callouts[len(callouts)-1].Body), `"inputTokens":3`)
		require.Contains(t, string(callouts[len(callouts)-1].Body), `"outputTokens":2`)
	})
}

func getHeader(headers [][2]string, key string) (string, bool) {
	for _, h := range headers {
		if h[0] == key {
			return h[1], true
		}
	}
	return "", false
}
