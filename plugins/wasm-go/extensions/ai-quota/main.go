package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/tokenusage"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"

	"github.com/alibaba/higress/plugins/wasm-go/extensions/ai-quota/util"
)

const (
	pluginName             = "ai-quota"
	quotaChargedContextKey = "ai-quota-charged"
	quotaTerminalTailKey   = "ai-quota-terminal-tail"

	streamingContentType = "text/event-stream"
	// The gateway's downstream connection buffer limit (32KiB) also caps a
	// buffered response, so a larger non-streaming body would fail the request
	// unless the encoder limit is raised. Same ceiling as ai-proxy.
	responseBodyBufferLimit uint32 = 100 * 1024 * 1024
)

type ChatMode string

const (
	ChatModeCompletion ChatMode = "completion"
	ChatModeNone       ChatMode = "none"
)

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessResponseHeaders(onHttpResponseHeaders),
		wrapper.ProcessStreamingResponseBody(onHttpStreamingResponseBody),
		wrapper.ProcessResponseBody(onHttpResponseBody),
	)
}

// QuotaConfig is one route's quota gate. The retired per-key counter's
// settings -- redis_key_prefix, admin_consumer and admin_path -- are no longer
// read: the wallet's key names are the contract with the console, and the
// console, not an admin path on the gateway, is what moves a balance.
type QuotaConfig struct {
	redisInfo          RedisInfo `yaml:"redis"`
	EnablePathSuffixes []string  `yaml:"enable_path_suffixes"`
	redisClient        wrapper.RedisClient
}

type RedisInfo struct {
	ServiceName string `required:"true" yaml:"service_name" json:"service_name"`
	ServicePort int    `required:"false" yaml:"service_port" json:"service_port"`
	Username    string `required:"false" yaml:"username" json:"username"`
	Password    string `required:"false" yaml:"password" json:"password"`
	Timeout     int    `required:"false" yaml:"timeout" json:"timeout"`
	Database    int    `required:"false" yaml:"database" json:"database"`
}

func parseConfig(json gjson.Result, config *QuotaConfig) error {
	log.Debugf("parse config()")
	suffixResult := json.Get("enable_path_suffixes")
	if !suffixResult.Exists() {
		config.EnablePathSuffixes = []string{"/v1/chat/completions", "/v1/messages"}
	} else if !suffixResult.IsArray() {
		return errors.New("enable_path_suffixes must be an array")
	} else {
		pathSuffixes := suffixResult.Array()
		config.EnablePathSuffixes = make([]string, 0, len(pathSuffixes))
		for _, suffix := range pathSuffixes {
			suffixStr := strings.TrimSpace(suffix.String())
			if suffixStr == "" {
				continue
			}
			config.EnablePathSuffixes = append(config.EnablePathSuffixes, suffixStr)
		}
	}
	if len(config.EnablePathSuffixes) == 0 {
		return errors.New("enable_path_suffixes must not be empty")
	}
	// Redis
	redisConfig := json.Get("redis")
	if !redisConfig.Exists() {
		return errors.New("missing redis in config")
	}
	serviceName := redisConfig.Get("service_name").String()
	if serviceName == "" {
		return errors.New("redis service name must not be empty")
	}
	servicePort := int(redisConfig.Get("service_port").Int())
	if servicePort == 0 {
		if strings.HasSuffix(serviceName, ".static") {
			// use default logic port which is 80 for static service
			servicePort = 80
		} else {
			servicePort = 6379
		}
	}
	username := redisConfig.Get("username").String()
	password := redisConfig.Get("password").String()
	timeout := int(redisConfig.Get("timeout").Int())
	if timeout == 0 {
		timeout = 1000
	}
	database := int(redisConfig.Get("database").Int())
	config.redisInfo.ServiceName = serviceName
	config.redisInfo.ServicePort = servicePort
	config.redisInfo.Username = username
	config.redisInfo.Password = password
	config.redisInfo.Timeout = timeout
	config.redisInfo.Database = database
	config.redisClient = wrapper.NewRedisClusterClient(wrapper.FQDNCluster{
		FQDN: serviceName,
		Port: int64(servicePort),
	})

	return config.redisClient.Init(username, password, int64(timeout), wrapper.WithDataBase(database))
}

