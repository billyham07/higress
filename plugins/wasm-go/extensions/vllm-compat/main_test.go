package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func defaultConfig() *Config {
	return &Config{
		stripSource:   true,
		schemaName:    defaultSchemaName,
		guidedJson:    true,
		guidedChoice:  true,
		guidedRegex:   true,
		guidedGrammar: true,
	}
}

func TestNoGuidedFieldsIsNoOp(t *testing.T) {
	body := []byte(`{"model":"qwen3.5","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"chat_template_kwargs":{"enable_thinking":true}}`)
	out, res := transformBody(body, defaultConfig())
	assert.Empty(t, res.applied)
	assert.JSONEq(t, string(body), string(out))
}

func TestGuidedJsonBecomesResponseFormat(t *testing.T) {
	body := []byte(`{"model":"qwen3.5","guided_json":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]},"guided_decoding_backend":"xgrammar"}`)
	out, res := transformBody(body, defaultConfig())
	require.NotEmpty(t, res.applied)

	assert.Equal(t, "json_schema", gjson.GetBytes(out, "response_format.type").String())
	assert.Equal(t, defaultSchemaName, gjson.GetBytes(out, "response_format.json_schema.name").String())
	assert.JSONEq(t,
		`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`,
		gjson.GetBytes(out, "response_format.json_schema.schema").Raw)

	assert.False(t, gjson.GetBytes(out, "guided_json").Exists())
	assert.False(t, gjson.GetBytes(out, "guided_decoding_backend").Exists())
	// Untouched fields survive.
	assert.Equal(t, "qwen3.5", gjson.GetBytes(out, "model").String())
}

func TestGuidedJsonAsEncodedString(t *testing.T) {
	schema := `{"type":"object","properties":{"age":{"type":"integer"}}}`
	encoded, err := json.Marshal(schema)
	require.NoError(t, err)
	body := []byte(`{"model":"qwen3.5","guided_json":` + string(encoded) + `}`)

	out, res := transformBody(body, defaultConfig())
	require.NotEmpty(t, res.applied)
	assert.JSONEq(t, schema, gjson.GetBytes(out, "response_format.json_schema.schema").Raw)
}

func TestExistingResponseFormatWins(t *testing.T) {
	body := []byte(`{"response_format":{"type":"json_object"},"guided_json":{"type":"object"}}`)
	out, res := transformBody(body, defaultConfig())
	assert.Empty(t, res.applied)
	assert.Equal(t, "json_object", gjson.GetBytes(out, "response_format.type").String())
	// Nothing was translated, so nothing was stripped either.
	assert.True(t, gjson.GetBytes(out, "guided_json").Exists())
}

func TestOverwriteExistingResponseFormat(t *testing.T) {
	config := defaultConfig()
	config.overwriteExisting = true
	body := []byte(`{"response_format":{"type":"json_object"},"guided_json":{"type":"object"}}`)

	out, res := transformBody(body, config)
	require.NotEmpty(t, res.applied)
	assert.Equal(t, "json_schema", gjson.GetBytes(out, "response_format.type").String())
}

func TestGuidedChoiceBecomesRegex(t *testing.T) {
	body := []byte(`{"guided_choice":["yes","no","不确定"]}`)
	out, res := transformBody(body, defaultConfig())
	require.NotEmpty(t, res.applied)
	assert.Equal(t, "(yes|no|不确定)", gjson.GetBytes(out, "regex").String())
	assert.False(t, gjson.GetBytes(out, "guided_choice").Exists())
}

func TestGuidedChoiceEscapesMetacharacters(t *testing.T) {
	body := []byte(`{"guided_choice":["a+b","c(d)","e.f"]}`)
	out, res := transformBody(body, defaultConfig())
	require.NotEmpty(t, res.applied)
	assert.Equal(t, `(a\+b|c\(d\)|e\.f)`, gjson.GetBytes(out, "regex").String())
}

func TestGuidedRegexBeatsGuidedChoice(t *testing.T) {
	body := []byte(`{"guided_choice":["yes","no"],"guided_regex":"[0-9]{4}"}`)
	out, res := transformBody(body, defaultConfig())
	require.NotEmpty(t, res.applied)
	assert.Equal(t, "[0-9]{4}", gjson.GetBytes(out, "regex").String())
	// The losing field stays, so the discrepancy remains visible upstream.
	assert.True(t, gjson.GetBytes(out, "guided_choice").Exists())
}

func TestGuidedGrammarBecomesEbnf(t *testing.T) {
	body := []byte(`{"guided_grammar":"root ::= \"yes\" | \"no\""}`)
	out, res := transformBody(body, defaultConfig())
	require.NotEmpty(t, res.applied)
	assert.Equal(t, `root ::= "yes" | "no"`, gjson.GetBytes(out, "ebnf").String())
}

func TestDisabledMappingIsNoOp(t *testing.T) {
	config := defaultConfig()
	config.guidedJson = false
	body := []byte(`{"guided_json":{"type":"object"}}`)

	out, res := transformBody(body, config)
	assert.Empty(t, res.applied)
	assert.True(t, gjson.GetBytes(out, "guided_json").Exists())
}

func TestStripSourceDisabled(t *testing.T) {
	config := defaultConfig()
	config.stripSource = false
	body := []byte(`{"guided_json":{"type":"object"}}`)

	out, res := transformBody(body, config)
	require.NotEmpty(t, res.applied)
	assert.True(t, gjson.GetBytes(out, "guided_json").Exists())
	assert.Equal(t, "json_schema", gjson.GetBytes(out, "response_format.type").String())
}

func TestMalformedGuidedJsonIsLeftAlone(t *testing.T) {
	body := []byte(`{"guided_json":"not a schema"}`)
	out, res := transformBody(body, defaultConfig())
	assert.Empty(t, res.applied)
	assert.JSONEq(t, string(body), string(out))
}
