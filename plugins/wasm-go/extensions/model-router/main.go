package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	DefaultMaxBodyBytes = 100 * 1024 * 1024 // 100MB
	AutoModelPrefix     = "higress/auto"
)

func main() {}

func init() {
	wrapper.SetCtx(
		"model-router",
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
		wrapper.WithRebuildMaxMemBytes[ModelRouterConfig](200*1024*1024),
	)
}

// AutoRoutingRule defines a regex-based routing rule for auto model selection
type AutoRoutingRule struct {
	Pattern *regexp.Regexp
	Model   string
}

type ModelRouterConfig struct {
	modelKey              string
	addProviderHeader     string
	modelToHeader         string
	enableOnPathSuffix    []string
	keepOriginalModelName bool
	// Auto routing configuration
	enableAutoRouting bool
	autoRoutingRules  []AutoRoutingRule
	defaultModel      string
	registry          *Registry
}

func parseConfig(json gjson.Result, config *ModelRouterConfig) error {
	config.modelKey = json.Get("modelKey").String()
	if config.modelKey == "" {
		config.modelKey = "model"
	}
	config.addProviderHeader = json.Get("addProviderHeader").String()
	config.modelToHeader = json.Get("modelToHeader").String()
	config.keepOriginalModelName = json.Get("keepOriginalModelName").Bool()

	enableOnPathSuffix := json.Get("enableOnPathSuffix")
	if enableOnPathSuffix.Exists() && enableOnPathSuffix.IsArray() {
		for _, item := range enableOnPathSuffix.Array() {
			config.enableOnPathSuffix = append(config.enableOnPathSuffix, item.String())
		}
	} else {
		// Default suffixes if not provided
		config.enableOnPathSuffix = []string{
			"/completions",
			"/embeddings",
			"/images/generations",
			"/audio/speech",
			"/fine_tuning/jobs",
			"/moderations",
			"/image-synthesis",
			"/video-synthesis",
			"/rerank",
			"/messages",
			"/responses",
		}
	}

	// Parse auto routing configuration
	autoRouting := json.Get("autoRouting")
	if autoRouting.Exists() {
		config.enableAutoRouting = autoRouting.Get("enable").Bool()
		config.defaultModel = autoRouting.Get("defaultModel").String()

		rules := autoRouting.Get("rules")
		if rules.Exists() && rules.IsArray() {
			for _, rule := range rules.Array() {
				patternStr := rule.Get("pattern").String()
				model := rule.Get("model").String()
				if patternStr == "" || model == "" {
					log.Warnf("skipping invalid auto routing rule: pattern=%s, model=%s", patternStr, model)
					continue
				}
				compiled, err := regexp.Compile(patternStr)
				if err != nil {
					log.Warnf("failed to compile regex pattern '%s': %v", patternStr, err)
					continue
				}
				config.autoRoutingRules = append(config.autoRoutingRules, AutoRoutingRule{
					Pattern: compiled,
					Model:   model,
				})
				log.Debugf("loaded auto routing rule: pattern=%s, model=%s", patternStr, model)
			}
		}
	}

	registry, err := parseRegistry(json)
	if err != nil {
		return err
	}
	config.registry = registry

	return nil
}

func stripClientInternalHeaders(config ModelRouterConfig) {
	headers := defaultInternalHeaders
	if config.registry != nil && len(config.registry.StripHeaders) > 0 {
		headers = config.registry.StripHeaders
	}
	seen := map[string]struct{}{}
	for _, name := range headers {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		_ = proxywasm.RemoveHttpRequestHeader(key)
	}
	if config.modelToHeader != "" {
		_ = proxywasm.RemoveHttpRequestHeader(config.modelToHeader)
	}
	if config.addProviderHeader != "" {
		_ = proxywasm.RemoveHttpRequestHeader(config.addProviderHeader)
	}
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config ModelRouterConfig) types.Action {
	stripClientInternalHeaders(config)

	path, err := proxywasm.GetHttpRequestHeader(":path")
	if err != nil {
		return types.ActionContinue
	}

	// Remove query parameters for suffix check
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

	// Prepare for body processing
	proxywasm.RemoveHttpRequestHeader("content-length")
	// 100MB buffer limit
	ctx.SetRequestBodyBufferLimit(DefaultMaxBodyBytes)

	return types.HeaderStopIteration
}

