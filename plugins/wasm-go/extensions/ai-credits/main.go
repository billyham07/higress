package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const pluginName = "ai-credits"

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
		wrapper.ProcessResponseHeaders(onHttpResponseHeaders),
		wrapper.ProcessStreamingResponseBody(onHttpStreamingResponseBody),
		wrapper.ProcessStreamDone(onHttpStreamDone),
		wrapper.PrePluginStartOrReload[CreditsConfig](func(wrapper.PluginContext) error {
			// The wrapper rebuilds the root tick list for every plugin start or
			// config reload. Drop verifier state before parseConfig installs the
			// one root-context persistence worker for the new configuration.
			resetOutboxVerifier()
			return nil
		}),
	)
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config CreditsConfig) types.Action {
	stripCreditsClientHeaders()
	if config.Mode == ModeOff {
		ctx.DontReadRequestBody()
		ctx.SetContext(ctxSkip, true)
		return types.ActionContinue
	}

	path := ctx.Path()
	if !pathEnabled(path, config.EnableSuffixes) {
		ctx.DontReadRequestBody()
		ctx.SetContext(ctxSkip, true)
		return types.ActionContinue
	}

	consumer, err := proxywasm.GetHttpRequestHeader(config.ConsumerHeader)
	consumer = strings.TrimSpace(consumer)
	if err != nil || consumer == "" {
		if config.Mode == ModeEnforce {
			return denyLocal(401, "unauthenticated", "request is not authenticated")
		}
		ctx.DontReadRequestBody()
		ctx.SetContext(ctxSkip, true)
		return types.ActionContinue
	}
	ctx.SetContext(ctxConsumer, consumer)
	requestID := trustedRequestID()
	attemptID := uuid.NewString()
	ctx.SetContext(ctxRequestID, requestID)
	ctx.SetContext(ctxAttemptID, attemptID)

	if !wrapper.HasRequestBody() {
		if config.Mode == ModeEnforce {
			return denyLocal(400, errMissingModel, "request is missing model")
		}
		ctx.SetContext(ctxSkip, true)
		return types.ActionContinue
	}
	proxywasm.RemoveHttpRequestHeader("content-length")
	ctx.SetRequestBodyBufferLimit(32 * 1024 * 1024)
	return types.HeaderStopIteration
}

