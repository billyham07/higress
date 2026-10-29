// Package main implements ai-input-guard: cheap early rejection of AI requests
// that can never be served, before they are buffered by later plugins or reach
// the model servers.
//
// Two checks, both far cheaper than tokenizing:
//   - body size: Content-Length above maxBodyBytes is rejected in the header
//     phase, before Envoy buffers anything.
//   - input size: a lower bound on prompt tokens derived from character counts
//     (see estimate.go). If even the lower bound exceeds the model's context
//     window, the backend would reject the request after tokenizing it, so it
//     is rejected here instead.
package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

const (
	// Matches the documented Anthropic Messages API limit; over 14 days of
	// production traffic the largest legitimate body was 19 MB.
	DefaultMaxBodyBytes = 32 * 1024 * 1024
	// Largest context window in the fleet (GLM / DeepSeek / Qwen self-hosted).
	DefaultMaxContextTokens = 1048576
	// Buffer limit in dry-run mode, matching ai-proxy, so that observing never
	// turns into an Envoy 413 of its own.
	dryRunBufferLimit = 100 * 1024 * 1024
)

func main() {}

func init() {
	wrapper.SetCtx(
		"ai-input-guard",
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
	)
}

type InputGuardConfig struct {
	maxBodyBytes            int
	defaultMaxContextTokens int
	modelMaxContextTokens   map[string]int
	enableOnPathSuffix      []string
	// dryRun records would-be rejections in ai_log and lets the request through.
	dryRun bool
	// Bodies at or below this size cannot exceed any configured context window,
	// so they are never read.
	skipBodyBelow int
}

func parseConfig(json gjson.Result, config *InputGuardConfig) error {
	config.maxBodyBytes = DefaultMaxBodyBytes
	if v := json.Get("maxBodyBytes"); v.Exists() {
		config.maxBodyBytes = int(v.Int())
	}
	config.defaultMaxContextTokens = DefaultMaxContextTokens
	if v := json.Get("defaultMaxContextTokens"); v.Exists() {
		config.defaultMaxContextTokens = int(v.Int())
	}
	config.dryRun = json.Get("dryRun").Bool()
	if config.maxBodyBytes < 0 || config.defaultMaxContextTokens < 0 {
		return fmt.Errorf("maxBodyBytes and defaultMaxContextTokens must be non-negative")
	}

	config.modelMaxContextTokens = map[string]int{}
	var bad error
	json.Get("modelMaxContextTokens").ForEach(func(key, value gjson.Result) bool {
		if value.Int() < 0 {
			bad = fmt.Errorf("modelMaxContextTokens[%s] must be non-negative", key.String())
			return false
		}
		config.modelMaxContextTokens[key.String()] = int(value.Int())
		return true
	})
	if bad != nil {
		return bad
	}

	enableOnPathSuffix := json.Get("enableOnPathSuffix")
	if enableOnPathSuffix.Exists() && enableOnPathSuffix.IsArray() {
		for _, item := range enableOnPathSuffix.Array() {
			config.enableOnPathSuffix = append(config.enableOnPathSuffix, item.String())
		}
	} else {
		config.enableOnPathSuffix = []string{
			"/completions",
			"/embeddings",
			"/rerank",
			"/messages",
			"/responses",
		}
	}

	minContext := config.defaultMaxContextTokens
	for _, v := range config.modelMaxContextTokens {
		if v > 0 && (minContext == 0 || v < minContext) {
			minContext = v
		}
	}
	if minContext == 0 {
		// Text check disabled everywhere: only the header check runs.
		config.skipBodyBelow = -1
	} else {
		config.skipBodyBelow = minContext * minCharsPerToken
	}
	return nil
}

func (c *InputGuardConfig) contextFor(model string) int {
	if v, ok := c.modelMaxContextTokens[model]; ok {
		return v
	}
	return c.defaultMaxContextTokens
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config InputGuardConfig) types.Action {
	path, err := proxywasm.GetHttpRequestHeader(":path")
	if err != nil {
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}
	enable := false
	for _, suffix := range config.enableOnPathSuffix {
		if suffix == "*" || strings.HasSuffix(path, suffix) {
			enable = true
			break
		}
	}
	if !enable || !ctx.HasRequestBody() {
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	ctx.SetContext("anthropic", strings.HasSuffix(path, "/messages"))

	length := -1
	if v, _ := proxywasm.GetHttpRequestHeader("content-length"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			length = n
		}
	}
	if config.maxBodyBytes > 0 && length > config.maxBodyBytes {
		ctx.DontReadRequestBody()
		return rejectBodyTooLarge(ctx, config, length)
	}

	// Embedding and rerank inputs are batches limited per item, so their sum
	// is not comparable to one context window; only the size cap applies.
	batch := strings.HasSuffix(path, "/embeddings") || strings.HasSuffix(path, "/rerank")
	contentType, _ := proxywasm.GetHttpRequestHeader("content-type")
	if batch || config.skipBodyBelow < 0 || !strings.Contains(contentType, "application/json") ||
		(length >= 0 && length <= config.skipBodyBelow) {
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}

	// Chunked bodies have no Content-Length; the buffer limit makes Envoy
	// reply 413 itself once they grow past maxBodyBytes.
	if config.dryRun {
		ctx.SetRequestBodyBufferLimit(dryRunBufferLimit)
	} else if config.maxBodyBytes > 0 {
		ctx.SetRequestBodyBufferLimit(uint32(config.maxBodyBytes))
	}
	return types.HeaderStopIteration
}