func onHttpRequestBody(ctx wrapper.HttpContext, config ModelRouterConfig, body []byte) types.Action {
	contentType, err := proxywasm.GetHttpRequestHeader("content-type")
	if err != nil {
		return types.ActionContinue
	}

	if strings.Contains(contentType, "application/json") {
		return handleJsonBody(ctx, config, body)
	} else if strings.Contains(contentType, "multipart/form-data") {
		return handleMultipartBody(ctx, config, body, contentType)
	}

	return types.ActionContinue
}

// extractLastUserMessage extracts the content of the last message with role "user" from the messages array
func extractLastUserMessage(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}

	var lastUserContent string
	for _, msg := range messages.Array() {
		if msg.Get("role").String() == "user" {
			content := msg.Get("content")
			if content.IsArray() {
				// Handle array content (e.g., multimodal messages with text and images)
				for _, item := range content.Array() {
					if item.Get("type").String() == "text" {
						lastUserContent = item.Get("text").String()
					}
				}
			} else {
				lastUserContent = content.String()
			}
		}
	}
	return lastUserContent
}

// matchAutoRoutingRule matches the user message against auto routing rules and returns the matched model
func matchAutoRoutingRule(config ModelRouterConfig, userMessage string) (string, bool) {
	for _, rule := range config.autoRoutingRules {
		if rule.Pattern.MatchString(userMessage) {
			log.Debugf("auto routing rule matched: pattern=%s, model=%s", rule.Pattern.String(), rule.Model)
			return rule.Model, true
		}
	}
	return "", false
}

func handleJsonBody(ctx wrapper.HttpContext, config ModelRouterConfig, body []byte) types.Action {
	if !json.Valid(body) {
		log.Error("invalid json body")
		return types.ActionContinue
	}
	modelValue := gjson.GetBytes(body, config.modelKey).String()
	if modelValue == "" {
		if config.registry != nil && config.registry.Enabled {
			return rejectModelRoute(ctx, errMissingModel, "request is missing model")
		}
		return types.ActionContinue
	}

	// Check if auto routing should be triggered
	if config.enableAutoRouting && modelValue == AutoModelPrefix {
		userMessage := extractLastUserMessage(body)
		var targetModel string
		if userMessage != "" {
			if matchedModel, found := matchAutoRoutingRule(config, userMessage); found {
				targetModel = matchedModel
				log.Infof("auto routing: user message matched, routing to model: %s", matchedModel)
			}
		}
		// No rule matched, use default model if configured
		if targetModel == "" && config.defaultModel != "" {
			targetModel = config.defaultModel
			log.Infof("auto routing: no rule matched, using default model: %s", config.defaultModel)
		}

		if targetModel != "" {
			modelValue = targetModel
			newBody, err := sjson.SetBytes(body, config.modelKey, targetModel)
			if err != nil {
				log.Errorf("failed to update model in auto routing json body: %v", err)
				return types.ActionContinue
			}
			body = newBody
			_ = proxywasm.ReplaceHttpRequestBody(newBody)
			log.Debugf("auto routing: updated body model field to: %s", targetModel)
			if config.registry == nil || !config.registry.Enabled {
				_ = proxywasm.ReplaceHttpRequestHeader("x-higress-llm-model", targetModel)
				return types.ActionContinue
			}
		} else {
			log.Warnf("auto routing: no rule matched and no default model configured")
			return types.ActionContinue
		}
	}

	if config.registry != nil && config.registry.Enabled {
		if config.registry.publicationBlocked() {
			return rejectModelRoute(ctx, errRegistryUnavailable, "unified registry has no published models")
		}
		return applyPublishedModelRoute(ctx, config, body, modelValue)
	}

	if config.modelToHeader != "" {
		_ = proxywasm.ReplaceHttpRequestHeader(config.modelToHeader, modelValue)
	}

	if config.addProviderHeader != "" {
		parts := strings.SplitN(modelValue, "/", 2)
		if len(parts) == 2 {
			provider := parts[0]
			model := parts[1]
			_ = proxywasm.ReplaceHttpRequestHeader(config.addProviderHeader, provider)

			if !config.keepOriginalModelName {
				newBody, err := sjson.SetBytes(body, config.modelKey, model)
				if err != nil {
					log.Errorf("failed to update model in json body: %v", err)
					return types.ActionContinue
				}
				_ = proxywasm.ReplaceHttpRequestBody(newBody)
			}
			log.Debugf("model route to provider: %s, model: %s", provider, model)
		} else {
			log.Debugf("model route to provider not work, model: %s", modelValue)
		}
	}

	return types.ActionContinue
}