func onHttpRequestBody(ctx wrapper.HttpContext, config CreditsConfig, body []byte) types.Action {
	if ctx.GetBoolContext(ctxSkip, false) || config.Mode == ModeOff {
		return types.ActionContinue
	}
	consumer, _ := ctx.GetContext(ctxConsumer).(string)
	path := ctx.Path()
	modelValue := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	stream := gjson.GetBytes(body, "stream").Bool()
	ctx.SetContext(ctxStream, stream)

	canonical := modelValue
	upstream := modelValue
	groupKey := ""
	endpoint := endpointFromPath(path)
	if config.Registry != nil && config.Registry.Enabled {
		if config.Registry.publicationBlocked() {
			if config.Mode == ModeEnforce {
				return denyLocal(statusForCode(errRegistryUnavailable), errRegistryUnavailable, "unified registry has no published models")
			}
		} else {
			resolved := config.Registry.Resolve(path, modelValue)
			if !resolved.OK {
				if config.Mode == ModeEnforce {
					return denyLocal(statusForCode(resolved.ErrorCode), resolved.ErrorCode, resolved.ErrorMessage)
				}
				log.Warnf("ai-credits shadow resolve failed: %s %s", resolved.ErrorCode, resolved.ErrorMessage)
			} else {
				canonical = resolved.Entry.CanonicalID
				upstream = resolved.Entry.UpstreamModel
				groupKey = resolved.Entry.GroupKey
				endpoint = resolved.Endpoint
				if ok, code, message := config.Registry.Authorize(consumer, canonical); !ok {
					if config.Mode == ModeEnforce {
						return denyLocal(statusForCode(code), code, message)
					}
					log.Warnf("ai-credits shadow authorize failed: %s %s", code, message)
				}
			}
		}
	}
	ctx.SetContext(ctxCanonicalModel, canonical)

	var maxOut *int64
	if raw := gjson.GetBytes(body, "max_tokens"); raw.Exists() {
		value := raw.Int()
		maxOut = &value
	} else if raw := gjson.GetBytes(body, "max_output_tokens"); raw.Exists() {
		value := raw.Int()
		maxOut = &value
	}

	// F6: the request's own bytes carry the provable input ceiling. A request
	// that references content the bytes cannot bound is refused here rather
	// than admitted with a zero (or invented) estimate -- the reservation IS
	// the strict cap, so an unbounded input makes the cap meaningless.
	bound := computeInputBound(body, config.UnboundedReferenceTokens)
	if !bound.Provable {
		if config.Mode == ModeEnforce {
			return denyLocal(statusForCode(errUnboundedEstimate), errUnboundedEstimate,
				"request input cannot be bounded: "+bound.Reason)
		}
		log.Warnf("ai-credits shadow unbounded input: %s", bound.Reason)
	}

	req := AdmitRequest{
		ContractVersion:      CreditsContractVersion,
		RequestID:            stringFromCtx(ctx, ctxRequestID),
		AttemptID:            stringFromCtx(ctx, ctxAttemptID),
		Consumer:             consumer,
		EntryPath:            path,
		EntryProtocol:        protocolErrorType(path),
		Endpoint:             endpoint,
		RequestedModel:       modelValue,
		CanonicalModel:       canonical,
		UpstreamModel:        upstream,
		GroupKey:             groupKey,
		Stream:               stream,
		MaxOutputTokens:      maxOut,
		EstimatedInputTokens: bound.TokensUpperBound,
	}
	if config.Registry != nil {
		req.ConfigVersion = config.Registry.ConfigVersion
	}

	headers := [][2]string{
		{"content-type", "application/json"},
		{config.AuthHeader, config.AuthKey},
		{"x-ai-credits-contract", CreditsContractVersion},
	}
	err := config.Client.Call("POST", config.AdmitPath, headers, marshalJSON(req),
		func(statusCode int, _ http.Header, responseBody []byte) {
			handleAdmit(ctx, config, body, statusCode, responseBody)
		}, config.Timeout)
	if err != nil {
		log.Errorf("ai-credits admit call failed: %v", err)
		if config.Mode == ModeEnforce {
			_ = denyLocal(503, "credits_unavailable", "credits admission service is unavailable")
			return types.ActionContinue
		}
		return types.ActionContinue
	}
	return types.ActionPause
}

func handleAdmit(ctx wrapper.HttpContext, config CreditsConfig, body []byte, statusCode int, responseBody []byte) {
	parsed, err := parseAdmitResponse(responseBody)
	if err != nil {
		log.Errorf("ai-credits admit response decode failed: %v", err)
		if config.Mode == ModeEnforce {
			_ = denyLocal(503, "credits_unavailable", "credits admission service returned an invalid response")
			return
		}
		log.Warnf("ai-credits shadow admit decode failed status=%d", statusCode)
		proxywasm.ResumeHttpRequest()
		return
	}
	allowed := statusCode/100 == 2 && parsed.Decision == DecisionAllow
	if !allowed {
		if config.Mode == ModeEnforce {
			code := "credits_unavailable"
			message := "credits admission service is unavailable"
			status := uint32(503)
			if parsed.Decision == DecisionDeny {
				code = "insufficient_credits"
				message = "request denied by credits admission"
				status = 402
				if statusCode >= 400 {
					status = uint32(statusCode)
				}
			} else if statusCode >= 400 {
				status = uint32(statusCode)
			}
			if parsed.Error != nil {
				if parsed.Error.Code != "" {
					code = parsed.Error.Code
				}
				if parsed.Error.Message != "" {
					message = parsed.Error.Message
				}
			}
			_ = denyLocal(status, code, message)
			return
		}
		log.Warnf("ai-credits shadow admit denied status=%d body=%s", statusCode, string(responseBody))
		proxywasm.ResumeHttpRequest()
		return
	}
	if config.Mode == ModeEnforce && parsed.StrictCap && (parsed.MaxOutputTokens == nil || *parsed.MaxOutputTokens < 0) {
		_ = denyLocal(503, "credits_unavailable", "strict cap requires a max output bound")
		return
	}
	ctx.SetContext(ctxAdmitted, true)
	ctx.SetContext(ctxReservationID, parsed.ReservationID)
	if parsed.CanonicalModel != "" {
		ctx.SetContext(ctxCanonicalModel, parsed.CanonicalModel)
	}
	if parsed.MaxOutputTokens != nil {
		clamped, err := clampOutputTokens(body, *parsed.MaxOutputTokens)
		if err != nil {
			log.Errorf("failed to clamp max tokens: %v", err)
			if config.Mode == ModeEnforce && parsed.StrictCap {
				_ = denyLocal(503, "credits_unavailable", "failed to apply output bound")
				return
			}
		} else if clamped != nil {
			_ = proxywasm.ReplaceHttpRequestBody(clamped)
		}
	}
	proxywasm.ResumeHttpRequest()
}