func onHttpRequestBody(ctx wrapper.HttpContext, config InputGuardConfig, body []byte) types.Action {
	if config.maxBodyBytes > 0 && len(body) > config.maxBodyBytes {
		return rejectBodyTooLarge(ctx, config, len(body))
	}
	model := gjson.GetBytes(body, "model").String()
	limit := config.contextFor(model)
	if limit <= 0 || len(body) <= limit*minCharsPerToken {
		return types.ActionContinue
	}
	estimated, over := estimateInputTokens(body, limit)
	if !over {
		log.Debugf("input estimate %d within %d for model %q (%d bytes)", estimated, limit, model, len(body))
		return types.ActionContinue
	}
	if !record(ctx, config, "context_length_exceeded", estimated,
		fmt.Sprintf("model=%q bytes=%d estimated_tokens>=%d limit=%d", model, len(body), estimated, limit)) {
		return types.ActionContinue
	}
	message := fmt.Sprintf("This model's maximum context length is %d tokens, but the request input is at least %d tokens. "+
		"Please reduce the length of the messages.", limit, estimated)
	sendError(ctx, http.StatusBadRequest, "ai-input-guard.context_length_exceeded",
		"invalid_request_error", "context_length_exceeded", message)
	return types.ActionContinue
}

func rejectBodyTooLarge(ctx wrapper.HttpContext, config InputGuardConfig, length int) types.Action {
	if !record(ctx, config, "body_too_large", 0,
		fmt.Sprintf("body %d bytes exceeds %d", length, config.maxBodyBytes)) {
		return types.ActionContinue
	}
	message := fmt.Sprintf("Request body is %d bytes, exceeding the maximum of %d bytes.", length, config.maxBodyBytes)
	sendError(ctx, http.StatusRequestEntityTooLarge, "ai-input-guard.body_too_large",
		"request_too_large", "request_too_large", message)
	return types.ActionContinue
}

// record writes the verdict to ai_log (ai_loginput_guard in SLS) and reports
// whether the request should be rejected. In dry-run mode the value carries a
// "dry_run:" prefix and the request goes on; ai-statistics later merges its own
// fields into the same map, so the consumer shows up next to the verdict.
func record(ctx wrapper.HttpContext, config InputGuardConfig, reason string, tokens int, detail string) bool {
	verdict := reason
	if config.dryRun {
		verdict = "dry_run:" + reason
	}
	attrs := map[string]interface{}{"input_guard": verdict}
	if tokens > 0 {
		attrs["input_guard_tokens"] = tokens
	}
	// Replace the map rather than add to it: the wrapper merges the whole map
	// into ai_log, and this plugin must not write anyone else's fields.
	ctx.SetUserAttributeMap(attrs)
	if err := ctx.WriteUserAttributeToLogWithKey(wrapper.AILogKey); err != nil {
		log.Warnf("write ai_log failed: %v", err)
	}
	if config.dryRun {
		log.Warnf("dry-run, would reject: %s", detail)
		return false
	}
	log.Warnf("rejected: %s", detail)
	return true
}

// sendError replies in the error shape of the protocol the client speaks, so
// OpenAI and Anthropic SDKs both surface the message instead of a parse error.
func sendError(ctx wrapper.HttpContext, status int, detail, errType, code, message string) {
	var body []byte
	if anthropic, _ := ctx.GetContext("anthropic").(bool); anthropic {
		body = mustJSON(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": errType, "message": message},
		})
	} else {
		body = mustJSON(map[string]any{
			"error": map[string]any{"type": errType, "code": code, "message": message, "param": nil},
		})
	}
	headers := [][2]string{{"content-type", "application/json"}}
	if err := proxywasm.SendHttpResponseWithDetail(uint32(status), detail, headers, body, -1); err != nil {
		log.Errorf("send local response failed: %v", err)
	}
}