func handleMultipartBody(ctx wrapper.HttpContext, config ModelRouterConfig, body []byte, contentType string) types.Action {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		log.Errorf("failed to parse content type: %v", err)
		return types.ActionContinue
	}
	boundary, ok := params["boundary"]
	if !ok {
		log.Errorf("no boundary in content type")
		return types.ActionContinue
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var newBody bytes.Buffer
	writer := multipart.NewWriter(&newBody)
	writer.SetBoundary(boundary)

	modified := false

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Errorf("failed to read multipart part: %v", err)
			return types.ActionContinue
		}

		// Read part content
		partContent, err := io.ReadAll(part)
		if err != nil {
			log.Errorf("failed to read part content: %v", err)
			return types.ActionContinue
		}

		formName := part.FormName()
		if formName == config.modelKey {
			modelValue := string(partContent)

			if config.registry != nil && config.registry.Enabled {
				path, _ := proxywasm.GetHttpRequestHeader(":path")
				resolved := config.registry.Resolve(path, modelValue)
				if !resolved.OK {
					return rejectModelRoute(ctx, resolved.ErrorCode, resolved.ErrorMessage)
				}
				applyResolvedHeaders(config, resolved.Entry)
				if !config.keepOriginalModelName {
					partContent = []byte(resolved.Entry.UpstreamModel)
					modified = true
				}
			} else if config.modelToHeader != "" {
				_ = proxywasm.ReplaceHttpRequestHeader(config.modelToHeader, modelValue)
			}

			if (config.registry == nil || !config.registry.Enabled) && config.addProviderHeader != "" {
				parts := strings.SplitN(modelValue, "/", 2)
				if len(parts) == 2 {
					provider := parts[0]
					model := parts[1]
					_ = proxywasm.ReplaceHttpRequestHeader(config.addProviderHeader, provider)

					if !config.keepOriginalModelName {
						// Write modified part
						h := make(http.Header)
						for k, v := range part.Header {
							h[k] = v
						}

						pw, err := writer.CreatePart(textproto.MIMEHeader(h))
						if err != nil {
							log.Errorf("failed to create part: %v", err)
							return types.ActionContinue
						}
						_, err = pw.Write([]byte(model))
						if err != nil {
							log.Errorf("failed to write part content: %v", err)
							return types.ActionContinue
						}
						modified = true
						log.Debugf("model route to provider: %s, model: %s", provider, model)
						continue
					}
					log.Debugf("model route to provider: %s, model kept: %s", provider, modelValue)
				} else {
					log.Debugf("model route to provider not work, model: %s", modelValue)
				}
			}
		}

		// Write original part
		h := make(http.Header)
		for k, v := range part.Header {
			h[k] = v
		}
		pw, err := writer.CreatePart(textproto.MIMEHeader(h))
		if err != nil {
			log.Errorf("failed to create part: %v", err)
			return types.ActionContinue
		}
		_, err = pw.Write(partContent)
		if err != nil {
			log.Errorf("failed to write part content: %v", err)
			return types.ActionContinue
		}
	}

	writer.Close()

	if modified {
		_ = proxywasm.ReplaceHttpRequestBody(newBody.Bytes())
	}

	return types.ActionContinue
}

