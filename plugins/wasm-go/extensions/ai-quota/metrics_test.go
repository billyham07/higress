package main

import (
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

func TestTheChargeIsExportedAsAGatewayCounter(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(priceConfig(map[string]interface{}{
			"model_prices": map[string]interface{}{
				"glm-5.2": map[string]interface{}{
					"unit": "tokens", "per": 1000,
					"input_micros": 2000000, "output_micros": 8000000,
				},
			},
		}))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetRouteName("ai-route-bailian.internal"))
		require.NoError(t, host.SetClusterName("outbound|443||bailian.dns"))

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
			[]byte(`data: {"model":"glm-5.2","choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`), false)

		// The name's shape is load-bearing: Envoy pulls ai_route, ai_cluster,
		// ai_model and ai_consumer out of it positionally, so a counter built
		// any other way exports with no labels and cannot be grouped or
		// joined against the token counters at all.
		value, err := host.GetCounterMetric(
			"route.ai-route-bailian.internal.upstream.outbound|443||bailian.dns.model.glm-5.2.consumer.consumer1.metric.credit_millis")
		require.NoError(t, err)
		require.Equal(t, uint64(36000), value)
	})
}

func TestAFreeRequestDefinesNoCounter(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(priceConfig(map[string]interface{}{
			"default_price": map[string]interface{}{"unit": "tokens", "per": 1},
		}))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetRouteName("ai-route-h20.internal"))
		require.NoError(t, host.SetClusterName("outbound|8000||h20.dns"))

		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"x-mse-consumer", "consumer1"},
			{"x-higress-llm-model", "qwen3-vl-8b"},
		})
		admitFunded(host)
		host.CallOnHttpRequestBody([]byte(`{"stream":true}`))
		host.CallOnHttpStreamingResponseBody(
			[]byte(`data: {"model":"qwen3-vl-8b","choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`), false)

		// Adding zero to a counter changes nothing, and defining the series
		// would add cardinality for a model that by the operator's own price
		// costs nothing. The access log still carries the explicit 0.
		_, err := host.GetCounterMetric(
			creditsMetricName("ai-route-h20.internal", "outbound|8000||h20.dns", "qwen3-vl-8b", "consumer1"))
		require.Error(t, err)

		recorded, present := accessLogField(t, host, "credit_millis")
		require.True(t, present)
		require.Equal(t, "0", recorded)
	})
}

func TestTheCounterUsesTheModelTheResponseReported(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(priceConfig(map[string]interface{}{
			"default_price": map[string]interface{}{"unit": "tokens", "per": 1000, "input_micros": 1000000},
		}))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetRouteName("ai-route-qwen3.5-alias.internal"))
		require.NoError(t, host.SetClusterName("outbound|443||qwen.dns"))

		// The caller asked for an alias; the upstream answered under its real
		// name, which is the name ai-statistics files the token counters
		// under. Filing the charge under the alias instead would render in
		// the dashboard as a model with a cost and no traffic, next to a
		// model with traffic and no cost.
		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"x-mse-consumer", "consumer1"},
			{"x-higress-llm-model", "qwen3.5"},
		})
		admitFunded(host)
		host.CallOnHttpRequestBody([]byte(`{"stream":true}`))
		host.CallOnHttpStreamingResponseBody(
			[]byte(`data: {"model":"qwen3.5-max-2026-09-01","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":0,"total_tokens":1000}}`), false)

		value, err := host.GetCounterMetric(
			creditsMetricName("ai-route-qwen3.5-alias.internal", "outbound|443||qwen.dns", "qwen3.5-max-2026-09-01", "consumer1"))
		require.NoError(t, err)
		require.Equal(t, uint64(1000), value)
	})
}
