package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

// A route's credit multiplier scales what every request on it is charged.
// Its reason to exist is 0x: self-hosted capacity (the H20 group) that people
// may keep using after they have spent their credits on paid vendors.
//
// Ways this can go wrong, each caught by a test below:
//
//  1. A 0x route still refuses an empty or overdrawn wallet, which is the
//     one thing the multiplier was added to stop.
//  2. A 0x route refuses a key that reached its own cap. A free request
//     spends nothing, so the cap has nothing to protect.
//  3. A 0x route admits a key the console never issued. Free is a price, not
//     an identity: the gate stays fail-closed on missing records.
//  4. A 0x route still spends the wallet.
//  5. A fractional multiplier charges the base amount, or rounds differently
//     from the base price.
//  6. The multiplier is skipped on an unpriced route (1 token = 1 credit).
//  7. The price's min_charge_millis floor lifts a 0x charge off zero.
//  8. The access log shows the charge but not what it was scaled from, so a
//     0 cannot be told from a free price.
//  9. A malformed multiplier is accepted and silently read as 0 or 1x.
// 10. A route without a multiplier stops refusing empty wallets.

func multiplierConfig(percent interface{}, extra map[string]interface{}) []byte {
	merged := map[string]interface{}{"multiplier_percent": percent}
	for key, value := range extra {
		merged[key] = value
	}
	return priceConfig(merged)
}

var tokenPrice = map[string]interface{}{
	"model_prices": map[string]interface{}{
		"glm-5.2": map[string]interface{}{"unit": "tokens", "per": 1000, "input_micros": 2000000, "output_micros": 8000000},
	},
}

func modelRequest(host test.TestHost) types.Action {
	return host.CallOnHttpRequestHeaders([][2]string{
		{":authority", "example.com"},
		{":path", "/v1/chat/completions"},
		{":method", "POST"},
		{"x-mse-consumer", "consumer1"},
		{"x-higress-llm-model", "glm-5.2"},
	})
}

// Failures 1, 2, 3 and 10.
func TestAdmissionUnderAMultiplier(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		cases := []struct {
			name    string
			percent interface{} // nil leaves the field out
			key     []byte
			wallet  []byte
			code    string
		}{
			{
				name:    "a free route admits an empty wallet",
				percent: 0,
				key:     keyHash("u:1", "none", "", "", nil, nil),
				wallet:  walletHash("p1", "0", "0"),
			},
			{
				name:    "a free route admits an overdrawn wallet",
				percent: 0,
				key:     keyHash("u:1", "none", "", "", nil, nil),
				wallet:  walletHash("p1", "-250000", "0"),
			},
			{
				name:    "a free route admits a key past its own cap",
				percent: 0,
				key:     keyHash("u:1", "once", "1000", "p1", "0", "1000"),
				wallet:  walletHash("p1", "0", "0"),
			},
			{
				name:    "a free route still refuses a key the console never issued",
				percent: 0,
				key:     keyHash("", "", "", "", nil, nil),
				code:    "ai-quota.no_account",
			},
			{
				name:    "a free route still refuses a key whose wallet is gone",
				percent: 0,
				key:     keyHash("u:1", "none", "", "", nil, nil),
				wallet:  walletHash("", "", ""),
				code:    "ai-quota.no_account",
			},
			{
				name:    "a discounted route still refuses an empty wallet",
				percent: 50,
				key:     keyHash("u:1", "none", "", "", nil, nil),
				wallet:  walletHash("p1", "0", "0"),
				code:    "ai-quota.noquota",
			},
			{
				name:   "a route without a multiplier still refuses an empty wallet",
				key:    keyHash("u:1", "none", "", "", nil, nil),
				wallet: walletHash("p1", "0", "0"),
				code:   "ai-quota.noquota",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				config := priceConfig(tokenPrice)
				if tc.percent != nil {
					config = multiplierConfig(tc.percent, tokenPrice)
				}
				host, status := test.NewTestHost(config)
				defer host.Reset()
				require.Equal(t, types.OnPluginStartStatusOK, status)

				modelRequest(host)
				host.CallOnRedisCall(0, tc.key)
				if tc.wallet != nil {
					host.CallOnRedisCall(0, tc.wallet)
				}
				response := host.GetLocalResponse()
				if tc.code == "" {
					require.Nil(t, response, "expected the request to be admitted")
					require.Equal(t, types.ActionContinue, host.GetHttpStreamAction())
					return
				}
				require.NotNil(t, response, "expected a refusal")
				require.Equal(t, uint32(http.StatusForbidden), response.StatusCode)
				require.Equal(t, tc.code, response.StatusCodeDetail)
			})
		}
	})
}

