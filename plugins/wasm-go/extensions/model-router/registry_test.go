package main

import (
	"encoding/json"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func registryConfig() json.RawMessage {
	data, _ := json.Marshal(map[string]interface{}{
		"modelKey":      "model",
		"modelToHeader": "x-higress-llm-model",
		"enableOnPathSuffix": []string{
			"/v1/chat/completions",
			"/chat/completions",
		},
		"registry": map[string]interface{}{
			"version":         "unified-model.v1",
			"defaultGroupKey": "h20",
			"consumers": map[string][]string{
				"tester": {"h20/qwen3.5", "bailian/glm-5.2"},
			},
			"models": []map[string]interface{}{
				{
					"canonicalId":   "h20/qwen3.5",
					"groupKey":      "h20",
					"upstreamModel": "qwen3.5",
					"aliases":       []string{"qwen3.5"},
					"routeValue":    "qwen3.5",
					"capabilities":  map[string]bool{"openai.chat.completions": true},
				},
				{
					"canonicalId":   "bailian/glm-5.2",
					"groupKey":      "bailian",
					"upstreamModel": "glm-5.2",
					"aliases":       []string{"glm-5.2"},
					"routeValue":    "glm-5.2",
					"capabilities":  map[string]bool{"openai.chat.completions": true},
				},
			},
		},
	})
	return data
}

func TestPublishedRegistryFourCalls(t *testing.T) {
	cases := []struct {
		path, model, upstream, route string
	}{
		{"/v1/chat/completions", "h20/qwen3.5", "qwen3.5", "qwen3.5"},
		{"/v1/chat/completions", "qwen3.5", "qwen3.5", "qwen3.5"},
		// A non-default group is addressed by its own path prefix. The root path
		// serves the default group, so `bailian/...` is reached at /bailian/,
		// not at /v1/ -- see TestPublishedRegistryRefusesOtherGroupAtRootPath.
		{"/bailian/v1/chat/completions", "glm-5.2", "glm-5.2", "glm-5.2"},
	}
	test.RunTest(t, func(t *testing.T) {
		for _, tc := range cases {
			host, status := test.NewTestHost(registryConfig())
			require.Equal(t, types.OnPluginStartStatusOK, status)
			func() {
				defer host.Reset()
				host.CallOnHttpRequestHeaders([][2]string{
					{":authority", "aigw.example.com"},
					{":path", tc.path},
					{":method", "POST"},
					{"content-type", "application/json"},
					{"x-mse-consumer", "tester"},
					{"x-higress-llm-model", "forged"},
				})
				action := host.CallOnHttpRequestBody([]byte(`{"model":"` + tc.model + `","messages":[]}`))
				require.Equal(t, types.ActionContinue, action, tc.path+" "+tc.model)
				require.Equal(t, tc.upstream, gjson.GetBytes(host.GetRequestBody(), "model").String())
				headers := host.GetRequestHeaders()
				route, found := getHeader(headers, "x-higress-llm-model")
				require.True(t, found)
				require.Equal(t, tc.route, route)
			}()
		}
	})
}

func TestPublishedRegistryRejectsUnknownAndConflict(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(registryConfig())
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "aigw.example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"content-type", "application/json"},
			{"x-mse-consumer", "tester"},
		})
		host.CallOnHttpRequestBody([]byte(`{"model":"no-such"}`))
		resp := host.GetLocalResponse()
		require.NotNil(t, resp)
		require.Equal(t, uint32(404), resp.StatusCode)
		require.Contains(t, string(resp.Data), "unknown_model")
	})
}

// key-auth runs after this plugin in the AUTHN phase, so a request reaches
// model routing before anyone has been identified. Rejecting on the absent
// consumer would reject every request in that ordering; the model-level check
// belongs to whoever runs after authentication.
func TestPublishedRegistryRoutesBeforeAuthenticationRuns(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(registryConfig())
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "aigw.example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"content-type", "application/json"},
			// no x-mse-consumer: key-auth has not run yet
		})
		action := host.CallOnHttpRequestBody([]byte(`{"model":"h20/qwen3.5","messages":[]}`))
		require.Equal(t, types.ActionContinue, action)
		require.Nil(t, host.GetLocalResponse(), "an unauthenticated request must still be routed")
		route, found := getHeader(host.GetRequestHeaders(), "x-higress-llm-model")
		require.True(t, found)
		require.Equal(t, "qwen3.5", route)
	})
}

// A caller some earlier filter did identify is still held to the published
// entitlement, so the check is not simply dropped.
func TestPublishedRegistryStillRefusesAnIdentifiedCallerWithoutTheModel(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(registryConfig())
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "aigw.example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"content-type", "application/json"},
			{"x-mse-consumer", "stranger"},
		})
		host.CallOnHttpRequestBody([]byte(`{"model":"h20/qwen3.5","messages":[]}`))
		resp := host.GetLocalResponse()
		require.NotNil(t, resp, "an identified caller outside the entitlement must be refused")
		require.Equal(t, uint32(403), resp.StatusCode)
	})
}

// Naming another group's model at the root path used to resolve, emit that
// group's routeValue, and let Envoy pick whichever route matched it. Where two
// groups share an Ingress predicate that is the default group's route, so the
// caller silently received a different model and was billed for it.
func TestPublishedRegistryRefusesOtherGroupAtRootPath(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(registryConfig())
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "aigw.example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"content-type", "application/json"},
			{"x-mse-consumer", "tester"},
		})
		host.CallOnHttpRequestBody([]byte(`{"model":"bailian/glm-5.2","messages":[]}`))
		resp := host.GetLocalResponse()
		require.NotNil(t, resp, "another group's model must not be served from the root path")
		require.Equal(t, uint32(404), resp.StatusCode)
		require.Contains(t, string(resp.Data), "bailian")
	})
}
