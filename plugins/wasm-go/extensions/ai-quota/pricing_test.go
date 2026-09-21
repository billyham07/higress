package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

func priceConfig(extra map[string]interface{}) json.RawMessage {
	base := map[string]interface{}{
		"admin_consumer":       "admin",
		"redis_key_prefix":     "chat_quota:",
		"admin_path":           "/quota",
		"enable_path_suffixes": []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"},
		"redis": map[string]interface{}{
			"service_name": "redis.static",
			"service_port": 6379,
			"timeout":      1000,
			"database":     0,
		},
	}
	for key, value := range extra {
		base[key] = value
	}
	data, _ := json.Marshal(base)
	return data
}

// decrByArguments isolates what a DECRBY actually asked for, so a test cannot
// pass because the amount it wanted happened to appear elsewhere in the
// serialized command.
func decrByArguments(t *testing.T, query string) string {
	t.Helper()
	index := strings.Index(strings.ToLower(query), "decrby")
	require.GreaterOrEqual(t, index, 0, "expected a DECRBY, got %q", query)
	var args []string
	for _, field := range strings.Fields(query[index+len("decrby"):]) {
		// The command is RESP encoded, so a length header precedes every
		// bulk string. Only the payloads are arguments.
		if strings.HasPrefix(field, "$") || strings.HasPrefix(field, "*") {
			continue
		}
		args = append(args, field)
	}
	require.Len(t, args, 2, "expected key and amount, got %q", query)
	return args[0] + " " + args[1]
}

func TestParsePriceRejectsUnusableRates(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		cases := []struct {
			name  string
			extra map[string]interface{}
		}{
			{
				// Half a micro-credit is not a rate this plugin can apply.
				// Truncating it to zero would price the model at nothing and
				// look like it worked.
				name:  "fractional rate",
				extra: map[string]interface{}{"default_price": map[string]interface{}{"input_micros": 0.5}},
			},
			{
				name:  "negative rate",
				extra: map[string]interface{}{"default_price": map[string]interface{}{"input_micros": -1}},
			},
			{
				name:  "unknown unit",
				extra: map[string]interface{}{"default_price": map[string]interface{}{"unit": "images"}},
			},
			{
				name:  "model prices is not an object",
				extra: map[string]interface{}{"model_prices": []string{"glm-5.3"}},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				// The mock host is a process-wide singleton behind a mutex.
				// A host that is dropped instead of reset never releases it,
				// and the next subtest waits forever -- including a host whose
				// plugin start failed, which is every case here.
				host, status := test.NewTestHost(priceConfig(tc.extra))
				defer host.Reset()
				require.Equal(t, types.OnPluginStartStatusFailed, status)
			})
		}
	})
}

