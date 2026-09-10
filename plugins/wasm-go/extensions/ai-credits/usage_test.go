package main

import (
	"os"
	"testing"

	"github.com/higress-group/wasm-go/pkg/tokenusage"
	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenAISubtractsCachedFromInput(t *testing.T) {
	got := normalizeTokenUsage(tokenusage.TokenUsage{
		InputToken:  100,
		OutputToken: 10,
		TotalToken:  110,
		InputTokenDetails: map[string]int64{
			"cached_tokens": 20,
		},
	}, true)
	require.Equal(t, int64(80), got.InputTokens)
	require.Equal(t, int64(10), got.OutputTokens)
	require.Equal(t, int64(20), got.CachedReadTokens)
	require.Equal(t, int64(0), got.CachedWriteTokens)
	require.Equal(t, int64(110), got.TotalTokens)
	require.True(t, got.Complete)
}

func TestNormalizeAnthropicKeepsExclusiveInput(t *testing.T) {
	got := normalizeTokenUsage(tokenusage.TokenUsage{
		InputToken:  80,
		OutputToken: 10,
		TotalToken:  0,
		InputTokenDetails: map[string]int64{
			tokenusage.InputTokenDetailsKeyAnthropicMessagesUsageCacheReadInputTokens:     20,
			tokenusage.InputTokenDetailsKeyAnthropicMessagesUsageCacheCreationInputTokens: 5,
		},
	}, true)
	require.Equal(t, int64(80), got.InputTokens)
	require.Equal(t, int64(10), got.OutputTokens)
	require.Equal(t, int64(20), got.CachedReadTokens)
	require.Equal(t, int64(5), got.CachedWriteTokens)
	require.Equal(t, int64(0), got.TotalTokens)
	require.True(t, got.Complete)
}

func TestNormalizeOAIAndAnthropicShareChargeShape(t *testing.T) {
	oai := normalizeTokenUsage(tokenusage.TokenUsage{
		InputToken: 100, OutputToken: 10,
		InputTokenDetails: map[string]int64{"cached_tokens": 20},
	}, true)
	anthropic := normalizeTokenUsage(tokenusage.TokenUsage{
		InputToken: 80, OutputToken: 10,
		InputTokenDetails: map[string]int64{
			tokenusage.InputTokenDetailsKeyAnthropicMessagesUsageCacheReadInputTokens: 20,
		},
	}, true)
	require.Equal(t, oai.InputTokens, anthropic.InputTokens)
	require.Equal(t, oai.OutputTokens, anthropic.OutputTokens)
	require.Equal(t, oai.CachedReadTokens, anthropic.CachedReadTokens)
}

func TestNormalizeExplicitZeroUsageIsComplete(t *testing.T) {
	got := normalizeTokenUsage(tokenusage.TokenUsage{}, true)
	require.True(t, got.Complete)
	require.Equal(t, int64(0), got.InputTokens)
}

func TestNormalizeMissingUsageObjectIsIncomplete(t *testing.T) {
	got := normalizeTokenUsage(tokenusage.TokenUsage{InputToken: 0}, false)
	require.False(t, got.Complete)
}

func TestResponsesUsageObjectIsRecognizedInsideResponse(t *testing.T) {
	require.True(t, usageObjectPresent([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`)))
	require.False(t, usageObjectPresent([]byte(`data: {"type":"response.completed","response":{"status":"completed"}}`)))
}

// OpenAI Responses opens every stream with response.created and
// response.in_progress carrying "usage": null, and gjson reports a null field
// as existing. Treating that as usage evidence made a Responses stream that
// died before any real usage settle as COMPLETE at zero credits -- the call was
// free and the row was closed, so neither the recovery window nor an operator
// would revisit it. Observed on higress-system-test against a real qwen3.5
// upstream: /v1/responses stream=true returned 200, emitted only
// response.created and response.in_progress, and settled 0 micros complete.
func TestResponsesNullUsageIsNotEvidence(t *testing.T) {
	created := []byte(`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","usage":null}}`)
	inProgress := []byte(`data: {"type":"response.in_progress","response":{"id":"resp_1","status":"in_progress","usage":null}}`)
	require.False(t, usageObjectPresent(created))
	require.False(t, usageObjectPresent(inProgress))

	// A chat-completions chunk with an explicit null usage is the same case.
	require.False(t, usageObjectPresent([]byte(`data: {"choices":[{"delta":{"content":"hi"}}],"usage":null}`)))
	// Anthropic's message envelope likewise.
	require.False(t, usageObjectPresent([]byte(`data: {"type":"message_start","message":{"usage":null}}`)))

	// A real usage object still counts, including a legitimate all-zero one.
	require.True(t, usageObjectPresent([]byte(`data: {"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)))
	require.True(t, usageObjectPresent([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":2}}}`)))
}

// The end state the null above produced: a stream that carried no usage must
// reach the ledger as pending_verify, not as a settled zero charge.
func TestStreamWithOnlyNullUsageSettlesAsPendingVerify(t *testing.T) {
	created := []byte(`data: {"type":"response.created","response":{"id":"resp_1","usage":null}}`)
	seen := usageObjectPresent(created)
	require.False(t, seen)
	status, reason := settleReason(true, false, UsagePayload{Complete: seen}, 200)
	require.Equal(t, SettlePendingVerify, status)
	require.Equal(t, ReasonMissingUsage, reason)
}

func TestSettleReasonMissingUsageIsPending(t *testing.T) {
	status, reason := settleReason(true, false, UsagePayload{Complete: false}, 200)
	require.Equal(t, SettlePendingVerify, status)
	require.Equal(t, ReasonMissingUsage, reason)
}

func TestSettleReasonCompleteIsSettledOnce(t *testing.T) {
	status, reason := settleReason(true, false, UsagePayload{Complete: true, TotalTokens: 10}, 200)
	require.Equal(t, SettleSettled, status)
	require.Equal(t, ReasonEOS, reason)

	status, reason = settleReason(false, true, UsagePayload{Complete: true, TotalTokens: 10}, 200)
	require.Equal(t, SettleSettled, status)
	require.Equal(t, ReasonSemanticEnd, reason)
}

func TestSettleReasonUpstreamErrorKeepsEvidence(t *testing.T) {
	status, reason := settleReason(true, false, UsagePayload{Complete: false}, 500)
	require.Equal(t, SettlePendingVerify, status)
	require.Equal(t, ReasonUpstreamError, reason)
}

func TestClampOutputTokensCapsExisting(t *testing.T) {
	out, err := clampOutputTokens([]byte(`{"model":"qwen3.5","max_tokens":9999}`), 128)
	require.NoError(t, err)
	require.Contains(t, string(out), `"max_tokens":128`)
}

func TestParseAdmitDeny(t *testing.T) {
	parsed, err := parseAdmitResponse([]byte(`{"decision":"deny","error":{"code":"insufficient_credits","message":"no budget"}}`))
	require.NoError(t, err)
	require.Equal(t, DecisionDeny, parsed.Decision)
	require.Equal(t, "insufficient_credits", parsed.Error.Code)
}

func TestParseAdmitRejectsStringMicros(t *testing.T) {
	_, err := parseAdmitResponse([]byte(`{"decision":"allow","reservationId":"res-1","reservedCreditsMicros":"123456","maxBillableCreditsMicros":"999"}`))
	require.Error(t, err)
}

func TestParseAdmitRejectsInvalidJSON(t *testing.T) {
	_, err := parseAdmitResponse([]byte(`{decision:`))
	require.Error(t, err)
}

func TestWasmPluginTemplatesUseFailClose(t *testing.T) {
	for _, name := range []string{"plugin.yaml", "isolated-test.yaml"} {
		data, err := os.ReadFile(name)
		require.NoError(t, err, name)
		require.NotContains(t, string(data), "FAIL_CLOSED", name)
		require.Contains(t, string(data), "failStrategy: FAIL_CLOSE", name)
	}
}

func TestParseAdmitAcceptsInt64Micros(t *testing.T) {
	parsed, err := parseAdmitResponse([]byte(`{"decision":"allow","reservationId":"res-1","reservedCreditsMicros":123456,"maxBillableCreditsMicros":999,"maxOutputTokens":64,"strictCap":true}`))
	require.NoError(t, err)
	require.Equal(t, DecisionAllow, parsed.Decision)
	require.Equal(t, int64(123456), parsed.ReservedCreditsMicros)
	require.Equal(t, int64(999), parsed.MaxBillableCreditsMicros)
}