func onHttpRequestHeaders(context wrapper.HttpContext, config QuotaConfig) types.Action {
	context.DisableReroute()
	log.Debugf("onHttpRequestHeaders()")
	consumer, err := proxywasm.GetHttpRequestHeader("x-mse-consumer")
	if err != nil {
		return deniedNoKeyAuthData()
	}
	if consumer == "" {
		return deniedUnauthorizedConsumer()
	}

	rawPath := context.Path()
	path, _ := url.Parse(rawPath)
	chatMode := getOperationMode(path.Path, config.EnablePathSuffixes)
	context.SetContext("chatMode", chatMode)
	context.SetContext("consumer", consumer)
	log.Debugf("chatMode:%s, consumer:%s", chatMode, consumer)
	if chatMode == ChatModeNone {
		return types.ActionContinue
	}

	// The quota gate never needs the request body.
	context.DontReadRequestBody()
	if err := admitQuota(context, config, consumer); err != nil {
		log.Errorf("failed to read quota key for %s: %v", consumer, err)
		deniedNoQuotaAccount()
		return types.ActionContinue
	}
	return types.HeaderStopAllIterationAndWatermark
}

// onHttpResponseHeaders buffers a non-streaming response. Envoy may hand a JSON
// body over in several chunks, and the usage object at its end cannot be parsed
// out of a fragment that does not start at the top-level object, so the request
// would go uncharged. Only an SSE response is parsed chunk by chunk.
func onHttpResponseHeaders(ctx wrapper.HttpContext, _ QuotaConfig) types.Action {
	if chatMode, ok := ctx.GetContext("chatMode").(ChatMode); !ok || chatMode == ChatModeNone {
		ctx.DontReadResponseBody()
		return types.ActionContinue
	}
	contentType, _ := proxywasm.GetHttpResponseHeader("content-type")
	if !strings.Contains(strings.ToLower(contentType), streamingContentType) {
		ctx.BufferResponseBody()
		ctx.SetResponseBodyBufferLimit(responseBodyBufferLimit)
	}
	return types.ActionContinue
}

func onHttpResponseBody(ctx wrapper.HttpContext, config QuotaConfig, body []byte) types.Action {
	recordResponseUsage(ctx, body)
	chargeQuotaOnce(ctx, config)
	return types.ActionContinue
}

func onHttpStreamingResponseBody(ctx wrapper.HttpContext, config QuotaConfig, data []byte, endOfStream bool) []byte {
	if !recordResponseUsage(ctx, data) {
		return data
	}

	// SSE clients often stop reading after a protocol-level terminal frame without
	// waiting for transport EOF. Charge on that semantic end as well as the normal
	// endOfStream callback, with an exactly-once guard.
	if !endOfStream && !isSemanticStreamEnd(ctx, data) {
		return data
	}
	chargeQuotaOnce(ctx, config)
	return data
}

// recordResponseUsage keeps the usage found in data, and reports whether the
// request is one the quota governs.
func recordResponseUsage(ctx wrapper.HttpContext, data []byte) bool {
	if chatMode, ok := ctx.GetContext("chatMode").(ChatMode); !ok || chatMode == ChatModeNone {
		return false
	}
	usage := tokenusage.GetTokenUsage(ctx, data)
	enrichAnthropicStreamingUsage(ctx, data, &usage)
	if usage.TotalToken > 0 {
		ctx.SetContext(tokenusage.CtxKeyInputToken, usage.InputToken)
		ctx.SetContext(tokenusage.CtxKeyOutputToken, usage.OutputToken)
		ctx.SetContext(tokenusage.CtxKeyTotalToken, usage.TotalToken)
		if len(usage.InputTokenDetails) > 0 {
			ctx.SetContext(tokenusage.CtxKeyInputTokenDetails, usage.InputTokenDetails)
		}
	}
	return true
}

func chargeQuotaOnce(ctx wrapper.HttpContext, config QuotaConfig) {
	if ctx.GetBoolContext(quotaChargedContextKey, false) {
		return
	}
	consumer, ok := ctx.GetContext("consumer").(string)
	if !ok || consumer == "" {
		return
	}

	totalToken, ok := getQuotaToken(
		ctx.GetContext(tokenusage.CtxKeyTotalToken),
		ctx.GetContext(tokenusage.CtxKeyInputToken),
		ctx.GetContext(tokenusage.CtxKeyOutputToken),
		ctx.GetContext(tokenusage.CtxKeyInputTokenDetails),
	)
	if !ok {
		return
	}

	log.Debugf("charge consumer:%s, totalToken:%d", consumer, totalToken)
	if err := spendQuota(ctx, config, consumer, totalToken); err != nil {
		log.Errorf("failed to charge consumer %s quota: %v", consumer, err)
		return
	}
	ctx.SetContext(quotaChargedContextKey, true)
}

