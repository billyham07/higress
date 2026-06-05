package provider

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"

	"github.com/alibaba/higress/plugins/wasm-go/extensions/ai-proxy/util"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/resp"
)

const (
	TokenSelectionPolicyPrefixCache = "prefix_cache"
	defaultTokenPrefixCacheTTL      = 1800

	tokenPrefixCacheRedisLua = `-- hex string => bytes
local function hex_to_bytes(hex)
    local bytes = {}
    for i = 1, #hex, 2 do
        local byte_str = hex:sub(i, i+1)
        local byte_val = tonumber(byte_str, 16)
        table.insert(bytes, byte_val)
    end
    return bytes
end

-- bytes => hex string
local function bytes_to_hex(bytes)
    local result = ""
    for _, byte in ipairs(bytes) do
        result = result .. string.format("%02X", byte)
    end
    return result
end

-- byte XOR
local function byte_xor(a, b)
    local result = 0
    for i = 0, 7 do
        local bit_val = 2^i
        if ((a % (bit_val * 2)) >= bit_val) ~= ((b % (bit_val * 2)) >= bit_val) then
            result = result + bit_val
        end
    end
    return result
end

-- hex string XOR
local function hex_xor(a, b)
    if #a ~= #b then
        error("Hex strings must be of equal length, first is " .. a .. " second is " .. b)
    end

    local a_bytes = hex_to_bytes(a)
    local b_bytes = hex_to_bytes(b)

    local result_bytes = {}
    for i = 1, #a_bytes do
        table.insert(result_bytes, byte_xor(a_bytes[i], b_bytes[i]))
    end

    return bytes_to_hex(result_bytes)
end

local function is_allowed(target)
    for i = 3, #KEYS do
        if target == KEYS[i] then
            return true
        end
    end
    return false
end

local ttl = KEYS[1]
local default_target = KEYS[2]
local target = ""
local key = ""
local current_key = ""

local index = 1
while index <= #ARGV do
    if current_key == "" then
        current_key = ARGV[index]
    else
        current_key = hex_xor(current_key, ARGV[index])
    end

    if redis.call("EXISTS", current_key) == 1 then
        local tmp_target = redis.call("GET", current_key)
        if not is_allowed(tmp_target) then
            break
        end
        key = current_key
        target = tmp_target
        redis.call("EXPIRE", current_key, ttl)
        index = index + 1
    else
        break
    end
end

if target == "" then
    target = default_target
end

while index <= #ARGV do
    if key == "" then
        key = ARGV[index]
    else
        key = hex_xor(key, ARGV[index])
    end
    redis.call("SET", key, target)
    redis.call("EXPIRE", key, ttl)
    index = index + 1
end

return target`
)

type TokenSelectionConfig struct {
	policy      string
	prefixCache *TokenPrefixCacheConfig
}

type TokenPrefixCacheConfig struct {
	redisClient        wrapper.RedisClient
	serviceFQDN        string
	servicePort        int64
	username           string
	password           string
	timeout            int64
	database           int64
	redisKeyTTL        int
	trimSpace          bool
	collapseWhitespace bool
	lowercase          bool
}

var whitespaceRegexp = regexp.MustCompile(`\s+`)

func (c *TokenSelectionConfig) FromJson(json gjson.Result) {
	*c = TokenSelectionConfig{}
	c.policy = json.Get("policy").String()
	if c.policy == "" {
		return
	}
	if c.policy == TokenSelectionPolicyPrefixCache {
		pc := &TokenPrefixCacheConfig{}
		pc.FromJson(json.Get("redis"), json.Get("normalizer"))
		c.prefixCache = pc
	}
}

func (c *TokenSelectionConfig) Validate(providerConfig *ProviderConfig) error {
	if c.policy == "" {
		return nil
	}
	if c.policy != TokenSelectionPolicyPrefixCache {
		return fmt.Errorf("unsupported tokenSelection policy %s", c.policy)
	}
	if len(providerConfig.apiTokens) < 2 {
		return errors.New("tokenSelection prefix_cache requires at least two apiTokens")
	}
	if c.prefixCache == nil {
		return errors.New("tokenSelection prefix_cache requires redis config")
	}
	return c.prefixCache.Init()
}

func (c *TokenSelectionConfig) IsPrefixCacheEnabled() bool {
	return c.policy == TokenSelectionPolicyPrefixCache && c.prefixCache != nil
}

func (c *TokenPrefixCacheConfig) FromJson(redisJson gjson.Result, normalizerJson gjson.Result) {
	c.serviceFQDN = redisJson.Get("serviceFQDN").String()
	c.servicePort = redisJson.Get("servicePort").Int()
	c.username = redisJson.Get("username").String()
	c.password = redisJson.Get("password").String()
	c.timeout = redisJson.Get("timeout").Int()
	if c.timeout == 0 {
		c.timeout = 3000
	}
	c.database = redisJson.Get("database").Int()
	c.redisKeyTTL = int(redisJson.Get("redisKeyTTL").Int())
	if c.redisKeyTTL == 0 {
		c.redisKeyTTL = defaultTokenPrefixCacheTTL
	}
	c.trimSpace = true
	c.collapseWhitespace = true
	if v := normalizerJson.Get("trimSpace"); v.Exists() {
		c.trimSpace = v.Bool()
	}
	if v := normalizerJson.Get("collapseWhitespace"); v.Exists() {
		c.collapseWhitespace = v.Bool()
	}
	c.lowercase = normalizerJson.Get("lowercase").Bool()
}

