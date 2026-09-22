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
	// quotaStatusContextKey holds the upstream's HTTP status. It decides
	// whether a per-request price applies at all -- see chargeFor.
	quotaStatusContextKey = "ai-quota-status"
	// creditsLogKey is the field name this plugin contributes to the shared
	// access-log object. It sits alongside the token counts ai-statistics
	// writes, so one log line carries both what was used and what it cost.
	//
	// The name carries the unit because the value is in MILLI-credits, and
	// ai-statistics turns this field into the gateway counter
	// route_upstream_model_consumer_metric_credit_millis automatically. A
	// field still called `credits` holding thousandths would make every
	// reader -- dashboard, log column, whoever greps it next -- silently
	// wrong by a factor of a thousand.
	creditsLogKey = "credit_millis"
	// quotaCharactersContextKey holds the character count read from the
	// request body, for routes priced per character. It is recorded on the
	// way in because the response -- audio -- cannot carry it.
	quotaCharactersContextKey = "ai-quota-characters"
	// quotaSecondsContextKey holds a duration-style usage read from the
	// response, for routes priced per second.
	quotaSecondsContextKey = "ai-quota-seconds"
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
		wrapper.ProcessResponseHeaders(onHttpResponseHeaders),
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
	// unpriced and one token costs one whole credit -- the behaviour before
	// credits, expressed in the milli-credit ledger.
	DefaultPrice *Price           `yaml:"default_price"`
	ModelPrices  map[string]Price `yaml:"model_prices"`
	// counters caches the credit counters this rule has defined, keyed by the
	// full metric name. Defining a counter twice is wasteful rather than
	// wrong, but the cache is also what keeps the per-request path free of
	// host calls once a (route, model, consumer) triple has been seen.
	counters map[string]proxywasm.MetricCounter
}