func onHttpResponseHeaders(ctx wrapper.HttpContext, config CreditsConfig) types.Action {
	if ctx.GetBoolContext(ctxSkip, false) || config.Mode == ModeOff {
		return types.ActionContinue
	}
	status := 0
	if raw, err := proxywasm.GetHttpResponseHeader(":status"); err == nil {
		status, _ = strconv.Atoi(raw)
	}
	ctx.SetContext(ctxHTTPStatus, status)
	return types.ActionContinue
}

func onHttpStreamingResponseBody(ctx wrapper.HttpContext, config CreditsConfig, data []byte, endOfStream bool) []byte {
	if ctx.GetBoolContext(ctxSkip, false) || config.Mode == ModeOff {
		return data
	}
	if !ctx.GetBoolContext(ctxAdmitted, false) && config.Mode == ModeEnforce {
		return data
	}
	accumulateUsage(ctx, data)
	// A provider may place its semantic terminal marker in the same Envoy
	// callback that carries transport EOF. Inspect every callback; the
	// settlement reason still distinguishes transport EOF from a semantic end.
	semantic := isSemanticStreamEnd(ctx, data)
	// Usage is complete only at a response boundary. OpenAI-compatible
	// providers may emit finish_reason and usage in separate chunks, and a
	// client can disconnect after the usage chunk but before [DONE]. A usage
	// object by itself is therefore only evidence seen; the final HTTP chunk or
	// an explicit protocol terminal marker closes the stream. Anthropic's
	// message_stop and OpenAI [DONE]/response.completed are handled by
	// isSemanticStreamEnd.
	if ctx.GetBoolContext(ctxUsageSeen, false) && (endOfStream || semantic) {
		ctx.SetContext(ctxUsageComplete, true)
	}
	if !endOfStream && !semantic {
		return data
	}
	settleOnce(ctx, config, endOfStream, semantic)
	return data
}

