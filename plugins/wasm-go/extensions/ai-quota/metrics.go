package main

import (
	"fmt"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

// The credit charge is exported as a gateway counter as well as written to
// the access log.
//
// The two answer different questions and neither replaces the other. The log
// line says what one request cost and is the only place a single charge can
// be looked up; the counter is what a usage dashboard can sum over a month
// without reading a million log lines. Deriving the dashboard from the log
// would mean scanning the log store for every panel, and deriving a single
// request's cost from the counter is not possible at all.
const creditsMetric = "credits"

// The metric name is built exactly the way ai-statistics builds its token
// counters, because Envoy extracts the four labels positionally from this
// shape: route.<route>.upstream.<cluster>.model.<model>.consumer.<consumer>.
// metric.<name> becomes route_upstream_model_consumer_metric_<name> with
// ai_route, ai_cluster, ai_model and ai_consumer. A different shape would
// still export, but with no labels at all.
func creditsMetricName(route, cluster, model, consumer string) string {
	return fmt.Sprintf("route.%s.upstream.%s.model.%s.consumer.%s.metric.%s",
		route, cluster, model, consumer, creditsMetric)
}

// routeAndCluster reads the two properties that identify where a request
// went. They are read at request time and carried on the context: the
// response path is a different filter callback, and a property read there
// can return nothing.
func routeAndCluster() (string, string) {
	return property("route_name"), property("cluster_name")
}

// property returns "-" for an unreadable property, which is what
// ai-statistics records, so a call that lands in the unknown bucket lands in
// the SAME unknown bucket on both counters instead of in two different ones.
func property(name string) string {
	raw, err := proxywasm.GetProperty([]string{name})
	if err != nil || len(raw) == 0 {
		return "-"
	}
	return string(raw)
}

// reportCreditsMetric increments the per-(route, cluster, model, consumer)
// credit counter.
//
// A zero charge is not recorded. Adding zero to a counter changes nothing,
// and defining the series would only add cardinality for a model that, by
// the operator's own price, costs nothing.
func reportCreditsMetric(ctx wrapper.HttpContext, config QuotaConfig, consumer string, amount int64) {
	if amount <= 0 || config.counters == nil {
		return
	}
	name := creditsMetricName(
		ctx.GetStringContext(quotaRouteContextKey, "-"),
		ctx.GetStringContext(quotaClusterContextKey, "-"),
		metricModel(ctx),
		consumer,
	)
	counter, ok := config.counters[name]
	if !ok {
		counter = proxywasm.DefineCounterMetric(name)
		config.counters[name] = counter
	}
	counter.Increment(uint64(amount))
}

// metricModel is the model name this charge is filed under.
//
// It is deliberately the model the RESPONSE reported, not the one the request
// asked for, even though the price is chosen by the requested model. The two
// are almost always the same, and where they differ the dashboard is what
// suffers: it joins credits to tokens by ai_model, so a charge filed under a
// name ai-statistics never used would appear as a model with a cost and no
// traffic. Pricing keeps using the requested model, because that is the name
// the price table is keyed by.
func metricModel(ctx wrapper.HttpContext) string {
	if model := ctx.GetStringContext(quotaUsageModelContextKey, ""); model != "" {
		return model
	}
	if model := ctx.GetStringContext(quotaModelContextKey, ""); model != "" {
		return model
	}
	return "-"
}
