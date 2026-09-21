package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/tokenusage"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/resp"

	"github.com/alibaba/higress/plugins/wasm-go/extensions/ai-quota/util"
)

const (
	pluginName             = "ai-quota"
	quotaChargedContextKey = "ai-quota-charged"
	quotaTerminalTailKey   = "ai-quota-terminal-tail"
	quotaModelContextKey   = "ai-quota-model"
	// quotaUsageModelContextKey holds the model the response reported, which
	// is the label ai-statistics files its token counters under.
	quotaUsageModelContextKey = "ai-quota-usage-model"
	quotaRouteContextKey      = "ai-quota-route"
	quotaClusterContextKey    = "ai-quota-cluster"
	// creditsLogKey is the field name this plugin contributes to the shared
	// access-log object. It sits alongside the token counts ai-statistics
	// writes, so one log line carries both what was used and what it cost.
	creditsLogKey = "credits"
	// modelHeader is written by the model-router plugin, which runs in the
	// AUTHN phase and therefore before this one. Reading the model from a
	// header rather than from the request body keeps the completion path on
	// DontReadRequestBody: buffering every prompt to learn one field would
	// cost far more than the charge it enables.
	modelHeader = "x-higress-llm-model"
)

type ChatMode string

const (
	ChatModeCompletion ChatMode = "completion"
	ChatModeAdmin      ChatMode = "admin"
	ChatModeNone       ChatMode = "none"
)

type AdminMode string

const (
	AdminModeRefresh AdminMode = "refresh"
	AdminModeQuery   AdminMode = "query"
	AdminModeDelta   AdminMode = "delta"
	AdminModeNone    AdminMode = "none"
)

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
		wrapper.ProcessStreamingResponseBody(onHttpStreamingResponseBody),
	)
}

type QuotaConfig struct {
	redisInfo          RedisInfo         `yaml:"redis"`
	RedisKeyPrefix     string            `yaml:"redis_key_prefix"`
	AdminConsumer      string            `yaml:"admin_consumer"`
	AdminPath          string            `yaml:"admin_path"`
	EnablePathSuffixes []string          `yaml:"enable_path_suffixes"`
	credential2Name    map[string]string `yaml:"-"`
	redisClient        wrapper.RedisClient
	// DefaultPrice applies to every model this route serves that has no entry
	// of its own. Nil, together with an empty ModelPrices, means the route is
	// unpriced and one token costs one unit -- the behaviour before credits.
	DefaultPrice *Price           `yaml:"default_price"`
	ModelPrices  map[string]Price `yaml:"model_prices"`
	// counters caches the credit counters this rule has defined, keyed by the
	// full metric name. Defining a counter twice is wasteful rather than
	// wrong, but the cache is also what keeps the per-request path free of
	// host calls once a (route, model, consumer) triple has been seen.
	counters map[string]proxywasm.MetricCounter
}

type Consumer struct {
	Name       string `yaml:"name"`
	Credential string `yaml:"credential"`
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
	config.counters = make(map[string]proxywasm.MetricCounter)
	// admin
	config.AdminPath = json.Get("admin_path").String()
	config.AdminConsumer = json.Get("admin_consumer").String()
	if config.AdminPath == "" {
		config.AdminPath = "/quota"
	}
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
	if config.AdminConsumer == "" {
		return errors.New("missing admin_consumer in config")
	}
	// Redis
	config.RedisKeyPrefix = json.Get("redis_key_prefix").String()
	if config.RedisKeyPrefix == "" {
		config.RedisKeyPrefix = "chat_quota:"
	}
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

	if err := parsePrices(json, config); err != nil {
		return err
	}

	return config.redisClient.Init(username, password, int64(timeout), wrapper.WithDataBase(database))
}

