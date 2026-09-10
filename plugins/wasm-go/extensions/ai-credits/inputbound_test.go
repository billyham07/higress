package main

import (
	"encoding/json"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

// A text request is bounded by its own bytes: every token a tokenizer emits
// consumes at least one byte of the body it came from.
func TestTextRequestIsBoundedByItsBytes(t *testing.T) {
	body := []byte(`{"model":"qwen3.5","messages":[{"role":"user","content":"hello"}]}`)
	bound := computeInputBound(body, 0)
	require.True(t, bound.Provable)
	require.Equal(t, int64(len(body)), bound.TokensUpperBound)
	require.Zero(t, bound.ExternalReferences)
}

// An inline data: image travels inside the body, so the byte bound still
// covers it and no allowance is needed.
func TestInlineImageStaysInsideTheByteBound(t *testing.T) {
	body := []byte(`{"model":"qwen3.5","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is this"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	bound := computeInputBound(body, 0)
	require.True(t, bound.Provable)
	require.Zero(t, bound.ExternalReferences)
	require.Equal(t, int64(len(body)), bound.TokensUpperBound)
}

// A remote image URL is a few bytes that expand into an unknown number of
// tokens upstream. With no operator allowance the request has no provable
// bound, and the plugin must refuse rather than invent one.
func TestRemoteImageWithoutAllowanceIsNotProvable(t *testing.T) {
	body := []byte(`{"model":"qwen3.5","messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`)
	bound := computeInputBound(body, 0)
	require.False(t, bound.Provable)
	require.Equal(t, 1, bound.ExternalReferences)
	require.Equal(t, "remote image reference", bound.Reason)
}

// With an allowance declared, each unbounded reference adds exactly that many
// tokens to the ceiling.
func TestRemoteReferencesChargeTheDeclaredAllowance(t *testing.T) {
	body := []byte(`{"model":"qwen3.5","messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://example.com/a.png"}},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/b.png"}}]}]}`)
	bound := computeInputBound(body, 1500)
	require.True(t, bound.Provable)
	require.Equal(t, 2, bound.ExternalReferences)
	require.Equal(t, int64(len(body))+3000, bound.TokensUpperBound)
}

// The Anthropic shape reports its payload under `source`: base64 is inline,
// a url source is a reference.
func TestAnthropicImageSourcesAreClassified(t *testing.T) {
	inline := []byte(`{"model":"qwen3.5","messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`)
	require.True(t, computeInputBound(inline, 0).Provable)

	remote := []byte(`{"model":"qwen3.5","messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}`)
	require.False(t, computeInputBound(remote, 0).Provable)
}

// A file reference carries no payload at all, so it can never be bounded from
// the request bytes.
func TestFileReferenceIsAReference(t *testing.T) {
	body := []byte(`{"model":"qwen3.5","messages":[{"role":"user","content":[` +
		`{"type":"input_file","file_id":"file-abc"}]}]}`)
	bound := computeInputBound(body, 0)
	require.False(t, bound.Provable)
	require.Equal(t, "file reference", bound.Reason)
}

// End to end: the admit call the plugin sends carries the bound, not a zero.
// A zero estimate is what made the backend refuse every strict-cap admission.
func TestAdmitCarriesTheInputBound(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(creditsConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "aigw.example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"content-type", "application/json"},
			{"x-mse-consumer", "u-alice"},
		})
		body := []byte(`{"model":"qwen3.5","messages":[{"role":"user","content":"hi"}]}`)
		action := host.CallOnHttpRequestBody(body)
		require.Equal(t, types.ActionPause, action)
		callouts := host.GetHttpCalloutAttributes()
		require.NotEmpty(t, callouts)
		var sent map[string]any
		require.NoError(t, json.Unmarshal(callouts[0].Body, &sent))
		require.Equal(t, float64(len(body)), sent["estimatedInputTokens"])
		host.CompleteHttp()
	})
}

// An unbounded request is refused before the ledger is asked: admitting it
// would reserve against an estimate nobody can defend.
func TestEnforceRefusesAnUnboundedRequest(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(creditsConfig(ModeEnforce))
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)
		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "aigw.example.com"},
			{":path", "/v1/chat/completions"},
			{":method", "POST"},
			{"content-type", "application/json"},
			{"x-mse-consumer", "u-alice"},
		})
		host.CallOnHttpRequestBody([]byte(`{"model":"qwen3.5","messages":[{"role":"user","content":[` +
			`{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`))
		resp := host.GetLocalResponse()
		require.NotNil(t, resp)
		require.Equal(t, uint32(400), resp.StatusCode)
		require.Contains(t, string(resp.Data), "unbounded_estimate")
		host.CompleteHttp()
	})
}