func getQuotaToken(totalTokenValue any, inputTokenValue any, outputTokenValue any, inputTokenDetailsValue ...any) (int64, bool) {
	if len(inputTokenDetailsValue) > 0 {
		if details, ok := inputTokenDetailsValue[0].(map[string]int64); ok {
			cacheRead := details[tokenusage.InputTokenDetailsKeyAnthropicMessagesUsageCacheReadInputTokens]
			cacheCreation := details[tokenusage.InputTokenDetailsKeyAnthropicMessagesUsageCacheCreationInputTokens]
			if cacheRead > 0 || cacheCreation > 0 {
				inputToken, inputOK := inputTokenValue.(int64)
				outputToken, outputOK := outputTokenValue.(int64)
				if inputOK && outputOK {
					return inputToken + outputToken + cacheRead + cacheCreation, true
				}
			}
		}
	}
	if totalToken, ok := totalTokenValue.(int64); ok && totalToken > 0 {
		return totalToken, true
	}

	inputToken, inputOK := inputTokenValue.(int64)
	outputToken, outputOK := outputTokenValue.(int64)
	if !inputOK || !outputOK {
		return 0, false
	}
	return inputToken + outputToken, true
}

func isSemanticStreamEnd(ctx wrapper.HttpContext, data []byte) bool {
	tail, _ := ctx.GetContext(quotaTerminalTailKey).([]byte)
	probe := make([]byte, 0, len(tail)+len(data))
	probe = append(probe, tail...)
	probe = append(probe, data...)

	const terminalTailBytes = 512
	if len(probe) > terminalTailBytes {
		tail = append([]byte(nil), probe[len(probe)-terminalTailBytes:]...)
	} else {
		tail = append([]byte(nil), probe...)
	}
	ctx.SetContext(quotaTerminalTailKey, tail)

	if bytes.Contains(probe, []byte("[DONE]")) || bytes.Contains(probe, []byte("message_stop")) {
		return true
	}
	if bytes.Contains(probe, []byte("response.completed")) ||
		bytes.Contains(probe, []byte("response.incomplete")) ||
		bytes.Contains(probe, []byte("response.failed")) {
		_, ok := getQuotaToken(
			ctx.GetContext(tokenusage.CtxKeyTotalToken),
			ctx.GetContext(tokenusage.CtxKeyInputToken),
			ctx.GetContext(tokenusage.CtxKeyOutputToken),
			ctx.GetContext(tokenusage.CtxKeyInputTokenDetails),
		)
		return ok
	}

	usage := wrapper.GetValueFromBody(data, []string{"usage"})
	choices := wrapper.GetValueFromBody(data, []string{"choices"})
	if usage != nil && choices != nil && choices.IsArray() && len(choices.Array()) == 0 {
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
	if previous, ok := ctx.GetContext(tokenusage.CtxKeyInputTokenDetails).(map[string]int64); ok {
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

	cacheRead := usage.InputTokenDetails[tokenusage.InputTokenDetailsKeyAnthropicMessagesUsageCacheReadInputTokens]
	cacheCreation := usage.InputTokenDetails[tokenusage.InputTokenDetailsKeyAnthropicMessagesUsageCacheCreationInputTokens]
	if cacheRead > 0 || cacheCreation > 0 || bytes.Contains(data, []byte(`"message_delta"`)) {
		usage.TotalToken = usage.InputToken + usage.OutputToken + cacheRead + cacheCreation
	}
}

func deniedNoKeyAuthData() types.Action {
	util.SendResponse(http.StatusUnauthorized, "ai-quota.no_key", "text/plain", "Request denied by ai quota check. No Key Authentication information found.")
	return types.ActionContinue
}

func deniedUnauthorizedConsumer() types.Action {
	util.SendResponse(http.StatusForbidden, "ai-quota.unauthorized", "text/plain", "Request denied by ai quota check. Unauthorized consumer.")
	return types.ActionContinue
}

func getOperationMode(path string, pathSuffixes []string) ChatMode {
	for _, suffix := range pathSuffixes {
		if strings.HasSuffix(path, suffix) {
			return ChatModeCompletion
		}
	}
	return ChatModeNone
}