func settleOnce(ctx wrapper.HttpContext, config CreditsConfig, endOfStream, semantic bool) {
	if ctx.GetBoolContext(ctxSettled, false) {
		return
	}
	ctx.SetContext(ctxSettled, true)
	usage := snapshotUsage(ctx)
	httpStatus, _ := ctx.GetContext(ctxHTTPStatus).(int)
	status, reason := settleReason(endOfStream, semantic, usage, httpStatus)
	if !endOfStream && !semantic && !usage.Complete {
		status = SettlePendingVerify
		reason = ReasonDisconnect
	}
	req := settleRequest(ctx, status, reason, usage, httpStatus)
	ctx.SetContext(ctxSettleRequest, req)
	ctx.SetContext(ctxOutboxDurable, false)
	outboxRaw := marshalJSON(OutboxEntry{
		ContractVersion:  CreditsContractVersion,
		EnqueuedAtUnixMs: time.Now().UnixMilli(),
		Cause:            OutboxCauseDispatch,
		Settle:           json.RawMessage(marshalJSON(req)),
	})
	// Store the exact bytes before dispatch: a synchronous HSET submission
	// failure may invoke the callback before outboxRecordWithRaw returns.
	ctx.SetContext(ctxOutboxRaw, string(outboxRaw))
	// HSet is asynchronous. Do not treat dispatching that Redis call as durable:
	// wait for Redis' response before sending the usage-bearing settlement. If
	// the write cannot be confirmed, try the direct path while retaining the
	// exact payload in the log; a healthy backend can still account for it.
	lifecycle := &outboxLifecycle{live: true}
	ctx.SetContext(ctxOutboxLifecycle, lifecycle)
	if outboxRecordWithRawLifecycle(config, req, OutboxCauseDispatch, outboxRaw, func(err error) {
		if err != nil {
			// Redis/WAITAOF failures stay in the verifier for retry. Sending
			// the usage-bearing request before the write barrier succeeds would
			// make the durable-before-settlement invariant false.
			log.Errorf("ai-credits settle outbox durability failed reservation=%s error=%v", req.ReservationID, err)
			return
		}
		if !lifecycle.live {
			// The HTTP context is gone. Continue from the root callback so the
			// backend settlement and compare-delete are not tied to a destroyed
			// stream context.
			sendSettleDetached(config, req, string(outboxRaw), 0)
			return
		}
		ctx.SetContext(ctxOutboxDurable, true)
		sendSettle(ctx, config, req, 0)
	}) {
		return
	}
	sendSettle(ctx, config, req, 0)
}

func settleRequest(ctx wrapper.HttpContext, status, reason string, usage UsagePayload, httpStatus int) SettleRequest {
	return SettleRequest{
		ContractVersion: CreditsContractVersion,
		RequestID:       stringFromCtx(ctx, ctxRequestID),
		AttemptID:       stringFromCtx(ctx, ctxAttemptID),
		ReservationID:   stringFromCtx(ctx, ctxReservationID),
		Consumer:        stringFromCtx(ctx, ctxConsumer),
		CanonicalModel:  stringFromCtx(ctx, ctxCanonicalModel),
		Status:          status,
		HTTPStatus:      httpStatus,
		Stream:          ctx.GetBoolContext(ctxStream, false),
		Usage:           usage,
		Reason:          reason,
	}
}

