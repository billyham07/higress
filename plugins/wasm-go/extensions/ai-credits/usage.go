package main

import (
	"bytes"

	"github.com/higress-group/wasm-go/pkg/tokenusage"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

const (
	ctxUsageInput      = "ai-credits-input"
	ctxUsageOutput     = "ai-credits-output"
	ctxUsageTotal      = "ai-credits-total"
	ctxUsageCacheRead  = "ai-credits-cache-read"
	ctxUsageCacheWrite = "ai-credits-cache-write"
	ctxUsageSeen       = "ai-credits-usage-seen"
	// ctxUsageComplete is deliberately separate from ctxUsageSeen. Providers
	// such as Anthropic emit input usage in message_start before the stream has
	// finished; seeing that object is not proof that a disconnect can be billed
	// precisely. Completion is set only by an explicit terminal marker or a
	// final usage-bearing chunk.
	ctxUsageComplete  = "ai-credits-usage-complete"
	ctxTerminalTail   = "ai-credits-terminal-tail"
	ctxSettled        = "ai-credits-settled"
	ctxHTTPStatus     = "ai-credits-http-status"
	ctxStream         = "ai-credits-stream"
	ctxReservationID  = "ai-credits-reservation-id"
	ctxRequestID      = "ai-credits-request-id"
	ctxAttemptID      = "ai-credits-attempt-id"
	ctxConsumer       = "ai-credits-consumer"
	ctxCanonicalModel = "ai-credits-canonical"
	ctxSkip           = "ai-credits-skip"
	ctxAdmitted       = "ai-credits-admitted"
	// ctxSettleRequest holds the settlement this request built, so the
	// stream-done hook can tell "never attempted" from "attempted".
	ctxSettleRequest = "ai-credits-settle-request"
	// ctxSettleAcked is set only when the backend answered 2xx. Nothing else
	// proves the settlement landed.
	ctxSettleAcked = "ai-credits-settle-acked"
	// ctxOutboxDurable is set only after both the Redis HSET response and the
	// local WAITAOF acknowledgement succeeded. It controls whether a failed
	// direct settle may rely on Redis as the recovery copy.
	ctxOutboxDurable = "ai-credits-outbox-durable"
	// ctxOutboxRaw is the exact wrapped value written by HSET. A 2xx callback
	// may arrive after another gateway write reused the field, so deletion must
	// compare this value instead of deleting by reservation id alone.
	ctxOutboxRaw = "ai-credits-outbox-raw"
	// ctxOutboxLifecycle points at a small heap object shared with the root
	// outbox verifier. A root callback can outlive this HTTP context, so it must
	// not call back into a destroyed context.
	ctxOutboxLifecycle = "ai-credits-outbox-lifecycle"
)

func accumulateUsage(ctx wrapper.HttpContext, data []byte) {
	usage := tokenusage.GetTokenUsage(ctx, data)
	enrichAnthropicStreamingUsage(ctx, data, &usage)
	if previous, ok := ctx.GetContext("ai-credits-details").(map[string]int64); ok {
		if usage.InputTokenDetails == nil {
			usage.InputTokenDetails = map[string]int64{}
		}
		for key, value := range previous {
			if _, exists := usage.InputTokenDetails[key]; !exists {
				usage.InputTokenDetails[key] = value
			}
		}
	}
	if len(usage.InputTokenDetails) > 0 {
		ctx.SetContext("ai-credits-details", usage.InputTokenDetails)
	}
	objectPresent := usageObjectPresent(data)
	// Some Envoy/provider combinations split an SSE JSON object across body
	// callbacks. The tokenusage SDK can still recover non-zero counters from a
	// completed piece, while the direct presence probe cannot. Remember that
	// counters are evidence too, but retain the explicit object probe so a
	// legitimate zero-token usage object is not lost.
	usageEvidence := objectPresent || usage.InputToken != 0 || usage.OutputToken != 0 ||
		usage.TotalToken != 0 || len(usage.InputTokenDetails) > 0
	if usageEvidence {
		ctx.SetContext(ctxUsageSeen, true)
	}
	if (ctx.GetBoolContext(ctxUsageSeen, false) || objectPresent) &&
		(bytes.Contains(data, []byte("message_stop")) ||
			bytes.Contains(data, []byte("response.completed")) ||
			bytes.Contains(data, []byte("response.incomplete")) ||
			bytes.Contains(data, []byte("response.failed"))) {
		ctx.SetContext(ctxUsageComplete, true)
	}
	normalized := normalizeTokenUsage(usage, ctx.GetBoolContext(ctxUsageSeen, false) || objectPresent)
	if usage.InputToken > 0 || objectPresent {
		ctx.SetContext(ctxUsageInput, normalized.InputTokens)
	}
	if usage.OutputToken > 0 || objectPresent {
		ctx.SetContext(ctxUsageOutput, normalized.OutputTokens)
	}
	if normalized.CachedReadTokens > 0 || objectPresent {
		ctx.SetContext(ctxUsageCacheRead, normalized.CachedReadTokens)
	}
	if normalized.CachedWriteTokens > 0 || objectPresent {
		ctx.SetContext(ctxUsageCacheWrite, normalized.CachedWriteTokens)
	}
	if usage.TotalToken > 0 || objectPresent {
		ctx.SetContext(ctxUsageTotal, normalized.TotalTokens)
	}
}

func snapshotUsage(ctx wrapper.HttpContext) UsagePayload {
	return UsagePayload{
		InputTokens:       int64FromCtx(ctx, ctxUsageInput),
		OutputTokens:      int64FromCtx(ctx, ctxUsageOutput),
		CachedReadTokens:  int64FromCtx(ctx, ctxUsageCacheRead),
		CachedWriteTokens: int64FromCtx(ctx, ctxUsageCacheWrite),
		TotalTokens:       int64FromCtx(ctx, ctxUsageTotal),
		Complete:          ctx.GetBoolContext(ctxUsageComplete, false),
	}
}

// normalizeTokenUsage implements v2 contract §6.0: inputTokens is non-cached input.
// OpenAI prompt_tokens includes cached_tokens; Anthropic input_tokens does not.
// totalTokens is upstream metadata and is not used for charging.
func normalizeTokenUsage(usage tokenusage.TokenUsage, usageObjectPresent bool) UsagePayload {
	read, write := cacheTokens(usage.InputTokenDetails)
	input := usage.InputToken
	if openAIInputIncludesCache(usage.InputTokenDetails) {
		if input >= read {
			input -= read
		} else {
			input = 0
		}
	}
	return UsagePayload{
		InputTokens:       input,
		OutputTokens:      usage.OutputToken,
		CachedReadTokens:  read,
		CachedWriteTokens: write,
		TotalTokens:       usage.TotalToken,
		Complete:          usageObjectPresent,
	}
}

// usageObjectPresent reports whether this chunk carries a usage object.
//
// An explicit JSON null is not one. OpenAI Responses opens every stream with
// response.created / response.in_progress carrying "usage": null, and gjson
// says a null field exists. Counting that as evidence meant a Responses stream
// that died before any real usage was settled as COMPLETE at zero credits: the
// call was free and the row was closed, so neither the recovery window nor an
// operator would ever revisit it. A legitimate zero-token usage object is an
// object with zero counters, which still reads as present here.
func usageObjectPresent(data []byte) bool {
	// OpenAI Responses streaming events carry the final usage under the
	// response object (response.completed). Keep this separate from the
	// semantic terminal check: usage alone is still only evidence until
	// response.completed or transport EOF arrives.
	for _, path := range []string{"usage", "message.usage", "response.usage"} {
		value := wrapper.GetValueFromBody(data, []string{path})
		if value != nil && value.Type != gjson.Null {
			return true
		}
	}
	return false
}

func openAIInputIncludesCache(details map[string]int64) bool {
	if details == nil {
		return false
	}
	_, ok := details["cached_tokens"]
	return ok
}

func int64FromCtx(ctx wrapper.HttpContext, key string) int64 {
	value, ok := ctx.GetContext(key).(int64)
	if !ok {
		return 0
	}
	return value
}

func cacheTokens(details map[string]int64) (int64, int64) {
	if details == nil {
		return 0, 0
	}
	var read, write int64
	if cached, ok := details["cached_tokens"]; ok {
		read = cached
	} else if cached, ok := details[tokenusage.InputTokenDetailsKeyAnthropicMessagesUsageCacheReadInputTokens]; ok {
		read = cached
	} else if cached, ok := details["cache_read_input_tokens"]; ok {
		read = cached
	}
	if cached, ok := details[tokenusage.InputTokenDetailsKeyAnthropicMessagesUsageCacheCreationInputTokens]; ok {
		write = cached
	} else if cached, ok := details["cache_creation_input_tokens"]; ok {
		write = cached
	}
	return read, write
}

func isSemanticStreamEnd(ctx wrapper.HttpContext, data []byte) bool {
	tail, _ := ctx.GetContext(ctxTerminalTail).([]byte)
	probe := make([]byte, 0, len(tail)+len(data))
	probe = append(probe, tail...)
	probe = append(probe, data...)
	const terminalTailBytes = 512
	if len(probe) > terminalTailBytes {
		tail = append([]byte(nil), probe[len(probe)-terminalTailBytes:]...)
	} else {
		tail = append([]byte(nil), probe...)
	}
	ctx.SetContext(ctxTerminalTail, tail)
	if bytes.Contains(probe, []byte("[DONE]")) || bytes.Contains(probe, []byte("message_stop")) {
		return true
	}
	if bytes.Contains(probe, []byte("response.completed")) ||
		bytes.Contains(probe, []byte("response.incomplete")) ||
		bytes.Contains(probe, []byte("response.failed")) {
		// The semantic marker closes the Responses stream. Usage is still
		// required by settleReason, so recognizing the marker here cannot turn
		// a marker-only response into a charge.
		return true
	}
	return bytes.Contains(probe, []byte(`"message_delta"`)) &&
		bytes.Contains(probe, []byte(`"stop_reason"`)) &&
		bytes.Contains(probe, []byte(`"usage"`))
}

func enrichAnthropicStreamingUsage(ctx wrapper.HttpContext, data []byte, usage *tokenusage.TokenUsage) {
	if usage.InputTokenDetails == nil {
		usage.InputTokenDetails = make(map[string]int64)
	}
	if previous, ok := ctx.GetContext("ai-credits-details").(map[string]int64); ok {
		for key, value := range previous {
			if _, exists := usage.InputTokenDetails[key]; !exists {
				usage.InputTokenDetails[key] = value
			}
		}
	}
	if value := wrapper.GetValueFromBody(data, []string{"message.usage.cache_read_input_tokens"}); value != nil {
		usage.InputTokenDetails[tokenusage.InputTokenDetailsKeyAnthropicMessagesUsageCacheReadInputTokens] = value.Int()
	}
	if value := wrapper.GetValueFromBody(data, []string{"message.usage.cache_creation_input_tokens"}); value != nil {
		usage.InputTokenDetails[tokenusage.InputTokenDetailsKeyAnthropicMessagesUsageCacheCreationInputTokens] = value.Int()
	}
	if value := wrapper.GetValueFromBody(data, []string{"usage.output_tokens"}); value != nil &&
		bytes.Contains(data, []byte(`"message_delta"`)) {
		usage.OutputToken = value.Int()
	}
}

func settleReason(endOfStream bool, semantic bool, usage UsagePayload, httpStatus int) (status, reason string) {
	if httpStatus >= 400 {
		if usage.Complete {
			return SettleSettled, ReasonUpstreamError
		}
		return SettlePendingVerify, ReasonUpstreamError
	}
	if usage.Complete {
		if semantic && !endOfStream {
			return SettleSettled, ReasonSemanticEnd
		}
		return SettleSettled, ReasonEOS
	}
	if endOfStream {
		return SettlePendingVerify, ReasonMissingUsage
	}
	if semantic {
		return SettlePendingVerify, ReasonMissingUsage
	}
	return SettlePendingVerify, ReasonDisconnect
}