func TestChargeFor(t *testing.T) {
	cases := []struct {
		name       string
		price      Price
		usage      tokenBreakdown
		usageKnown bool
		want       int64
		chargeable bool
	}{
		{
			name:       "per thousand tokens",
			price:      Price{Unit: UnitTokens, Per: 1000, InputMicros: 2_000_000, OutputMicros: 8_000_000},
			usage:      tokenBreakdown{Input: 10000, Output: 2000},
			usageKnown: true,
			want:       36_000,
			chargeable: true,
		},
		{
			// 66.75 credits. A whole-credit ledger had to round this; the
			// milli-credit ledger represents it exactly.
			name: "cache components keep their fractional credit",
			price: Price{Unit: UnitTokens, Per: 1, InputMicros: 1_000_000, OutputMicros: 2_000_000,
				CacheReadMicros: 100_000, CacheWriteMicros: 1_250_000},
			usage:      tokenBreakdown{Input: 20, Output: 10, CacheRead: 80, CacheWrite: 15},
			usageKnown: true,
			want:       66_750,
			chargeable: true,
		},
		{
			// The AIGC case: the response carried no usage, and the request is
			// still a request.
			name:       "requests unit bills without usage",
			price:      Price{Unit: UnitRequests, Per: 1, RequestMicros: 5_000_000},
			usage:      tokenBreakdown{},
			usageKnown: false,
			want:       5_000,
			chargeable: true,
		},
		{
			// A token price cannot bill a token count it never received. It
			// reports nothing chargeable rather than deducting a guess.
			name:       "token unit without usage is not chargeable",
			price:      Price{Unit: UnitTokens, Per: 1, InputMicros: 1_000_000},
			usage:      tokenBreakdown{},
			usageKnown: false,
			want:       0,
			chargeable: false,
		},
		{
			name:       "a zero rate really is free",
			price:      Price{Unit: UnitTokens, Per: 1, MinChargeMillis: 1},
			usage:      tokenBreakdown{Input: 10000, Output: 5000},
			usageKnown: true,
			want:       0,
			chargeable: true,
		},
		{
			// Too small to round to even a thousandth of a credit, but not
			// free: min_charge_millis is how an operator says a real request
			// must never cost nothing.
			name:       "min charge floors a sub-milli request",
			price:      Price{Unit: UnitTokens, Per: 1, InputMicros: 10, MinChargeMillis: 1},
			usage:      tokenBreakdown{Input: 10},
			usageKnown: true,
			want:       1,
			chargeable: true,
		},
		{
			// The regression this ledger exists for. Priced from a vendor
			// sheet -- 8 credits per million input, 28 per million output --
			// an ordinary 500-in/500-out chat costs 0.018 credits. Rounded to
			// whole credits that is zero, and for a while every ordinary call
			// on this gateway billed nothing at all.
			name: "an ordinary chat priced per million tokens still bills",
			price: Price{Unit: UnitTokens, Per: 1_000_000,
				InputMicros: 8_000_000, OutputMicros: 28_000_000},
			usage:      tokenBreakdown{Input: 500, Output: 500},
			usageKnown: true,
			want:       18,
			chargeable: true,
		},
		{
			// `per` scales the rate and the divisor together, so the same
			// price quoted at either scale must charge the same. An operator
			// who reads "每多少 Token" as a lever on cost is reading it wrong,
			// and this is what holds that.
			name: "per is presentation and does not change the charge",
			price: Price{Unit: UnitTokens, Per: 1_000,
				InputMicros: 8_000, OutputMicros: 28_000},
			usage:      tokenBreakdown{Input: 500, Output: 500},
			usageKnown: true,
			want:       18,
			chargeable: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, chargeable, err := chargeFor(tc.price, metered{tokens: tc.usage, tokensKnown: tc.usageKnown, succeeded: true})
			require.NoError(t, err)
			require.Equal(t, tc.chargeable, chargeable)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestChargeForRefusesAnOutOfRangeAmount(t *testing.T) {
	// A Go int is 32 bits on wasm. A charge past that must be refused, not
	// wrapped around into a small and plausible-looking credit.
	price := Price{Unit: UnitTokens, Per: 1, InputMicros: 1_000_000_000}
	_, _, err := chargeFor(price, metered{tokens: tokenBreakdown{Input: 1_000_000_000}, tokensKnown: true, succeeded: true})
	require.Error(t, err)
}

func TestStreamingChargeUsesTheModelPrice(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		cases := []struct {
			name     string
			extra    map[string]interface{}
			model    string
			terminal []byte
			want     string
		}{
			{
				// No price anywhere: one token, one unit, exactly as before
				// credits existed. This is what makes the build safe to deploy
				// ahead of any price configuration.
				// One token still costs one whole credit here, which in the
				// ledger's unit is 1000. An unpriced route must not get a
				// thousandfold discount out of a units change.
				name:     "unpriced route still charges one credit per token",
				extra:    map[string]interface{}{},
				model:    "glm-5.2",
				terminal: []byte(`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`),
				want:     "12000",
			},
			{
				name: "model price wins over the route default",
				extra: map[string]interface{}{
					"default_price": map[string]interface{}{"unit": "tokens", "per": 1000, "input_micros": 1000000, "output_micros": 1000000},
					"model_prices": map[string]interface{}{
						"glm-5.2": map[string]interface{}{"unit": "tokens", "per": 1000, "input_micros": 2000000, "output_micros": 8000000},
					},
				},
				model:    "glm-5.2",
				terminal: []byte(`data: {"choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`),
				want:     "36000",
			},
			{
				// A model nobody has priced is the normal case on a route whose
				// upstream keeps adding models. It falls to the route default
				// instead of going free or being refused.
				name: "unknown model falls to the route default",
				extra: map[string]interface{}{
					"default_price": map[string]interface{}{"unit": "tokens", "per": 1000, "input_micros": 1000000, "output_micros": 1000000},
				},
				model:    "a-model-that-appeared-yesterday",
				terminal: []byte(`data: {"choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`),
				want:     "12000",
			},
			{
				// Self-hosted capacity: the intent is written down as a rate of
				// zero rather than expressed by leaving the plugin unbound.
				name: "a zero rate charges nothing",
				extra: map[string]interface{}{
					"default_price": map[string]interface{}{"unit": "tokens", "per": 1},
				},
				model:    "glm-5.3-flash",
				terminal: []byte(`data: {"choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`),
				want:     "0",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				host, status := test.NewTestHost(priceConfig(tc.extra))
				defer host.Reset()
				require.Equal(t, types.OnPluginStartStatusOK, status)

				host.CallOnHttpRequestHeaders([][2]string{
					{":authority", "example.com"},
					{":path", "/v1/chat/completions"},
					{":method", "POST"},
					{"x-mse-consumer", "consumer1"},
					{"x-higress-llm-model", tc.model},
				})
				host.CallOnRedisCall(0, test.CreateRedisResp(1000000))
				host.CallOnHttpRequestBody([]byte(`{"stream":true}`))

				host.CallOnHttpStreamingResponseBody(tc.terminal, false)
				calls, query := redisCalloutCountAndLastQuery(host)
				require.Equal(t, 1, calls)
				require.Equal(t, "chat_quota:consumer1 "+tc.want, decrByArguments(t, query))
			})
		}
	})
}

// TestAFailedRequestIsNotChargedPerRequest: a per-request price bills the
// attempt, so without a status check an upstream 500 would deduct a full
// request's worth of credits from the caller who received the error.
func TestAFailedRequestIsNotChargedPerRequest(t *testing.T) {
	price := Price{Unit: UnitRequests, Per: 1, RequestMicros: 2_000_000}

	got, chargeable, err := chargeFor(price, metered{succeeded: false})
	require.NoError(t, err)
	require.False(t, chargeable, "a failed request must not be charged")
	require.Equal(t, int64(0), got)

	got, chargeable, err = chargeFor(price, metered{succeeded: true})
	require.NoError(t, err)
	require.True(t, chargeable)
	require.Equal(t, int64(2_000), got)
}

// TestAFailedStreamStillPaysForTheTokensItProduced: the status gate is for
// the per-request unit only. A stream that answered, generated real tokens
// and then broke has consumed capacity, and refunding it because the
// connection died afterwards would be the wrong correction.
func TestAFailedStreamStillPaysForTheTokensItProduced(t *testing.T) {
	price := Price{Unit: UnitTokens, Per: 1000, InputMicros: 1_000_000, OutputMicros: 4_000_000}
	usage := tokenBreakdown{Input: 10_000, Output: 2_000}

	got, chargeable, err := chargeFor(price, metered{tokens: usage, tokensKnown: true, succeeded: false})
	require.NoError(t, err)
	require.True(t, chargeable)
	require.Equal(t, int64(18_000), got)
}