// onHttpStreamDone is the last moment this request exists.
//
// It runs in the log phase, where an HTTP callout would be dispatched into a
// context that is about to be destroyed, so it does not try to settle over
// HTTP. It answers one question instead: did this request end without ever
// building a settlement? A client that hangs up mid-stream produces exactly
// that -- no terminal chunk, no semantic end, no settle -- and the reservation
// would otherwise sit untouched until the recovery sweep found it. Recording it
// here gives the replay path the request's real usage instead of leaving the
// backend to quarantine an empty reservation.
func onHttpStreamDone(ctx wrapper.HttpContext, config CreditsConfig) {
	if config.Mode == ModeOff || ctx.GetBoolContext(ctxSkip, false) {
		return
	}
	if !ctx.GetBoolContext(ctxAdmitted, false) {
		return
	}
	if lifecycle, ok := ctx.GetContext(ctxOutboxLifecycle).(*outboxLifecycle); ok && lifecycle != nil {
		lifecycle.live = false
	}
	if _, dispatched := ctx.GetContext(ctxSettleRequest).(SettleRequest); dispatched {
		// A settlement was built and its outbox write was attempted. A callback
		// may still be outstanding: scheduling HSET is not evidence that the
		// bytes reached Redis, and a worker can be torn down before either the
		// HSET or WAITAOF callback runs. Keep the exact wrapper in the log as a
		// second recovery witness until both acknowledgements are observed. The
		// Redis record, when it did arrive, remains available to the replay job.
		if config.Outbox != nil && !ctx.GetBoolContext(ctxOutboxDurable, false) {
			log.Errorf("ai-credits settle outbox durability unconfirmed reservation=%s payload=%s",
				stringFromCtx(ctx, ctxReservationID), stringFromCtx(ctx, ctxOutboxRaw))
		}
		return
	}
	usage := snapshotUsage(ctx)
	httpStatus, _ := ctx.GetContext(ctxHTTPStatus).(int)
	status, reason := settleReason(false, false, usage, httpStatus)
	req := settleRequest(ctx, status, reason, usage, httpStatus)
	outboxRaw := marshalJSON(OutboxEntry{
		ContractVersion:  CreditsContractVersion,
		EnqueuedAtUnixMs: time.Now().UnixMilli(),
		Cause:            OutboxCauseDisconnect,
		Settle:           json.RawMessage(marshalJSON(req)),
	})
	if !outboxRecordWithRawLifecycle(config, req, OutboxCauseDisconnect, outboxRaw, func(err error) {
		if err != nil {
			log.Errorf("ai-credits disconnect outbox durability failed reservation=%s error=%v", req.ReservationID, err)
			return
		}
		sendSettleDetached(config, req, string(outboxRaw), 0)
	}) {
		// There is no HTTP callback at stream teardown. In log-only/degraded mode
		// this exact body is the recovery evidence; a reservation/status summary
		// cannot reconstruct usage for an operator later.
		log.Errorf("ai-credits stream ended without a durable outbox reservation=%s status=%s reason=%s payload=%s",
			req.ReservationID, req.Status, req.Reason, string(marshalJSON(req)))
		return
	}
	log.Warnf("ai-credits stream ended without a settlement reservation=%s status=%s reason=%s",
		req.ReservationID, req.Status, req.Reason)
}

// sendSettle delivers one settlement, retrying a bounded number of times.
//
// Settlement is the only message that carries the request's real cost, and the
// backend cannot reconstruct it: the reservation row knows the ceiling, not
// what was actually spent. So a non-2xx answer is retried on the same
// idempotent (requestId, attemptId, reservationId) payload rather than logged
// and dropped.
//
// When every attempt fails the payload is written to the gateway log as one
// line of JSON. That log is collected, so the usage survives the pod: an
// operator (or the backend's recovery sweep, which sees the row as unsettled)
// has the exact settle body to replay. The settled flag is cleared at the same
// time, so any later hook on this request retries instead of assuming the
// settlement landed.
func sendSettle(ctx wrapper.HttpContext, config CreditsConfig, req SettleRequest, attempt int) {
	headers := [][2]string{
		{"content-type", "application/json"},
		{config.AuthHeader, config.AuthKey},
		{"x-ai-credits-contract", CreditsContractVersion},
	}
	payload := marshalJSON(req)
	err := config.Client.Call("POST", config.SettlePath, headers, payload,
		func(statusCode int, _ http.Header, responseBody []byte) {
			if statusCode/100 == 2 {
				// The ledger has the settlement. Nothing else proves that, so
				// this is the only place the durable record is dropped.
				ctx.SetContext(ctxSettleAcked, true)
				outboxForget(config, req.ReservationID, stringFromCtx(ctx, ctxOutboxRaw))
				return
			}
			log.Errorf("ai-credits settle failed status=%d attempt=%d body=%s",
				statusCode, attempt, string(responseBody))
			// 4xx other than 429 is a contract rejection: retrying an
			// identical payload cannot change the answer, so it goes straight
			// to the durable log.
			retryable := statusCode >= 500 || statusCode == 429 || statusCode == 408
			if retryable && attempt+1 < config.SettleRetries {
				sendSettle(ctx, config, req, attempt+1)
				return
			}
			reportLostSettlement(ctx, config, req, payload, statusCode)
		}, config.Timeout)
	if err != nil {
		log.Errorf("ai-credits settle call failed attempt=%d: %v", attempt, err)
		if attempt+1 < config.SettleRetries {
			sendSettle(ctx, config, req, attempt+1)
			return
		}
		reportLostSettlement(ctx, config, req, payload, 0)
	}
}

