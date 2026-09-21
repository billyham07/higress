package main

import (
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

// perRequestConfig prices a route by the call rather than by the token, which
// is the only shape that works for an endpoint with no token concept --
// transcription, speech, OCR.
func perRequestConfig() []byte {
	return priceConfig(map[string]interface{}{
		"default_price": map[string]interface{}{
			"unit": "requests", "per": 1, "request_micros": 2000000,
		},
		"enable_path_suffixes": []string{"/v1/audio/transcriptions"},
	})
}

func TestAPerRequestPriceChargesASuccessfulCall(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(perRequestConfig())
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetProperty([]string{aiLogKey}, quoteForProperty([]byte("{}"))))

		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "example.com"},
			{":path", "/v1/audio/transcriptions"},
			{":method", "POST"},
			{"x-mse-consumer", "consumer1"},
		})
		host.CallOnRedisCall(0, test.CreateRedisResp(1000000))
		host.CallOnHttpRequestBody([]byte(`{}`))
		host.CallOnHttpResponseHeaders([][2]string{{":status", "200"}})
		// No usage anywhere: that is the point of a per-request price.
		host.CallOnHttpStreamingResponseBody([]byte(`{"text":"hello"}`), true)

		recorded, present := accessLogField(t, host, "credit_millis")
		require.True(t, present, "a served call must be charged")
		require.Equal(t, "2000", recorded)
	})
}

func TestAPerRequestPriceDoesNotChargeAFailedCall(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		for _, status := range []string{"429", "500", "502"} {
			t.Run(status, func(t *testing.T) {
				host, startStatus := test.NewTestHost(perRequestConfig())
				defer host.Reset()
				require.Equal(t, types.OnPluginStartStatusOK, startStatus)
				require.NoError(t, host.SetProperty([]string{aiLogKey}, quoteForProperty([]byte("{}"))))

				host.CallOnHttpRequestHeaders([][2]string{
					{":authority", "example.com"},
					{":path", "/v1/audio/transcriptions"},
					{":method", "POST"},
					{"x-mse-consumer", "consumer1"},
				})
				host.CallOnRedisCall(0, test.CreateRedisResp(1000000))
				host.CallOnHttpRequestBody([]byte(`{}`))
				host.CallOnHttpResponseHeaders([][2]string{{":status", status}})
				host.CallOnHttpStreamingResponseBody([]byte(`{"error":{"message":"upstream failed"}}`), true)

				// A per-request price bills the attempt. Charging the caller
				// who received the error is the defect this guards.
				_, present := accessLogField(t, host, "credit_millis")
				require.False(t, present, "a %s must not be charged", status)
			})
		}
	})
}

func TestATokenPriceIsUnaffectedByTheStatusGate(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(priceConfig(map[string]interface{}{
			"default_price": map[string]interface{}{
				"unit": "tokens", "per": 1000,
				"input_micros": 1000000, "output_micros": 4000000,
			},
		}))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetProperty([]string{aiLogKey}, quoteForProperty([]byte("{}"))))

		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"x-mse-consumer", "consumer1"},
			{"x-higress-llm-model", "glm-5.2"},
		})
		host.CallOnRedisCall(0, test.CreateRedisResp(1000000))
		host.CallOnHttpRequestBody([]byte(`{"stream":true}`))
		// The upstream answered, generated real tokens, then the stream broke.
		// The tokens were produced; refunding them because the connection died
		// afterwards would be the wrong correction.
		host.CallOnHttpResponseHeaders([][2]string{{":status", "500"}})
		host.CallOnHttpStreamingResponseBody(
			[]byte(`data: {"choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`), false)

		recorded, present := accessLogField(t, host, "credit_millis")
		require.True(t, present, "consumed tokens must still be charged")
		require.Equal(t, "18000", recorded)
	})
}