// Failures 4, 5, 6, 7 and 8.
func TestTheChargeUnderAMultiplier(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// 10000 input + 2000 output at 2 and 8 credits per thousand is 36
		// credits, 36000 milli-credits, before any multiplier.
		terminal := []byte(`data: {"choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`)
		cases := []struct {
			name     string
			percent  interface{}
			extra    map[string]interface{}
			terminal []byte
			charged  string // "" when the wallet must not be touched
			logged   string
			base     string
		}{
			{
				name:    "a free route spends nothing and says what it would have cost",
				percent: 0, extra: tokenPrice, terminal: terminal,
				logged: "0", base: "36000",
			},
			{
				name:    "a half-price route charges half",
				percent: 50, extra: tokenPrice, terminal: terminal,
				charged: "18000", logged: "18000", base: "36000",
			},
			{
				name:    "a premium route charges more",
				percent: 150, extra: tokenPrice, terminal: terminal,
				charged: "54000", logged: "54000", base: "36000",
			},
			{
				// 7 milli-credits at a third rounds half up, like the price.
				name:    "a fraction of a milli-credit rounds half up",
				percent: 50,
				extra: map[string]interface{}{"default_price": map[string]interface{}{
					"unit": "tokens", "per": 1000, "input_micros": 1000000}},
				terminal: []byte(`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":0,"total_tokens":7}}`),
				charged:  "4", logged: "4", base: "7",
			},
			{
				name:    "an unpriced route is scaled too",
				percent: 50, extra: nil,
				terminal: []byte(`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`),
				charged:  "3500", logged: "3500", base: "7000",
			},
			{
				name:    "the price's floor does not lift a free route off zero",
				percent: 0,
				extra: map[string]interface{}{"default_price": map[string]interface{}{
					"unit": "tokens", "per": 1000000, "input_micros": 1, "min_charge_millis": 5}},
				terminal: []byte(`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":0,"total_tokens":3}}`),
				logged:   "0", base: "5",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				host, status := test.NewTestHost(multiplierConfig(tc.percent, tc.extra))
				defer host.Reset()
				require.Equal(t, types.OnPluginStartStatusOK, status)
				require.NoError(t, host.SetProperty([]string{aiLogKey}, quoteForProperty([]byte("{}"))))

				modelRequest(host)
				// An empty wallet: only a free route gets past admission with
				// it, so the funded wallet is used for the paying cases.
				host.CallOnRedisCall(0, keyHash("u:1", "none", "", "", nil, nil))
				if tc.charged == "" {
					host.CallOnRedisCall(0, walletHash("p1", "0", "0"))
				} else {
					host.CallOnRedisCall(0, walletHash("p1", "1000000000", "0"))
				}
				require.Nil(t, host.GetLocalResponse())
				before, _ := redisCalloutCountAndLastQuery(host)
				host.CallOnHttpStreamingResponseBody(tc.terminal, true)

				after, query := redisCalloutCountAndLastQuery(host)
				if tc.charged == "" {
					require.Equal(t, before, after, "a free request must not touch the wallet")
				} else {
					require.Equal(t, before+1, after)
					arguments := strings.Fields(chargeArguments(t, query))
					require.Equal(t, tc.charged, arguments[len(arguments)-1])
				}

				logged, present := accessLogField(t, host, "credit_millis")
				require.True(t, present)
				require.Equal(t, tc.logged, logged)
				base, present := accessLogField(t, host, "credit_base_millis")
				require.True(t, present)
				require.Equal(t, tc.base, base)
				percent, present := accessLogField(t, host, "credit_multiplier_percent")
				require.True(t, present)
				require.Equal(t, formatPercent(tc.percent), percent)
			})
		}
	})
}

// Failure 8, for the route nobody set a multiplier on: the log still says 1x,
// so every row of the column means the same thing.
func TestAnUnscaledChargeLogsOneTimes(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(priceConfig(tokenPrice))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		require.NoError(t, host.SetProperty([]string{aiLogKey}, quoteForProperty([]byte("{}"))))

		modelRequest(host)
		admitFunded(host)
		host.CallOnHttpStreamingResponseBody(
			[]byte(`data: {"choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`), true)

		logged, _ := accessLogField(t, host, "credit_millis")
		base, _ := accessLogField(t, host, "credit_base_millis")
		percent, _ := accessLogField(t, host, "credit_multiplier_percent")
		require.Equal(t, "36000", logged)
		require.Equal(t, "36000", base)
		require.Equal(t, "100", percent)
	})
}

// Failure 9.
func TestAMalformedMultiplierIsRefused(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		for _, value := range []interface{}{-1, 0.5, "50", 10001} {
			host, status := test.NewTestHost(multiplierConfig(value, nil))
			require.Equal(t, types.OnPluginStartStatusFailed, status, "multiplier_percent %v", value)
			host.Reset()
		}
	})
}

func formatPercent(value interface{}) string {
	return strconv.Itoa(value.(int))
}
