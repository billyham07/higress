package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

// aiLogKey is the property the charge has to land in. It is restated here
// rather than imported because the test reads the property directly, and
// because naming it is the point of the assertion.
//
// This test used to read "custom_log", which is where the wrapper's
// WriteUserAttributeToLog() puts things. Nothing reads that property: the
// gateway's access-log format carries one %FILTER_STATE(wasm.ai_log:PLAIN)%
// and no custom_log. So the test passed on every build while the field never
// once appeared in a real access log. It was measuring that a write
// happened, not that the charge was recorded.
const aiLogKey = "ai_log"

func TestTheChargeIsRecordedInTheAccessLog(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		cases := []struct {
			name     string
			extra    map[string]interface{}
			terminal []byte
			want     string
			absent   bool
		}{
			{
				name: "a priced request reports what it cost",
				extra: map[string]interface{}{
					"model_prices": map[string]interface{}{
						"glm-5.2": map[string]interface{}{"unit": "tokens", "per": 1000, "input_micros": 2000000, "output_micros": 8000000},
					},
				},
				terminal: []byte(`data: {"choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`),
				want:     "36000",
			},
			{
				// Free is a price somebody set. A statistics row showing 0 says
				// something an absent field does not.
				name: "a free request reports zero",
				extra: map[string]interface{}{
					"default_price": map[string]interface{}{"unit": "tokens", "per": 1},
				},
				terminal: []byte(`data: {"choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`),
				want:     "0",
			},
			{
				// Nothing was charged, so nothing is claimed. This must not
				// look like a request that cost zero.
				name: "an unmetered request reports nothing",
				extra: map[string]interface{}{
					"default_price": map[string]interface{}{"unit": "tokens", "per": 1, "input_micros": 1000000},
				},
				terminal: []byte("data: [DONE]\n\n"),
				absent:   true,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				host, status := test.NewTestHost(priceConfig(tc.extra))
				defer host.Reset()
				require.Equal(t, types.OnPluginStartStatusOK, status)

				// The emulator's property store outlives one host, so a
				// subtest asserting that nothing was written would otherwise
				// read the previous subtest's charge.
				require.NoError(t, host.SetProperty([]string{aiLogKey}, quoteForProperty([]byte("{}"))))

				host.CallOnHttpRequestHeaders([][2]string{
					{":authority", "example.com"},
					{":path", "/v1/chat/completions"},
					{":method", "POST"},
					{"x-mse-consumer", "consumer1"},
					{"x-higress-llm-model", "glm-5.2"},
				})
				admitFunded(host)
				host.CallOnHttpRequestBody([]byte(`{"stream":true}`))
				host.CallOnHttpStreamingResponseBody(tc.terminal, false)

				recorded, present := accessLogField(t, host, "credit_millis")
				if tc.absent {
					require.False(t, present, "expected no credit field, got %q", recorded)
					return
				}
				require.True(t, present, "expected a credit field in the access log")
				require.Equal(t, tc.want, recorded)
			})
		}
	})
}

func TestTheChargeDoesNotReplaceWhatOtherPluginsLogged(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(priceConfig(map[string]interface{}{
			"default_price": map[string]interface{}{"unit": "tokens", "per": 1000, "input_micros": 1000000, "output_micros": 1000000},
		}))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		// ai-statistics writes the token counts into this same object from its
		// own filter. The property is read-merge-write, and a plugin that
		// replaced it would silently delete the other plugin's fields.
		existing, err := json.Marshal(map[string]any{"input_token": 10000, "model": "glm-5.2"})
		require.NoError(t, err)
		require.NoError(t, host.SetProperty([]string{aiLogKey}, quoteForProperty(existing)))

		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"x-mse-consumer", "consumer1"},
			{"x-higress-llm-model", "glm-5.2"},
		})
		admitFunded(host)
		host.CallOnHttpRequestBody([]byte(`{"stream":true}`))
		host.CallOnHttpStreamingResponseBody(
			[]byte(`data: {"choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`), false)

		fields := accessLog(t, host)
		require.Equal(t, float64(10000), fields["input_token"], "the other plugin's token count must survive")
		require.Equal(t, "glm-5.2", fields["model"], "the other plugin's model must survive")
		require.Equal(t, float64(12000), fields["credit_millis"])
	})
}

// accessLog decodes the shared log object the plugins contribute to. The
// wrapper stores it as a quoted JSON string, so it is unquoted first.
func accessLog(t *testing.T, host test.TestHost) map[string]any {
	t.Helper()
	raw, err := host.GetProperty([]string{aiLogKey})
	require.NoError(t, err)
	if len(raw) == 0 {
		return map[string]any{}
	}
	var unquoted string
	require.NoError(t, json.Unmarshal([]byte(`"`+string(raw)+`"`), &unquoted))
	fields := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(unquoted), &fields))
	return fields
}

func accessLogField(t *testing.T, host test.TestHost, name string) (string, bool) {
	t.Helper()
	value, present := accessLog(t, host)[name]
	if !present {
		return "", false
	}
	if number, ok := value.(float64); ok {
		return strconv.FormatInt(int64(number), 10), true
	}
	return strings.TrimSpace(value.(string)), true
}

// quoteForProperty renders a JSON object the way the wrapper stores it: as an
// escaped string, not as raw JSON.
func quoteForProperty(document []byte) []byte {
	quoted, _ := json.Marshal(string(document))
	return quoted[1 : len(quoted)-1]
}
