// Copyright (c) 2022 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	pluginName            = "vllm-compat"
	defaultConsumerHeader = "x-mse-consumer"
	defaultSchemaName     = "guided_json"
	defaultMaxBodyBytes   = 10 * 1024 * 1024
)

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
	)
}

type Config struct {
	consumerHeader string
	// consumers limits the rewrite to specific authenticated callers. Empty means
	// every caller on the routes this plugin is attached to.
	consumers          map[string]struct{}
	enableOnPathSuffix []string
	// overwriteExisting decides what happens when the caller sent both a vLLM
	// field and its standard equivalent. Off by default: an explicit standard
	// field is the caller's real intent, the vLLM one is usually SDK boilerplate.
	overwriteExisting bool
	// stripSource removes a vLLM field once it has been translated, so the
	// upstream never sees two sources of truth for the same constraint.
	stripSource   bool
	schemaName    string
	guidedJson    bool
	guidedChoice  bool
	guidedRegex   bool
	guidedGrammar bool
}

func parseConfig(cfg gjson.Result, config *Config) error {
	config.consumerHeader = strings.TrimSpace(cfg.Get("consumerHeader").String())
	if config.consumerHeader == "" {
		config.consumerHeader = defaultConsumerHeader
	}

	config.consumers = make(map[string]struct{})
	consumers := cfg.Get("consumers")
	if consumers.Exists() && !consumers.IsArray() {
		return errors.New("consumers must be an array")
	}
	for _, item := range consumers.Array() {
		if name := strings.TrimSpace(item.String()); name != "" {
			config.consumers[name] = struct{}{}
		}
	}

	suffixes := cfg.Get("enableOnPathSuffix")
	if suffixes.Exists() && !suffixes.IsArray() {
		return errors.New("enableOnPathSuffix must be an array")
	}
	for _, item := range suffixes.Array() {
		if s := strings.TrimSpace(item.String()); s != "" {
			config.enableOnPathSuffix = append(config.enableOnPathSuffix, s)
		}
	}
	if len(config.enableOnPathSuffix) == 0 {
		config.enableOnPathSuffix = []string{"/chat/completions", "/completions"}
	}

	config.overwriteExisting = cfg.Get("overwriteExisting").Bool()
	config.stripSource = boolOrDefault(cfg.Get("stripSource"), true)
	config.schemaName = strings.TrimSpace(cfg.Get("schemaName").String())
	if config.schemaName == "" {
		config.schemaName = defaultSchemaName
	}
	config.guidedJson = boolOrDefault(cfg.Get("guidedJson"), true)
	config.guidedChoice = boolOrDefault(cfg.Get("guidedChoice"), true)
	config.guidedRegex = boolOrDefault(cfg.Get("guidedRegex"), true)
	config.guidedGrammar = boolOrDefault(cfg.Get("guidedGrammar"), true)
	return nil
}

func boolOrDefault(value gjson.Result, fallback bool) bool {
	if !value.Exists() {
		return fallback
	}
	return value.Bool()
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	if ctx.Method() != "POST" || !ctx.HasRequestBody() {
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}

	path := ctx.Path()
	if cut := strings.IndexAny(path, "?#"); cut >= 0 {
		path = path[:cut]
	}
	matched := false
	for _, suffix := range config.enableOnPathSuffix {
		if strings.HasSuffix(path, suffix) {
			matched = true
			break
		}
	}
	if !matched {
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}

	if len(config.consumers) > 0 {
		// key-auth injects this header once it has authenticated the caller.
		consumer, err := proxywasm.GetHttpRequestHeader(config.consumerHeader)
		if err != nil {
			consumer = ""
		}
		if _, ok := config.consumers[strings.TrimSpace(consumer)]; !ok {
			ctx.DontReadRequestBody()
			return types.ActionContinue
		}
	}

	// The rewrite changes the body length, so the original content-length must go.
	proxywasm.RemoveHttpRequestHeader("content-length")
	ctx.SetRequestBodyBufferLimit(defaultMaxBodyBytes)
	return types.HeaderStopIteration
}

func onHttpRequestBody(ctx wrapper.HttpContext, config Config, body []byte) types.Action {
	if len(body) == 0 {
		return types.ActionContinue
	}
	if !json.Valid(body) {
		// Not our business to reject it; let the upstream produce the error.
		return types.ActionContinue
	}

	newBody, result := transformBody(body, &config)
	for _, note := range result.skipped {
		log.Warnf("%s", note)
	}
	if len(result.applied) == 0 {
		return types.ActionContinue
	}
	log.Debugf("translated vLLM fields: %s", strings.Join(result.applied, ","))
	if err := proxywasm.ReplaceHttpRequestBody(newBody); err != nil {
		log.Errorf("failed to replace request body: %v", err)
	}
	return types.ActionContinue
}

// rewriteResult reports what transformBody did. It exists so the translation
// itself stays free of host calls and can be unit tested outside the sandbox.
type rewriteResult struct {
	// applied lists the vLLM fields that were translated.
	applied []string
	// skipped carries human-readable reasons a present field was left alone.
	skipped []string
}