func onHttpRequestHeaders(context wrapper.HttpContext, config QuotaConfig) types.Action {
	context.DisableReroute()
	log.Debugf("onHttpRequestHeaders()")
	// get tokens
	consumer, err := proxywasm.GetHttpRequestHeader("x-mse-consumer")
	if err != nil {
		return deniedNoKeyAuthData()
	}
	if consumer == "" {
		return deniedUnauthorizedConsumer()
	}

	rawPath := context.Path()
	path, _ := url.Parse(rawPath)
	chatMode, adminMode := getOperationMode(path.Path, config.AdminPath, config.EnablePathSuffixes)
	context.SetContext("chatMode", chatMode)
	context.SetContext("adminMode", adminMode)
	context.SetContext("consumer", consumer)
	if model, err := proxywasm.GetHttpRequestHeader(modelHeader); err == nil {
		context.SetContext(quotaModelContextKey, strings.TrimSpace(model))
	}
	route, cluster := routeAndCluster()
	context.SetContext(quotaRouteContextKey, route)
	context.SetContext(quotaClusterContextKey, cluster)
	log.Debugf("chatMode:%s, adminMode:%s, consumer:%s", chatMode, adminMode, consumer)
	if chatMode == ChatModeNone {
		return types.ActionContinue
	}
	if chatMode == ChatModeAdmin {
		// query quota
		if adminMode == AdminModeQuery {
			return queryQuota(context, config, consumer, path)
		}
		if adminMode == AdminModeRefresh || adminMode == AdminModeDelta {
			context.BufferRequestBody()
			return types.HeaderStopIteration
		}
		return types.ActionContinue
	}

	// there is no need to read request body when it is on chat completion mode
	context.DontReadRequestBody()
	// check quota here
	config.redisClient.Get(config.RedisKeyPrefix+consumer, func(response resp.Value) {
		isDenied := false
		if err := response.Error(); err != nil {
			isDenied = true
		}
		if response.IsNull() {
			isDenied = true
		}
		if response.Integer() <= 0 {
			isDenied = true
		}
		log.Debugf("get consumer:%s quota:%d isDenied:%t", consumer, response.Integer(), isDenied)
		if isDenied {
			util.SendResponse(http.StatusForbidden, "ai-quota.noquota", "text/plain", "Request denied by ai quota check, No quota left")
			return
		}
		proxywasm.ResumeHttpRequest()
	})
	return types.HeaderStopAllIterationAndWatermark
}

func onHttpRequestBody(ctx wrapper.HttpContext, config QuotaConfig, body []byte) types.Action {
	log.Debugf("onHttpRequestBody()")
	chatMode, ok := ctx.GetContext("chatMode").(ChatMode)
	if !ok {
		return types.ActionContinue
	}
	if chatMode == ChatModeNone || chatMode == ChatModeCompletion {
		return types.ActionContinue
	}
	adminMode, ok := ctx.GetContext("adminMode").(AdminMode)
	if !ok {
		return types.ActionContinue
	}
	adminConsumer, ok := ctx.GetContext("consumer").(string)
	if !ok {
		return types.ActionContinue
	}

	if adminMode == AdminModeRefresh {
		return refreshQuota(ctx, config, adminConsumer, string(body))
	}
	if adminMode == AdminModeDelta {
		return deltaQuota(ctx, config, adminConsumer, string(body))
	}

	return types.ActionContinue
}

