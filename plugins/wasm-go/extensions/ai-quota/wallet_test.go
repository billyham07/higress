package main

// Ways the wallet gate can fail, each named by the test that catches it:
//
//  1. Admission reads the wrong hash, or the wallet before the key.
//     TestAdmissionReadsTheKeyThenItsWallet
//  2. A key the console never issued, or one whose wallet is gone, is let
//     through (the gate must stay fail-closed). TestAdmissionDecisions
//  3. An empty or overdrawn wallet admits. TestAdmissionDecisions
//  4. A key's period cap counts usage from an earlier period, so the key stays
//     locked after a refresh. TestAdmissionDecisions
//  5. A one-time cap is forgotten across periods. TestAdmissionDecisions
//  6. An unlimited wallet refuses at zero, or bypasses the key's own cap.
//     TestAdmissionDecisions
//  7. The charge spends a wallet other than the one admission resolved, or a
//     token count other than the response's. TestTheChargeSpendsTheWalletAdmissionResolved
//  8. A request that was never admitted is charged against a guessed wallet.
//     TestAChargeWithoutAdmissionIsNotSent

import (
	"net/http"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

// keyHash is an HMGET reply for quota_key in quotaKeyFields order.
func keyHash(owner, mode, limit, period string, periodUsed, onceUsed interface{}) []byte {
	var ownerValue, limitValue, periodValue interface{}
	if owner != "" {
		ownerValue = owner
	}
	if limit != "" {
		limitValue = limit
	}
	if period != "" {
		periodValue = period
	}
	return test.CreateRedisRespArray([]interface{}{ownerValue, mode, limitValue, periodValue, periodUsed, onceUsed})
}

// walletHash is an HMGET reply for quota_wallet in quotaWalletFields order.
func walletHash(period, periodLeft string, unlimited interface{}) []byte {
	var periodValue interface{}
	if period != "" {
		periodValue = period
	}
	return test.CreateRedisRespArray([]interface{}{periodValue, periodLeft, unlimited})
}

// admitFunded answers both admission reads for an uncapped key spending a
// wallet with plenty left.
func admitFunded(host test.TestHost) {
	host.CallOnRedisCall(0, keyHash("u:1", "none", "", "", nil, nil))
	host.CallOnRedisCall(0, walletHash("monthly:1788192000:g0", "1000000000", nil))
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
		require.Contains(t, query, "quota_key:consumer1")

		host.CallOnRedisCall(0, keyHash("u:7", "none", "", "", nil, nil))
		calls, query = redisCalloutCountAndLastQuery(host)
		require.Equal(t, 1, calls)
		require.Contains(t, query, "quota_wallet:u:7")

		host.CallOnRedisCall(0, walletHash("p1", "5", nil))
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
				wallet: walletHash("", "", nil),
				code:   "ai-quota.no_account",
			},
			{
				name:   "an empty wallet is refused",
				key:    keyHash("u:1", "none", "", "", nil, nil),
				wallet: walletHash("p1", "0", nil),
				code:   "ai-quota.noquota",
			},
			{
				name:   "an overdrawn wallet is refused",
				key:    keyHash("u:1", "none", "", "", nil, nil),
				wallet: walletHash("p1", "-300", nil),
				code:   "ai-quota.noquota",
			},
			{
				name:   "a period cap reached in this period refuses",
				key:    keyHash("u:1", "period", "1000", "p1", "1000", "1000"),
				wallet: walletHash("p1", "9000", nil),
				code:   "ai-quota.key_limit",
			},
			{
				// The key's counter belongs to an earlier period, so it has
				// spent nothing in this one.
				name:   "a period cap from an earlier period does not refuse",
				key:    keyHash("u:1", "period", "1000", "p0", "1000", "1000"),
				wallet: walletHash("p1", "9000", nil),
			},
			{
				name:   "a one-time cap reached refuses whatever the period",
				key:    keyHash("u:1", "once", "1000", "p0", "0", "1000"),
				wallet: walletHash("p1", "9000", nil),
				code:   "ai-quota.key_limit",
			},
			{
				name:   "an unlimited wallet admits with nothing left",
				key:    keyHash("g:5", "none", "", "p1", "0", "0"),
				wallet: walletHash("p1", "-5000000", "1"),
			},
			{
				name:   "an unlimited wallet still honours the key's own cap",
				key:    keyHash("g:5", "period", "1000", "p1", "1000", "1000"),
				wallet: walletHash("p1", "-5000000", "1"),
				code:   "ai-quota.key_limit",
			},
			{
				// Only "1" marks a wallet unlimited.
				name:   "a wallet with any other unlimited mark is limited",
				key:    keyHash("u:1", "none", "", "", nil, nil),
				wallet: walletHash("p1", "0", "0"),
				code:   "ai-quota.noquota",
			},
			{
				// No cap does not mean free: the wallet still decides.
				name:   "an uncapped key spends until the wallet is empty",
				key:    keyHash("u:1", "none", "", "p1", "99999999", "99999999"),
				wallet: walletHash("p1", "1", nil),
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
		host.CallOnRedisCall(0, walletHash("p1", "100000", nil))
		host.CallOnHttpStreamingResponseBody(
			[]byte(`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`), true)

		calls, query := redisCalloutCountAndLastQuery(host)
		require.Equal(t, 1, calls)
		require.Contains(t, query, "HINCRBY", "the charge is the checked-in script")
		require.Contains(t, query, "quota_key:consumer1")
		require.Contains(t, query, "quota_wallet:u:42")
		require.Contains(t, query, "$1\r\n7\r\n", "the charge is the response's token count")
	})
}

func TestAChargeWithoutAdmissionIsNotSent(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(basicConfig)
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		completionRequest(host)
		// The key read fails, so the request is refused and no owner is known.
		host.CallOnRedisCall(0, keyHash("", "", "", "", nil, nil))
		host.CallOnHttpStreamingResponseBody(
			[]byte(`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`), true)

		calls, _ := redisCalloutCountAndLastQuery(host)
		require.Equal(t, 0, calls)
	})
}