// sendSettleDetached queues a settlement for delivery from the root
// PluginContext after the originating HTTP stream has ended. It is used only
// after the Redis write-barrier pair has confirmed local AOF. A failed
// detached delivery stays queued for bounded root-tick retries while the
// exact outbox value remains available to the replay worker; no request
// context is retained and no ledger outcome is guessed.
func sendSettleDetached(config CreditsConfig, req SettleRequest, raw string, attempt int) {
	enqueueDetachedSettlement(config, req, raw, attempt)
}

// reportLostSettlement records an undelivered settlement and reopens the
// request for another attempt.
//
// When both outbox callbacks succeeded, the outbox already holds this payload
// and the durable channel needs nothing here. If HSET or WAITAOF failed, that
// assumption is false: the exact payload stays in this log line so an operator
// can recover it even when the direct attempt also failed.
func reportLostSettlement(ctx wrapper.HttpContext, config CreditsConfig, req SettleRequest, payload []byte, statusCode int) {
	durable := ctx.GetBoolContext(ctxOutboxDurable, false)
	if config.Outbox == nil || !durable {
		log.Errorf("ai-credits settle unrecoverable reservation=%s status=%d outboxDurable=%t payload=%s",
			req.ReservationID, statusCode, durable, string(payload))
	} else {
		log.Errorf("ai-credits settle undelivered, held in the outbox reservation=%s status=%d",
			req.ReservationID, statusCode)
	}
	ctx.SetContext(ctxSettled, false)
}

func stripCreditsClientHeaders() {
	for _, name := range []string{
		"x-ai-credits-budget-subject",
		"x-ai-credits-reservation-id",
		"x-ai-credits-request-id",
		"x-ai-credits-canonical-model",
		DefaultAuthHeader,
		strings.ToLower(DefaultAuthHeader),
	} {
		_ = proxywasm.RemoveHttpRequestHeader(name)
	}
}

func trustedRequestID() string {
	if value, err := proxywasm.GetHttpRequestHeader("x-request-id"); err == nil {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return uuid.NewString()
}

func stringFromCtx(ctx wrapper.HttpContext, key string) string {
	value, _ := ctx.GetContext(key).(string)
	return value
}

func clampOutputTokens(body []byte, max int64) ([]byte, error) {
	if max < 0 {
		return nil, nil
	}
	updated := body
	found := false
	for _, field := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens"} {
		raw := gjson.GetBytes(updated, field)
		if !raw.Exists() {
			continue
		}
		found = true
		if raw.Int() < 0 || raw.Int() > max {
			next, err := sjson.SetBytes(updated, field, max)
			if err != nil {
				return nil, err
			}
			updated = next
		}
	}
	if !found {
		next, err := sjson.SetBytes(updated, "max_tokens", max)
		if err != nil {
			return nil, err
		}
		updated = next
	}
	return updated, nil
}

func statusForCode(code string) uint32 {
	switch code {
	case errUnknownModel, errUnknownGroup:
		return 404
	case errUnauthenticated:
		return 401
	case errUnauthorizedModel:
		return 403
	case errRegistryUnavailable:
		return 503
	case errMissingModel, errPrefixConflict, errUnsupportedEndpoint, errAmbiguousModel,
		errUnboundedEstimate:
		return 400
	default:
		return 400
	}
}

func denyLocal(status uint32, code, message string) types.Action {
	payload, _ := json.Marshal(map[string]interface{}{
		"error": map[string]string{
			"message": message,
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
	headers := [][2]string{{"content-type", "application/json; charset=utf-8"}}
	if err := proxywasm.SendHttpResponseWithDetail(status, pluginName+"."+code, headers, payload, -1); err != nil {
		log.Errorf("failed to send ai-credits deny: %v", err)
	}
	return types.ActionPause
}