// transformBody translates vLLM guided-decoding fields into the equivalents the
// SGLang/OpenAI-compatible upstream actually honours. It is a no-op for any
// request that carries none of them, and never rewrites a field the caller set
// itself unless overwriteExisting is on.
func transformBody(body []byte, config *Config) ([]byte, rewriteResult) {
	var result rewriteResult

	if config.guidedJson {
		if schema := gjson.GetBytes(body, "guided_json"); schema.Exists() {
			switch {
			case !config.overwriteExisting && gjson.GetBytes(body, "response_format").Exists():
				result.skipped = append(result.skipped, "response_format already set, guided_json left untouched")
			default:
				raw, ok := schemaObject(schema)
				if !ok {
					result.skipped = append(result.skipped, "guided_json is neither an object nor a JSON-encoded object, left untouched")
					break
				}
				updated, err := sjson.SetRawBytes(body, "response_format", []byte(buildJsonSchemaFormat(config.schemaName, raw)))
				if err != nil {
					result.skipped = append(result.skipped, "failed to build response_format from guided_json: "+err.Error())
					break
				}
				body = updated
				result.applied = append(result.applied, "guided_json")
			}
		}
	}

	// guided_regex and guided_choice both land on `regex`; an explicit pattern is
	// more specific than a choice list, so it wins when a caller sends both.
	regexSet := false
	if config.guidedRegex {
		if pattern := gjson.GetBytes(body, "guided_regex"); pattern.Type == gjson.String {
			if !config.overwriteExisting && gjson.GetBytes(body, "regex").Exists() {
				result.skipped = append(result.skipped, "regex already set, guided_regex left untouched")
			} else if updated, err := sjson.SetBytes(body, "regex", pattern.String()); err != nil {
				result.skipped = append(result.skipped, "failed to set regex from guided_regex: "+err.Error())
			} else {
				body = updated
				regexSet = true
				result.applied = append(result.applied, "guided_regex")
			}
		}
	}

	if config.guidedChoice && !regexSet {
		if choices := gjson.GetBytes(body, "guided_choice"); choices.IsArray() {
			pattern, ok := choicesToRegex(choices.Array())
			switch {
			case !ok:
				result.skipped = append(result.skipped, "guided_choice is not a non-empty list of strings, left untouched")
			case !config.overwriteExisting && gjson.GetBytes(body, "regex").Exists():
				result.skipped = append(result.skipped, "regex already set, guided_choice left untouched")
			default:
				updated, err := sjson.SetBytes(body, "regex", pattern)
				if err != nil {
					result.skipped = append(result.skipped, "failed to set regex from guided_choice: "+err.Error())
					break
				}
				body = updated
				result.applied = append(result.applied, "guided_choice")
			}
		}
	}

	if config.guidedGrammar {
		if grammar := gjson.GetBytes(body, "guided_grammar"); grammar.Type == gjson.String {
			if !config.overwriteExisting && gjson.GetBytes(body, "ebnf").Exists() {
				result.skipped = append(result.skipped, "ebnf already set, guided_grammar left untouched")
			} else if updated, err := sjson.SetBytes(body, "ebnf", grammar.String()); err != nil {
				result.skipped = append(result.skipped, "failed to set ebnf from guided_grammar: "+err.Error())
			} else {
				body = updated
				result.applied = append(result.applied, "guided_grammar")
			}
		}
	}

	if len(result.applied) > 0 && config.stripSource {
		// guided_decoding_backend / guided_whitespace_pattern only make sense
		// alongside a guided_* field, so they go with the ones we translated.
		drop := append([]string{}, result.applied...)
		drop = append(drop, "guided_decoding_backend", "guided_whitespace_pattern")
		for _, key := range drop {
			if updated, err := sjson.DeleteBytes(body, key); err == nil {
				body = updated
			}
		}
	}

	return body, result
}

// schemaObject returns the JSON schema as raw JSON. Clients send it either as an
// object or as a JSON-encoded string, and both forms are common in the wild.
func schemaObject(schema gjson.Result) (string, bool) {
	if schema.IsObject() {
		return schema.Raw, true
	}
	if schema.Type == gjson.String {
		if inner := schema.String(); json.Valid([]byte(inner)) && gjson.Parse(inner).IsObject() {
			return inner, true
		}
	}
	return "", false
}

func buildJsonSchemaFormat(name, schema string) string {
	nameJson, _ := json.Marshal(name)
	var b strings.Builder
	b.WriteString(`{"type":"json_schema","json_schema":{"name":`)
	b.Write(nameJson)
	b.WriteString(`,"schema":`)
	b.WriteString(schema)
	b.WriteString(`}}`)
	return b.String()
}

func choicesToRegex(choices []gjson.Result) (string, bool) {
	alternatives := make([]string, 0, len(choices))
	for _, choice := range choices {
		if choice.Type != gjson.String {
			return "", false
		}
		if value := choice.String(); value != "" {
			alternatives = append(alternatives, escapeRegex(value))
		}
	}
	if len(alternatives) == 0 {
		return "", false
	}
	return "(" + strings.Join(alternatives, "|") + ")", true
}

// escapeRegex quotes regex metacharacters. It exists instead of regexp.QuoteMeta
// so the wasm binary does not have to carry the regexp package.
func escapeRegex(s string) string {
	const meta = `\.+*?()|[]{}^$`
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x80 && strings.ContainsRune(meta, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
