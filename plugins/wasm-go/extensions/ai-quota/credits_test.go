package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

// keyHash is an HMGET reply for credit_key in creditKeyFields order.
func keyHash(owner, mode string, limit, period string, periodUsed, onceUsed interface{}) []byte {
	var ownerValue interface{} = owner
	if owner == "" {
		ownerValue = nil
	}
	var limitValue, periodValue interface{}
	if limit != "" {
		limitValue = limit
	}
	if period != "" {
		periodValue = period
	}
	return test.CreateRedisRespArray([]interface{}{ownerValue, mode, limitValue, periodValue, periodUsed, onceUsed})
}

// walletHash is an HMGET reply for credit_wallet in creditWalletFields order.
func walletHash(period string, periodLeft, extraLeft string) []byte {
	var periodValue interface{} = period
	if period == "" {
		periodValue = nil
	}
	return test.CreateRedisRespArray([]interface{}{periodValue, periodLeft, extraLeft})
}

// admitFunded answers both admission reads for a key with no cap of its own
// spending a wallet with plenty left.
func admitFunded(host test.TestHost) {
	host.CallOnRedisCall(0, keyHash("u:1", "none", "", "", nil, nil))
	host.CallOnRedisCall(0, walletHash("month:1788192000:g0", "1000000000", "0"))
}

func completionRequest(host test.TestHost) types.Action {
	return host.CallOnHttpRequestHeaders([][2]string{
		{":authority", "example.com"},
		{":path", "/v1/chat/completions"},
		{":method", "POST"},
		{"x-mse-consumer", "consumer1"},
	})
}

func TestAdmissionReadsTheKeyThenItsWallet(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(basicConfig)
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		require.Equal(t, types.HeaderStopAllIterationAndWatermark, completionRequest(host))
		calls, query := redisCalloutCountAndLastQuery(host)
		require.Equal(t, 1, calls)
		require.Contains(t, query, "credit_key:consumer1")

		host.CallOnRedisCall(0, keyHash("u:7", "none", "", "", nil, nil))
		calls, query = redisCalloutCountAndLastQuery(host)
		require.Equal(t, 1, calls)
		require.Contains(t, query, "credit_wallet:u:7")

		host.CallOnRedisCall(0, walletHash("p1", "5", "0"))
		require.Equal(t, types.ActionContinue, host.GetHttpStreamAction())
		host.CompleteHttp()
	})
}

func TestAdmissionDecisions(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		cases := []struct {
			name   string
			key    []byte
			wallet []byte // nil when the key read already decides
			code   string // empty when admitted
		}{
			{
				name: "a key the console never issued is refused",
				key:  keyHash("", "", "", "", nil, nil),
				code: "ai-quota.no_account",
			},
			{
				name:   "a key whose wallet is gone is refused",
				key:    keyHash("u:1", "none", "", "", nil, nil),
				wallet: walletHash("", "", ""),
				code:   "ai-quota.no_account",
			},
			{
				name:   "an empty wallet is refused",
				key:    keyHash("u:1", "none", "", "", nil, nil),
				wallet: walletHash("p1", "0", "0"),
				code:   "ai-quota.noquota",
			},
			{
				// Overshoot sits on the period bucket and must not hide the
				// one-time credits that are still there.
				name:   "extra credits carry a wallet whose period is overdrawn",
				key:    keyHash("u:1", "none", "", "", nil, nil),
				wallet: walletHash("p1", "-300", "50"),
			},
			{
				name:   "a period cap reached in this period refuses",
				key:    keyHash("u:1", "period", "1000", "p1", "1000", "1000"),
				wallet: walletHash("p1", "9000", "0"),
				code:   "ai-quota.key_limit",
			},
			{
				// The key's counter belongs to an earlier period, so it has
				// spent nothing in this one.
				name:   "a period cap from an earlier period does not refuse",
				key:    keyHash("u:1", "period", "1000", "p0", "1000", "1000"),
				wallet: walletHash("p1", "9000", "0"),
			},
			{
				name:   "a one-time cap reached refuses whatever the period",
				key:    keyHash("u:1", "once", "1000", "p0", "0", "1000"),
				wallet: walletHash("p1", "9000", "0"),
				code:   "ai-quota.key_limit",
			},
			{
				// No cap does not mean free: the wallet still decides.
				name:   "an uncapped key spends until the wallet is empty",
				key:    keyHash("u:1", "none", "", "p1", "99999999", "99999999"),
				wallet: walletHash("p1", "1", "0"),
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				host, status := test.NewTestHost(basicConfig)
				defer host.Reset()
				require.Equal(t, types.OnPluginStartStatusOK, status)

				completionRequest(host)
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

func TestTheChargeSpendsTheWalletAdmissionResolved(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(basicConfig)
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		completionRequest(host)
		host.CallOnRedisCall(0, keyHash("u:42", "period", "5000", "p1", "0", "0"))
		host.CallOnRedisCall(0, walletHash("p1", "100000", "0"))
		host.CallOnHttpStreamingResponseBody(
			[]byte(`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`), true)

		calls, query := redisCalloutCountAndLastQuery(host)
		require.Equal(t, 1, calls)
		require.Contains(t, query, "HINCRBY", "the charge is the checked-in script")
		require.Contains(t, query, "credit_key:consumer1")
		require.Contains(t, query, "credit_wallet:u:42")
		// Unpriced route: one token is one whole credit, 1000 milli-credits.
		require.True(t, strings.Contains(query, "7000"), "query %q does not carry the amount", query)
	})
}

func TestARefusedRequestIsNeverCharged(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(basicConfig)
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		completionRequest(host)
		host.CallOnRedisCall(0, keyHash("", "", "", "", nil, nil))
		host.CallOnHttpStreamingResponseBody(
			[]byte(`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`), true)

		calls, _ := redisCalloutCountAndLastQuery(host)
		require.Equal(t, 0, calls)
	})
}