func (c *TokenPrefixCacheConfig) Init() error {
	if c.serviceFQDN == "" || c.servicePort == 0 {
		return errors.New("invalid tokenSelection prefix_cache redis config")
	}
	c.redisClient = wrapper.NewRedisClusterClient(wrapper.FQDNCluster{
		FQDN: c.serviceFQDN,
		Port: c.servicePort,
	})
	return c.redisClient.Init(c.username, c.password, c.timeout, wrapper.WithDataBase(int(c.database)))
}

func (c *ProviderConfig) ApplyPrefixCacheToken(ctx wrapper.HttpContext, apiName ApiName, body []byte) (types.Action, bool) {
	if !c.tokenSelection.IsPrefixCacheEnabled() || !isPrefixCacheSupportedAPI(apiName) {
		return types.ActionContinue, false
	}
	return c.tokenSelection.prefixCache.Apply(ctx, c, body)
}

func isPrefixCacheSupportedAPI(apiName ApiName) bool {
	return apiName == ApiNameChatCompletion || apiName == ApiNameAnthropicMessages
}

func (c *TokenPrefixCacheConfig) Apply(ctx wrapper.HttpContext, providerConfig *ProviderConfig, body []byte) (types.Action, bool) {
	params := c.promptPrefixHashes(providerConfig.GetId(), body)
	if len(params) == 0 {
		return types.ActionContinue, false
	}

	availableTokens := providerConfig.GetAvailableApiToken(ctx)
	if len(availableTokens) == 0 {
		availableTokens = providerConfig.apiTokens
	}
	tokenIDs, tokenByID := providerConfig.tokenIDs(availableTokens)
	if len(tokenIDs) == 0 {
		return types.ActionContinue, false
	}

	defaultTokenID := tokenIDs[rand.Intn(len(tokenIDs))]
	keys := []interface{}{c.redisKeyTTL, defaultTokenID}
	for _, tokenID := range tokenIDs {
		keys = append(keys, tokenID)
	}

	err := c.redisClient.Eval(tokenPrefixCacheRedisLua, len(keys), keys, params, func(response resp.Value) {
		defer proxywasm.ResumeHttpRequest()
		if err := response.Error(); err != nil {
			log.Errorf("[token_prefix_cache] redis eval failed: %+v", err)
			return
		}

		tokenID := response.String()
		token := tokenByID[tokenID]
		if token == "" {
			log.Warnf("[token_prefix_cache] redis returned unknown token id %s", tokenID)
			return
		}
		providerConfig.OverrideApiTokenInUse(ctx, token)
		if err := proxywasm.ReplaceHttpRequestHeader(util.HeaderAuthorization, "Bearer "+token); err != nil {
			log.Errorf("[token_prefix_cache] failed to overwrite Authorization header: %v", err)
			return
		}
		log.Debugf("[token_prefix_cache] selected token id %s", tokenID)
	})
	if err != nil {
		log.Errorf("[token_prefix_cache] redis eval failed: %+v", err)
		return types.ActionContinue, false
	}
	return types.ActionPause, true
}

func (c *TokenPrefixCacheConfig) promptPrefixHashes(providerID string, body []byte) []interface{} {
	messages := gjson.GetBytes(body, "messages").Array()
	if len(messages) == 0 {
		return nil
	}

	params := make([]interface{}, 0, len(messages))
	raw := c.promptPrefixSystem(body)
	namespace := providerID
	if namespace == "" {
		namespace = "default"
	}
	for index, obj := range messages {
		if !obj.Get("role").Exists() || !obj.Get("content").Exists() {
			return nil
		}
		role := obj.Get("role").String()
		content := c.normalizePromptContent(c.promptContentText(obj.Get("content")))
		raw += role + ":" + content
		if role == roleUser || index == len(messages)-1 {
			params = append(params, computeTokenPrefixSHA1(namespace+"\x00"+raw))
			raw = ""
		}
	}
	return params
}

func (c *TokenPrefixCacheConfig) promptPrefixSystem(body []byte) string {
	system := gjson.GetBytes(body, "system")
	if !system.Exists() {
		return ""
	}
	return "system:" + c.normalizePromptContent(c.promptContentText(system))
}

func (c *TokenPrefixCacheConfig) promptContentText(content gjson.Result) string {
	if content.IsArray() {
		parts := make([]string, 0)
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Type == gjson.String {
				parts = append(parts, block.String())
				return true
			}
			if text := block.Get("text"); text.Exists() {
				parts = append(parts, text.String())
				return true
			}
			if nested := block.Get("content"); nested.Exists() {
				parts = append(parts, c.promptContentText(nested))
			}
			return true
		})
		return strings.Join(parts, "\n")
	}
	return content.String()
}

func (c *TokenPrefixCacheConfig) normalizePromptContent(content string) string {
	if c.trimSpace {
		content = strings.TrimSpace(content)
	}
	if c.collapseWhitespace {
		content = whitespaceRegexp.ReplaceAllString(content, " ")
	}
	if c.lowercase {
		content = strings.ToLower(content)
	}
	return content
}

func (c *ProviderConfig) tokenIDs(tokens []string) ([]string, map[string]string) {
	ids := make([]string, 0, len(tokens))
	tokenByID := make(map[string]string, len(tokens))
	for _, token := range tokens {
		index := c.apiTokenIndex(token)
		if index < 0 {
			continue
		}
		id := strconv.Itoa(index)
		if _, exists := tokenByID[id]; exists {
			continue
		}
		ids = append(ids, id)
		tokenByID[id] = token
	}
	return ids, tokenByID
}

func (c *ProviderConfig) apiTokenIndex(token string) int {
	for index, candidate := range c.apiTokens {
		if candidate == token {
			return index
		}
	}
	return -1
}

func computeTokenPrefixSHA1(data string) string {
	hasher := sha1.New()
	hasher.Write([]byte(data))
	return strings.ToUpper(hex.EncodeToString(hasher.Sum(nil)))
}