func applyPublishedModelRoute(ctx wrapper.HttpContext, config ModelRouterConfig, body []byte, modelValue string) types.Action {
	path, _ := proxywasm.GetHttpRequestHeader(":path")
	resolved := config.registry.Resolve(path, modelValue)
	if !resolved.OK {
		return rejectModelRoute(ctx, resolved.ErrorCode, resolved.ErrorMessage)
	}
	// This plugin runs ahead of key-auth in the AUTHN phase (priority 900
	// against key-auth's 310), because the route a request lands on is the one
	// this plugin selects and key-auth's allow list is per-route. That ordering
	// means `x-mse-consumer` is set only when some earlier filter already
	// authenticated the caller. Enforcing model authorization against an absent
	// consumer would reject every request, so the check applies only when the
	// caller is already known; the same registry is published to ai-credits,
	// which runs after authentication and refuses an unauthorized model there.
	if consumer, _ := proxywasm.GetHttpRequestHeader("x-mse-consumer"); strings.TrimSpace(consumer) != "" {
		if ok, code, message := config.registry.Authorize(consumer, resolved.Entry.CanonicalID); !ok {
			return rejectModelRoute(ctx, code, message)
		}
	}
	applyResolvedHeaders(config, resolved.Entry)
	if !config.keepOriginalModelName && resolved.Entry.UpstreamModel != modelValue {
		newBody, err := sjson.SetBytes(body, config.modelKey, resolved.Entry.UpstreamModel)
		if err != nil {
			log.Errorf("failed to update model in published json body: %v", err)
			return rejectModelRoute(ctx, errUnknownModel, "failed to apply published model mapping")
		}
		_ = proxywasm.ReplaceHttpRequestBody(newBody)
	}
	_ = proxywasm.ReplaceHttpRequestHeader("x-ai-credits-canonical-model", resolved.Entry.CanonicalID)
	return types.ActionContinue
}

func applyResolvedHeaders(config ModelRouterConfig, entry *ModelEntry) {
	routeHeader := config.modelToHeader
	if routeHeader == "" {
		routeHeader = "x-higress-llm-model"
	}
	_ = proxywasm.ReplaceHttpRequestHeader(routeHeader, entry.RouteValue)
	if config.addProviderHeader != "" {
		provider := entry.ProviderValue
		if provider == "" {
			provider = entry.GroupKey
		}
		_ = proxywasm.ReplaceHttpRequestHeader(config.addProviderHeader, provider)
	}
}

func rejectModelRoute(ctx wrapper.HttpContext, code, message string) types.Action {
	path, _ := proxywasm.GetHttpRequestHeader(":path")
	status := uint32(400)
	switch code {
	case errUnknownModel, errUnknownGroup:
		status = 404
	case errUnauthenticated:
		status = 401
	case errUnauthorizedModel:
		status = 403
	case errRegistryUnavailable:
		status = 503
	case errUnsupportedEndpoint:
		status = 400
	case errPrefixConflict, errAmbiguousModel, errMissingModel:
		status = 400
	}
	var payload []byte
	if protocolErrorType(path) == "anthropic" {
		payload, _ = json.Marshal(map[string]interface{}{
			"type": "error",
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": message,
				"code":    code,
			},
		})
	} else {
		payload, _ = json.Marshal(map[string]interface{}{
			"error": map[string]string{
				"message": message,
				"type":    "invalid_request_error",
				"code":    code,
			},
		})
	}
	headers := [][2]string{{"content-type", "application/json; charset=utf-8"}}
	if err := proxywasm.SendHttpResponseWithDetail(status, "model-router."+code, headers, payload, -1); err != nil {
		log.Errorf("failed to send model-router error: %v", err)
	}
	return types.ActionPause
}