func onHttpStreamingResponseBody(ctx wrapper.HttpContext, config QuotaConfig, data []byte, endOfStream bool) []byte {
	chatMode, ok := ctx.GetContext("chatMode").(ChatMode)
	if !ok {
		return data
	}
	if chatMode == ChatModeNone || chatMode == ChatModeAdmin {
		return data
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
		if usage.Model != "" {
			ctx.SetContext(quotaUsageModelContextKey, usage.Model)
		}
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

func chargeQuotaOnce(ctx wrapper.HttpContext, config QuotaConfig) {
	if ctx.GetBoolContext(quotaChargedContextKey, false) {
		return
	}
	if ctx.GetContext("consumer") == nil {
		return
	}

	model := ctx.GetStringContext(quotaModelContextKey, "")
	amount, ok := chargeAmount(ctx, config, model)
	if !ok {
		return
	}

	consumer := ctx.GetContext("consumer").(string)
	log.Debugf("update consumer:%s, model:%s, charge:%d", consumer, model, amount)
	if err := config.redisClient.DecrBy(config.RedisKeyPrefix+consumer, int(amount), nil); err != nil {
		log.Errorf("failed to update consumer %s quota: %v", consumer, err)
		return
	}
	ctx.SetContext(quotaChargedContextKey, true)
	reportCharge(ctx, amount)
	reportCreditsMetric(ctx, config, consumer, amount)
}

// reportCharge puts what was actually charged into the access log.
//
// The charge is the only number that says what a request cost, and nothing
// downstream can recompute it: prices change, so multiplying yesterday's
// tokens by today's rate answers a different question. It has to be recorded
// when it happens.
//
// The log attribute is written through the shared `custom_log` property,
// which is read-merge-write. ai-statistics writes the token counts into the
// same object from its own filter, so this adds a field rather than
// replacing what is there, and needs no access-log format change.
//
// A charge of zero is reported. Free is a price an operator set, and a
// statistics row showing 0 says something a missing field does not: an
// absent field means the request was never charged at all -- no usage
// arrived under a token price -- and those two must not look alike.
func reportCharge(ctx wrapper.HttpContext, amount int64) {
	// Contribute exactly one field, by replacing this plugin's attribute map
	// rather than adding to it. The wrapper merges the WHOLE map into the
	// shared object, and tokenusage has already filled it with its own view
	// of the request -- including model "unknown" when the terminal chunk
	// carried no model name. Merging all of that would overwrite what
	// ai-statistics logged from a filter that did see the request body, so a
	// plugin that only wants to add a cost would silently corrupt the model
	// and token fields of every log line.
	ctx.SetUserAttributeMap(map[string]interface{}{creditsLogKey: amount})
	if err := ctx.WriteUserAttributeToLog(); err != nil {
		// The quota has already moved. Losing the log line is bad, but it is
		// not a reason to fail a request whose money is already spent.
		log.Warnf("failed to record the credit charge in the access log: %v", err)
	}
}

// chargeAmount is what this request costs, in whole credits.
//
// An unpriced route keeps the pre-credits behaviour exactly: the charge is
// the token count. That is what makes this build safe to roll out ahead of
// any price configuration -- a route whose config has not been touched
// behaves as it did yesterday.
func chargeAmount(ctx wrapper.HttpContext, config QuotaConfig, model string) (int64, bool) {
	usage, usageKnown := usageBreakdown(ctx)

	price := priceFor(config, model)
	if price == nil {
		if !usageKnown {
			return 0, false
		}
		return usage.total(), true
	}

	amount, chargeable, err := chargeFor(*price, usage, usageKnown)
	if err != nil {
		// A charge that cannot be computed must not silently become zero: the
		// request consumed real capacity. Leaving the quota untouched and
		// saying so keeps the error recoverable by an operator.
		log.Errorf("cannot price model %q: %v", model, err)
		return 0, false
	}
	return amount, chargeable
}

// usageBreakdown recovers the four token components this request reported.
//
// The components are disjoint and summed, matching what getQuotaToken has
// always done: Anthropic reports cache reads and cache creations alongside
// input_tokens, not inside them.
func usageBreakdown(ctx wrapper.HttpContext) (tokenBreakdown, bool) {
	var usage tokenBreakdown
	if details, ok := ctx.GetContext(tokenusage.CtxKeyInputTokenDetails).(map[string]int64); ok {
		usage.CacheRead = details[tokenusage.InputTokenDetailsKeyAnthropicMessagesUsageCacheReadInputTokens]
		usage.CacheWrite = details[tokenusage.InputTokenDetailsKeyAnthropicMessagesUsageCacheCreationInputTokens]
	}

	input, inputOK := ctx.GetContext(tokenusage.CtxKeyInputToken).(int64)
	output, outputOK := ctx.GetContext(tokenusage.CtxKeyOutputToken).(int64)
	if inputOK && outputOK {
		usage.Input = input
		usage.Output = output
		return usage, true
	}

	// A provider that reports only a total leaves nothing to split. Billing
	// the whole of it at the input rate is stated here rather than guessed at
	// a call site, and it is logged, because a per-component price silently
	// applied to an unsplit total is a wrong bill that looks like a right one.
	if total, ok := ctx.GetContext(tokenusage.CtxKeyTotalToken).(int64); ok && total > 0 {
		log.Warnf("usage reported as a total only; billing %d tokens at the input rate", total)
		usage.Input = total - usage.CacheRead - usage.CacheWrite
		if usage.Input < 0 {
			usage.Input = 0
		}
		return usage, true
	}
	return usage, false
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

func getOperationMode(path string, adminPath string, pathSuffixes []string) (ChatMode, AdminMode) {
	fullAdminPath := "/v1/chat/completions" + adminPath
	if strings.HasSuffix(path, fullAdminPath+"/refresh") {
		return ChatModeAdmin, AdminModeRefresh
	}
	if strings.HasSuffix(path, fullAdminPath+"/delta") {
		return ChatModeAdmin, AdminModeDelta
	}
	if strings.HasSuffix(path, fullAdminPath) {
		return ChatModeAdmin, AdminModeQuery
	}
	for _, suffix := range pathSuffixes {
		if strings.HasSuffix(path, suffix) {
			return ChatModeCompletion, AdminModeNone
		}
	}
	return ChatModeNone, AdminModeNone
}

func refreshQuota(ctx wrapper.HttpContext, config QuotaConfig, adminConsumer string, body string) types.Action {
	// check consumer
	if adminConsumer != config.AdminConsumer {
		util.SendResponse(http.StatusForbidden, "ai-quota.unauthorized", "text/plain", "Request denied by ai quota check. Unauthorized admin consumer.")
		return types.ActionContinue
	}

	queryValues, _ := url.ParseQuery(body)
	values := make(map[string]string, len(queryValues))
	for k, v := range queryValues {
		values[k] = v[0]
	}
	queryConsumer := values["consumer"]
	quota, err := strconv.Atoi(values["quota"])
	if queryConsumer == "" || err != nil {
		util.SendResponse(http.StatusForbidden, "ai-quota.unauthorized", "text/plain", "Request denied by ai quota check. consumer can't be empty and quota must be integer.")
		return types.ActionContinue
	}
	err2 := config.redisClient.Set(config.RedisKeyPrefix+queryConsumer, quota, func(response resp.Value) {
		log.Debugf("Redis set key = %s quota = %d", config.RedisKeyPrefix+queryConsumer, quota)
		if err := response.Error(); err != nil {
			util.SendResponse(http.StatusServiceUnavailable, "ai-quota.error", "text/plain", fmt.Sprintf("redis error:%v", err))
			return
		}
		util.SendResponse(http.StatusOK, "ai-quota.refreshquota", "text/plain", "refresh quota successful")
	})

	if err2 != nil {
		util.SendResponse(http.StatusServiceUnavailable, "ai-quota.error", "text/plain", fmt.Sprintf("redis error:%v", err))
		return types.ActionContinue
	}

	return types.ActionPause
}

func queryQuota(ctx wrapper.HttpContext, config QuotaConfig, adminConsumer string, url *url.URL) types.Action {
	// check consumer
	if adminConsumer != config.AdminConsumer {
		util.SendResponse(http.StatusForbidden, "ai-quota.unauthorized", "text/plain", "Request denied by ai quota check. Unauthorized admin consumer.")
		return types.ActionContinue
	}
	// check url
	queryValues := url.Query()
	values := make(map[string]string, len(queryValues))
	for k, v := range queryValues {
		values[k] = v[0]
	}
	if values["consumer"] == "" {
		util.SendResponse(http.StatusForbidden, "ai-quota.unauthorized", "text/plain", "Request denied by ai quota check. consumer can't be empty.")
		return types.ActionContinue
	}
	queryConsumer := values["consumer"]
	err := config.redisClient.Get(config.RedisKeyPrefix+queryConsumer, func(response resp.Value) {
		quota := 0
		if err := response.Error(); err != nil {
			util.SendResponse(http.StatusServiceUnavailable, "ai-quota.error", "text/plain", fmt.Sprintf("redis error:%v", err))
			return
		} else if response.IsNull() {
			quota = 0
		} else {
			quota = response.Integer()
		}
		result := struct {
			Consumer string `json:"consumer"`
			Quota    int    `json:"quota"`
		}{
			Consumer: queryConsumer,
			Quota:    quota,
		}
		body, _ := json.Marshal(result)
		util.SendResponse(http.StatusOK, "ai-quota.queryquota", "application/json", string(body))
	})
	if err != nil {
		util.SendResponse(http.StatusServiceUnavailable, "ai-quota.error", "text/plain", fmt.Sprintf("redis error:%v", err))
		return types.ActionContinue
	}
	return types.ActionPause
}

func deltaQuota(ctx wrapper.HttpContext, config QuotaConfig, adminConsumer string, body string) types.Action {
	// check consumer
	if adminConsumer != config.AdminConsumer {
		util.SendResponse(http.StatusForbidden, "ai-quota.unauthorized", "text/plain", "Request denied by ai quota check. Unauthorized admin consumer.")
		return types.ActionContinue
	}

	queryValues, _ := url.ParseQuery(body)
	values := make(map[string]string, len(queryValues))
	for k, v := range queryValues {
		values[k] = v[0]
	}
	queryConsumer := values["consumer"]
	value, err := strconv.Atoi(values["value"])
	if queryConsumer == "" || err != nil {
		util.SendResponse(http.StatusForbidden, "ai-quota.unauthorized", "text/plain", "Request denied by ai quota check. consumer can't be empty and value must be integer.")
		return types.ActionContinue
	}

	if value >= 0 {
		err := config.redisClient.IncrBy(config.RedisKeyPrefix+queryConsumer, value, func(response resp.Value) {
			log.Debugf("Redis Incr key = %s value = %d", config.RedisKeyPrefix+queryConsumer, value)
			if err := response.Error(); err != nil {
				util.SendResponse(http.StatusServiceUnavailable, "ai-quota.error", "text/plain", fmt.Sprintf("redis error:%v", err))
				return
			}
			util.SendResponse(http.StatusOK, "ai-quota.deltaquota", "text/plain", "delta quota successful")
		})
		if err != nil {
			util.SendResponse(http.StatusServiceUnavailable, "ai-quota.error", "text/plain", fmt.Sprintf("redis error:%v", err))
			return types.ActionContinue
		}
	} else {
		err := config.redisClient.DecrBy(config.RedisKeyPrefix+queryConsumer, 0-value, func(response resp.Value) {
			log.Debugf("Redis Decr key = %s value = %d", config.RedisKeyPrefix+queryConsumer, 0-value)
			if err := response.Error(); err != nil {
				util.SendResponse(http.StatusServiceUnavailable, "ai-quota.error", "text/plain", fmt.Sprintf("redis error:%v", err))
				return
			}
			util.SendResponse(http.StatusOK, "ai-quota.deltaquota", "text/plain", "delta quota successful")
		})
		if err != nil {
			util.SendResponse(http.StatusServiceUnavailable, "ai-quota.error", "text/plain", fmt.Sprintf("redis error:%v", err))
			return types.ActionContinue
		}
	}

	return types.ActionPause
}