// metersCharacters says whether any price on this route bills per character.
//
// It decides one thing: whether the request body is buffered. The completion
// path otherwise runs on DontReadRequestBody, and the comment on modelHeader
// explains why that is worth protecting -- buffering every prompt to learn
// one field costs far more than the charge it enables. A per-character price
// is the one case where the request body IS the billable quantity, so the
// cost is the point rather than an overhead, and it is paid only on the
// routes configured that way.
func (config QuotaConfig) metersCharacters() bool {
	if config.DefaultPrice != nil && config.DefaultPrice.Unit == UnitCharacters {
		return true
	}
	for _, price := range config.ModelPrices {
		if price.Unit == UnitCharacters {
			return true
		}
	}
	return false
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

	// The completion path normally needs nothing from the request body -- the
	// model arrives in a header. A route priced per character is the
	// exception: its billable quantity is the request text itself.
	if config.metersCharacters() {
		context.BufferRequestBody()
	} else {
		context.DontReadRequestBody()
	}
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

// onHttpResponseHeaders records the upstream's status.
//
// The plugin has never needed it: under token pricing a failed request
// reports no usage, so it costs nothing without anybody checking. A
// per-request price has no such protection -- it charges for the attempt --
// so the status has to be captured before the body callback wants it.
func onHttpResponseHeaders(ctx wrapper.HttpContext, config QuotaConfig) types.Action {
	status, err := proxywasm.GetHttpResponseHeader(":status")
	if err != nil {
		log.Warnf("no response status available: %v", err)
		return types.ActionContinue
	}
	code, err := strconv.Atoi(strings.TrimSpace(status))
	if err != nil {
		log.Warnf("unreadable response status %q: %v", status, err)
		return types.ActionContinue
	}
	ctx.SetContext(quotaStatusContextKey, code)
	return types.ActionContinue
}

// responseSucceeded reports whether the upstream answered.
//
// An unknown status counts as a failure. The body callback only runs once
// headers have been through this filter, so in practice the status is always
// there; if it somehow is not, the safe reading of "we cannot tell whether
// the caller got an answer" is not to bill them for it.
func responseSucceeded(ctx wrapper.HttpContext) bool {
	code, ok := ctx.GetContext(quotaStatusContextKey).(int)
	return ok && code < 400
}

func onHttpRequestBody(ctx wrapper.HttpContext, config QuotaConfig, body []byte) types.Action {
	log.Debugf("onHttpRequestBody()")
	chatMode, ok := ctx.GetContext("chatMode").(ChatMode)
	if !ok {
		return types.ActionContinue
	}
	if chatMode == ChatModeCompletion {
		// Only reached when metersCharacters() asked for the body.
		if characters, ok := countRequestCharacters(body); ok {
			ctx.SetContext(quotaCharactersContextKey, characters)
		} else {
			// Left unset on purpose: chargeFor treats an unknown count as
			// "do not touch the quota", which surfaces a field-name
			// mismatch instead of silently synthesising free requests.
			log.Warnf("per-character pricing found no %v field in the request body", characterFields)
		}
		return types.ActionContinue
	}
	if chatMode == ChatModeNone {
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

	// A duration-style usage is not a token count, so GetTokenUsage ignores
	// it entirely. Transcription reports one, and it is the only number that
	// route can be billed on.
	if _, already := ctx.GetContext(quotaSecondsContextKey).(int64); !already {
		if seconds, ok := responseSeconds(data); ok {
			ctx.SetContext(quotaSecondsContextKey, seconds)
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
// It is written under wrapper.AILogKey, which is the `ai_log` property. That
// is the object ai-statistics fills and the only one the access-log format
// emits -- it carries a single %FILTER_STATE(wasm.ai_log:PLAIN)% and no
// custom_log at all. WriteUserAttributeToLog(), the obvious-looking call,
// writes to custom_log instead, so a charge recorded through it lands in a
// property nothing reads and the field simply never appears. The write is
// read-merge-write either way, so naming the right key adds a field rather
// than replacing what ai-statistics put there.
//
// The amount is in milli-credits, matching the deduction exactly: the log
// line, the gateway counter and the Redis counter must agree, and they only
// agree if nobody rescales one of them on the way out.
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
	if err := ctx.WriteUserAttributeToLogWithKey(wrapper.AILogKey); err != nil {
		// The quota has already moved. Losing the log line is bad, but it is
		// not a reason to fail a request whose money is already spent.
		log.Warnf("failed to record the credit charge in the access log: %v", err)
	}
}

// chargeAmount is what this request costs, in MILLI-credits.
//
// An unpriced route keeps the pre-credits behaviour exactly: one token costs
// one whole credit. That is what makes this build safe to roll out ahead of
// any price configuration -- a route whose config has not been touched
// behaves as it did yesterday. The token count is scaled into the ledger's
// unit to hold that equivalence; dropping the scale here would quietly make
// every unpriced route a thousand times cheaper.
func chargeAmount(ctx wrapper.HttpContext, config QuotaConfig, model string) (int64, bool) {
	usage := meteredUsage(ctx)

	price := priceFor(config, model)
	if price == nil {
		if !usage.tokensKnown {
			return 0, false
		}
		return usage.tokens.total() * millisPerCredit, true
	}

	amount, chargeable, err := chargeFor(*price, usage)
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
func meteredUsage(ctx wrapper.HttpContext) metered {
	usage := metered{succeeded: responseSucceeded(ctx)}
	if characters, ok := ctx.GetContext(quotaCharactersContextKey).(int64); ok {
		usage.characters, usage.charactersKnown = characters, true
	}
	if seconds, ok := ctx.GetContext(quotaSecondsContextKey).(int64); ok {
		usage.seconds, usage.secondsKnown = seconds, true
	}
	usage.tokens, usage.tokensKnown = tokenUsage(ctx)
	return usage
}

// Cache token detail keys, in the same order and with the same meaning
// ai-statistics uses for its cached_token / cache_creation_input_token
// metrics. The names are the provider's own: ExtractInputTokenDetails copies
// `prompt_tokens_details` into the map verbatim, so a key is whatever the
// upstream called it.
var (
	cacheReadKeys     = []string{"cached_tokens", "cache_read_input_tokens", "cached_content_token_count"}
	cacheCreationKeys = []string{"cache_creation_input_tokens"}
)

// firstPresent returns the first key that exists, matching getTokenDetailMetric:
// presence, not a non-zero value, is what selects a key. A provider reporting
// cached_tokens: 0 has answered the question.
func firstPresent(details map[string]int64, keys []string) int64 {
	for _, key := range keys {
		if value, ok := details[key]; ok && value >= 0 {
			return value
		}
	}
	return 0
}

// splitTokens turns a reported usage into the four DISJOINT components a price
// is applied to.
//
// The one thing that decides the bill here is whether the provider's cache
// count is already inside the input count, and providers disagree:
//
//	Anthropic   cache_read_input_tokens sits ALONGSIDE input_tokens
//	            (total = input + output + cache)
//	OpenAI      cached_tokens is INSIDE prompt_tokens
//	            (total = prompt + completion, cache in neither sum)
//
// Both shapes reach the same model here, so the distinction cannot be drawn
// per model or per route -- and it is not drawn by key name either. It uses
// the arithmetic ai-statistics already settled on for its
// cache_detail_in_total_token metric: compare the reported total against
// input + output. The response answers the question about itself.
//
// Verified against 100 production records carrying cache
// (SLS cloudeyeforai/ai-accesslog, 2026-09-21..22) across /v1/chat/completions,
// /bailian/v1/chat/completions and /bailian/v1/v1/messages: where total
// exceeded input+output the excess equalled the reported cache counts exactly,
// and where it did not the cache was inside input. 100/100.
func splitTokens(details map[string]int64, input, output, total int64) tokenBreakdown {
	usage := tokenBreakdown{Input: input, Output: output}
	cacheRead := firstPresent(details, cacheReadKeys)
	cacheCreation := firstPresent(details, cacheCreationKeys)
	if cacheRead == 0 && cacheCreation == 0 {
		return usage
	}

	// How much of the cache the provider counted OUTSIDE input+output. Clamped
	// to what was actually reported, exactly as ai-statistics clamps
	// cacheDetailInTotal, so a total inflated for any other reason cannot
	// manufacture cache tokens.
	alongside := int64(0)
	if total > input+output {
		alongside = total - input - output
		if observed := cacheRead + cacheCreation; alongside > observed {
			alongside = observed
		}
	}

	// Anything the provider did NOT count outside is inside the input count,
	// and has to be moved out of it so it is billed once, at the cache rate.
	inside := cacheRead + cacheCreation - alongside
	if inside > usage.Input {
		// More cache than the prompt it is supposedly part of is a provider
		// bug. Never drive input negative, and never bill cache tokens the
		// prompt cannot contain: drop the excess, reads first, so the whole
		// prompt is billed exactly once at the cache rate.
		excess := inside - usage.Input
		inside = usage.Input
		if cacheRead >= excess {
			cacheRead -= excess
		} else {
			cacheCreation -= excess - cacheRead
			cacheRead = 0
		}
		if cacheCreation < 0 {
			cacheCreation = 0
		}
	}
	usage.Input -= inside

	// Both components bill in full at their own rate. Moving tokens out of
	// Input above only removed the double count; it never changed how much
	// cache there was.
	usage.CacheRead = cacheRead
	usage.CacheWrite = cacheCreation
	return usage
}

// tokenUsage recovers the four token components this request reported.
func tokenUsage(ctx wrapper.HttpContext) (tokenBreakdown, bool) {
	details, _ := ctx.GetContext(tokenusage.CtxKeyInputTokenDetails).(map[string]int64)
	total, totalOK := ctx.GetContext(tokenusage.CtxKeyTotalToken).(int64)

	input, inputOK := ctx.GetContext(tokenusage.CtxKeyInputToken).(int64)
	output, outputOK := ctx.GetContext(tokenusage.CtxKeyOutputToken).(int64)
	if inputOK && outputOK {
		if !totalOK {
			// No total to compare against, so the arithmetic cannot say which
			// shape this is. Assume the cache is inside input, which is the
			// common shape and the one that bills LESS: a wrong guess here
			// undercharges rather than double-charges a discounted token.
			total = input + output
		}
		return splitTokens(details, input, output, total), true
	}

	// A provider that reports only a total leaves nothing to split. Billing
	// the whole of it at the input rate is stated here rather than guessed at
	// a call site, and it is logged, because a per-component price silently
	// applied to an unsplit total is a wrong bill that looks like a right one.
	if totalOK && total > 0 {
		log.Warnf("usage reported as a total only; billing %d tokens at the input rate", total)
		// Passing total as both the input and the total makes the rule read
		// "no excess outside input", so any reported cache is taken out of
		// the total and billed at the cache rate instead of the input rate.
		return splitTokens(details, total, 0, total), true
	}
	return tokenBreakdown{}, false
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
