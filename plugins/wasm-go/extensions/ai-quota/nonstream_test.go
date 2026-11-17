package main

import (
	"strings"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

// A non-streaming body arrives in as many chunks as Envoy likes, and the usage
// object at its end cannot be parsed out of a fragment. The body is buffered and
// charged whole, exactly once, at the route's price.
func TestNonStreamingResponseIsChargedWhole(t *testing.T) {
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
		admitFunded(host)
		host.CallOnHttpResponseHeaders([][2]string{{":status", "200"}, {"content-type", "application/json"}})

		body := []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"a long answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`)
		usageAt := strings.Index(string(body), `"usage"`)
		cuts := []int{usageAt / 2, usageAt + 12, len(body)}
		previous := 0
		for i, cut := range cuts {
			endOfStream := i == len(cuts)-1
			action := host.CallOnHttpStreamingResponseBody(body[previous:cut], endOfStream)
			previous = cut
			if !endOfStream {
				require.Equal(t, types.ActionPause, action, "a partial body is held back")
				_, present := accessLogField(t, host, "credit_millis")
				require.False(t, present, "nothing is charged before the body is whole")
			}
		}
		require.Equal(t, body, host.GetResponseBody(), "the client gets the body unchanged")

		recorded, present := accessLogField(t, host, "credit_millis")
		require.True(t, present)
		require.Equal(t, "18000", recorded)
	})
}
