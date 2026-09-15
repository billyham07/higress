package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func sampleRegistryJSON() []byte {
	return []byte(`{
		"registry": {
			"version": "unified-model.v1",
			"configVersion": "snap-1",
			"defaultGroupKey": "h20",
			"models": [
				{
					"canonicalId": "h20/qwen3.5",
					"groupKey": "h20",
					"publicId": "h20/qwen3.5",
					"upstreamModel": "qwen3.5",
					"aliases": ["qwen3.5"],
					"routeValue": "qwen3.5",
					"capabilities": {
						"openai.chat.completions": true,
						"anthropic.messages": true,
						"openai.embeddings": false
					}
				},
				{
					"canonicalId": "bailian/glm-5.2",
					"groupKey": "bailian",
					"publicId": "bailian/glm-5.2",
					"upstreamModel": "glm-5.2",
					"aliases": ["glm-5.2"],
					"routeValue": "glm-5.2",
					"capabilities": {
						"openai.chat.completions": true,
						"anthropic.messages": false
					}
				},
				{
					"canonicalId": "h20/org/special",
					"groupKey": "h20",
					"publicId": "h20/org/special",
					"upstreamModel": "org/special",
					"aliases": ["org/special"],
					"routeValue": "org/special",
					"capabilities": { "openai.chat.completions": true }
				}
			]
		}
	}`)
}

func TestRegistryFourCallShapes(t *testing.T) {
	reg, err := parseRegistry(gjson.ParseBytes(sampleRegistryJSON()))
	require.NoError(t, err)
	require.True(t, reg.Enabled)

	cases := []struct {
		path, model, canonical, upstream string
	}{
		{"/v1/chat/completions", "h20/qwen3.5", "h20/qwen3.5", "qwen3.5"},
		{"/v1/chat/completions", "qwen3.5", "h20/qwen3.5", "qwen3.5"},
		// bailian is reached at its own path prefix; the root path serves the
		// default group. Resolving it from / would emit a routeValue that only
		// distinguishes groups when every Ingress predicate is unique.
		{"/bailian/v1/chat/completions", "glm-5.2", "bailian/glm-5.2", "glm-5.2"},
	}
	for _, tc := range cases {
		got := reg.Resolve(tc.path, tc.model)
		require.True(t, got.OK, "%s %s: %s %s", tc.path, tc.model, got.ErrorCode, got.ErrorMessage)
		require.Equal(t, tc.canonical, got.Entry.CanonicalID)
		require.Equal(t, tc.upstream, got.Entry.UpstreamModel)
	}
}

func TestRegistryCarriesTrustedReleaseBindingAndPrice(t *testing.T) {
	reg, err := parseRegistry(gjson.ParseBytes([]byte(`{"registry":{"version":"unified-model.v1","models":[{"canonicalId":"h20/qwen3.5","groupKey":"h20","publicId":"qwen3.5","upstreamModel":"qwen3.5","routeValue":"qwen3.5","releaseId":"42","bindingId":"101","priceVersion":"501","capabilities":{"openai.chat.completions":true}}]}}`)))
	require.NoError(t, err)
	resolved := reg.Resolve("/v1/chat/completions", "qwen3.5")
	require.True(t, resolved.OK, resolved.ErrorMessage)
	require.Equal(t, "42", resolved.Entry.ReleaseID)
	require.Equal(t, "101", resolved.Entry.BindingID)
	require.Equal(t, "501", resolved.Entry.PriceVersion)
	require.Contains(t, reg.StripHeaders, "x-ai-credits-release-id")
	require.Contains(t, reg.StripHeaders, "x-ai-credits-binding-id")
	require.Contains(t, reg.StripHeaders, "x-ai-credits-price-version")
}

func TestRegistryRejects(t *testing.T) {
	reg, err := parseRegistry(gjson.ParseBytes(sampleRegistryJSON()))
	require.NoError(t, err)

	missing := reg.Resolve("/v1/chat/completions", "")
	require.Equal(t, errMissingModel, missing.ErrorCode)

	unknown := reg.Resolve("/v1/chat/completions", "no-such")
	require.Equal(t, errUnknownModel, unknown.ErrorCode)

	unknownGroup := reg.Resolve("/other/v1/chat/completions", "glm-5.2")
	require.Equal(t, errUnknownGroup, unknownGroup.ErrorCode)

	conflict := reg.Resolve("/bailian/v1/chat/completions", "bailian/glm-5.2")
	require.Equal(t, errPrefixConflict, conflict.ErrorCode)

	cross := reg.Resolve("/v1/chat/completions", "glm-5.2")
	require.Equal(t, errUnknownModel, cross.ErrorCode, "bare names must not search other groups")

	embed := reg.Resolve("/v1/embeddings", "qwen3.5")
	require.Equal(t, errUnsupportedEndpoint, embed.ErrorCode)

	slash := reg.Resolve("/v1/chat/completions", "org/special")
	require.True(t, slash.OK, slash.ErrorMessage)
	require.Equal(t, "h20/org/special", slash.Entry.CanonicalID)
}

func TestRegistryDisabledWithoutModels(t *testing.T) {
	reg, err := parseRegistry(gjson.ParseBytes([]byte(`{"modelToHeader":"x-higress-llm-model"}`)))
	require.NoError(t, err)
	require.False(t, reg.Enabled)
	require.False(t, reg.Resolve("/v1/chat/completions", "qwen3.5").OK)
}

func TestRegistryAuthorizesPerConsumer(t *testing.T) {
	reg, err := parseRegistry(gjson.ParseBytes([]byte(`{
		"registry": {
			"defaultGroupKey": "h20",
			"models": [
				{"canonicalId":"h20/qwen3.5","groupKey":"h20","upstreamModel":"qwen3.5","aliases":["qwen3.5"],"capabilities":{"openai.chat.completions":true}},
				{"canonicalId":"bailian/glm-5.2","groupKey":"bailian","upstreamModel":"glm-5.2","aliases":["glm-5.2"],"capabilities":{"openai.chat.completions":true}}
			],
			"consumers": {
				"umc-t9-a": ["h20/qwen3.5"],
				"umc-t9-b": ["bailian/glm-5.2"]
			}
		}
	}`)))
	require.NoError(t, err)
	ok, code, _ := reg.Authorize("umc-t9-a", "h20/qwen3.5")
	require.True(t, ok)
	ok, code, _ = reg.Authorize("umc-t9-a", "bailian/glm-5.2")
	require.False(t, ok)
	require.Equal(t, errUnauthorizedModel, code)
	ok, code, _ = reg.Authorize("", "h20/qwen3.5")
	require.False(t, ok)
	require.Equal(t, errUnauthenticated, code)
}

func TestUnifiedRegistryFailClosedWithoutGrants(t *testing.T) {
	models := `"version":"unified-model.v1","models":[{"canonicalId":"h20/qwen3.5","groupKey":"h20","upstreamModel":"qwen3.5","aliases":["qwen3.5"],"capabilities":{"openai.chat.completions":true}}]`

	missing, err := parseRegistry(gjson.ParseBytes([]byte(`{"registry":{` + models + `}}`)))
	require.NoError(t, err)
	ok, code, _ := missing.Authorize("umc-t9-a", "h20/qwen3.5")
	require.False(t, ok)
	require.Equal(t, errUnauthorizedModel, code)

	empty, err := parseRegistry(gjson.ParseBytes([]byte(`{"registry":{` + models + `,"consumers":{}}}`)))
	require.NoError(t, err)
	ok, code, _ = empty.Authorize("umc-t9-a", "h20/qwen3.5")
	require.False(t, ok)
	require.Equal(t, errUnauthorizedModel, code)

	malformed, err := parseRegistry(gjson.ParseBytes([]byte(`{"registry":{` + models + `,"consumers":["umc-t9-a"]}}`)))
	require.NoError(t, err)
	ok, code, _ = malformed.Authorize("umc-t9-a", "h20/qwen3.5")
	require.False(t, ok)
	require.Equal(t, errUnauthorizedModel, code)

	partial, err := parseRegistry(gjson.ParseBytes([]byte(`{"registry":{` + models + `,"consumers":{"umc-t9-a":"h20/qwen3.5"}}}`)))
	require.NoError(t, err)
	ok, _, _ = partial.Authorize("umc-t9-a", "h20/qwen3.5")
	require.False(t, ok, "non-array grant must not entitle every model")
}

func TestUnifiedRegistryFailClosedWithoutModels(t *testing.T) {
	missing, err := parseRegistry(gjson.ParseBytes([]byte(`{"registry":{"version":"unified-model.v1"}}`)))
	require.NoError(t, err)
	require.True(t, missing.Enabled)
	require.True(t, missing.publicationBlocked())
	ok, code, _ := missing.Authorize("tester", "h20/qwen3.5")
	require.False(t, ok)
	require.Equal(t, errRegistryUnavailable, code)

	empty, err := parseRegistry(gjson.ParseBytes([]byte(`{"registry":{"version":"unified-model.v1","models":[]}}`)))
	require.NoError(t, err)
	require.True(t, empty.publicationBlocked())

	malformed, err := parseRegistry(gjson.ParseBytes([]byte(`{"registry":{"version":"unified-model.v1","models":"qwen3.5"}}`)))
	require.NoError(t, err)
	require.True(t, malformed.publicationBlocked())

	invalid, err := parseRegistry(gjson.ParseBytes([]byte(`{"registry":{"version":"not-a-schema","models":[{"canonicalId":"h20/qwen3.5","groupKey":"h20","upstreamModel":"qwen3.5"}]}}`)))
	require.NoError(t, err)
	require.True(t, invalid.publicationBlocked())

	off, err := parseRegistry(gjson.ParseBytes([]byte(`{"registry":{"enable":false,"version":"unified-model.v1","models":[]},"modelToHeader":"x-higress-llm-model"}`)))
	require.NoError(t, err)
	require.False(t, off.Enabled)
	ok, _, _ = off.Authorize("tester", "h20/qwen3.5")
	require.True(t, ok)
}

func TestNoRegistryKeepsHistoricalAllow(t *testing.T) {
	reg, err := parseRegistry(gjson.ParseBytes([]byte(`{"modelToHeader":"x-higress-llm-model"}`)))
	require.NoError(t, err)
	require.False(t, reg.Enabled)
	ok, _, _ := reg.Authorize("anyone", "h20/qwen3.5")
	require.True(t, ok)
}

func TestDuplicateAliasRejected(t *testing.T) {
	_, err := parseRegistry(gjson.ParseBytes([]byte(`{
		"registry": {"models": [
			{"canonicalId":"h20/a","groupKey":"h20","upstreamModel":"a","aliases":["shared"]},
			{"canonicalId":"h20/b","groupKey":"h20","upstreamModel":"b","aliases":["shared"]}
		]}
	}`)))
	require.Error(t, err)
}

// Another group's model named from the root path must not resolve. Its
// routeValue only selects that group's route when every group's Ingress
// predicate is distinct; where they collide, the default group's route wins and
// the caller is served -- and billed for -- a model they did not ask for.
func TestResolveRefusesAnotherGroupFromTheRootPath(t *testing.T) {
	reg, err := parseRegistry(gjson.ParseBytes(registryConfig()))
	require.NoError(t, err)
	got := reg.Resolve("/v1/chat/completions", "bailian/glm-5.2")
	require.False(t, got.OK)
	require.Equal(t, errUnknownModel, got.ErrorCode)
	require.Contains(t, got.ErrorMessage, "bailian")
	require.Nil(t, got.Entry, "a refused resolution must not carry an entry to route on")
}

// The root entry may answer an unrecognised name with one named model, because
// the route it replaces forwards any name to a single upstream and callers have
// been sending names of their own for as long as it has existed. A group entry
// may not: answering there with another group's model is the silent
// substitution this registry exists to stop.
func TestOnlyTheRootEntryFallsBackForAnUnknownName(t *testing.T) {
	cfg := `{"registry":{"enable":true,"version":"unified-model.v1","defaultGroupKey":"h20",
	 "unknownModelFallback":"h20/deepseek-v4-flash",
	 "models":[
	  {"id":"h20/deepseek-v4-flash","canonicalId":"h20/deepseek-v4-flash","groupKey":"h20","routeValue":"deepseek-v4-flash","capabilities":{"openai.chat.completions":true}},
	  {"id":"bailian/qwen-max","canonicalId":"bailian/qwen-max","groupKey":"bailian","routeValue":"qwen-max","capabilities":{"openai.chat.completions":true}}
	 ]}}`
	reg, err := parseRegistry(gjson.Parse(cfg))
	if err != nil {
		t.Fatal(err)
	}

	// Root path, a name nobody published: served by the fallback, and said so.
	got := reg.Resolve("/v1/chat/completions", "something-nobody-published")
	if got.ErrorCode != "" {
		t.Fatalf("the root entry refused a name it is required to serve: %+v", got)
	}
	if got.Entry == nil || got.Entry.CanonicalID != "h20/deepseek-v4-flash" {
		t.Fatalf("resolved to %+v, want the root entry's fallback", got.Entry)
	}
	if !got.FellBack {
		t.Fatal("the request was served by something other than what it asked for, and the result does not say so")
	}

	// A name that does resolve is untouched by the fallback.
	got = reg.Resolve("/v1/chat/completions", "deepseek-v4-flash")
	if got.ErrorCode != "" || got.FellBack {
		t.Fatalf("a resolvable name went through the fallback: %+v", got)
	}

	// A group entry refuses, as it does with no fallback configured at all.
	got = reg.Resolve("/bailian/v1/chat/completions", "something-nobody-published")
	if got.ErrorCode == "" {
		t.Fatalf("a group entry served an unknown name: %+v", got)
	}

	// And a fallback naming a model this release does not carry resolves
	// nothing rather than becoming a pass.
	reg.UnknownModelFallback = "h20/not-published"
	got = reg.Resolve("/v1/chat/completions", "something-nobody-published")
	if got.ErrorCode == "" {
		t.Fatalf("a fallback pointing at nothing became a pass: %+v", got)
	}
}

// TestTheFallbackWithholdsItselfFromPathsNoEntryClaims covers the bailian-gc
// shape: an entry whose prefix is deeper than /<group>/v1. The plugin used to
// resolve the group from the first path segment and gave up on anything else,
// so such a path fell into the root group -- and once the root fallback
// existed, a request on one group's route was answered by another group's
// fallback model. The published prefixes make the path resolvable, and the
// fallback withholds itself from a path it cannot place.
func TestTheFallbackWithholdsItselfFromPathsNoEntryClaims(t *testing.T) {
	cfg := `{"registry":{"enable":true,"version":"unified-model.v1","defaultGroupKey":"h20",
	 "unknownModelFallback":"h20/deepseek-v4-flash",
	 "rootPathPrefixes":["/v1"],
	 "groupPaths":{"bailian-gc":["/bailian/multimodal-generation"],"bailian":["/bailian/v1"]},
	 "models":[
	  {"id":"h20/deepseek-v4-flash","canonicalId":"h20/deepseek-v4-flash","groupKey":"h20","routeValue":"deepseek-v4-flash","capabilities":{"openai.chat.completions":true}},
	  {"id":"bailian-gc/qwen-image-2.0","canonicalId":"bailian-gc/qwen-image-2.0","groupKey":"bailian-gc","routeValue":"qwen-image-2.0","capabilities":{"openai.chat.completions":true}}
	 ]}}`
	reg, err := parseRegistry(gjson.Parse(cfg))
	if err != nil {
		t.Fatal(err)
	}

	// A real model on the deep-prefix entry resolves in its own group, not in
	// the root group it used to fall into.
	got := reg.Resolve("/bailian/multimodal-generation/v1/chat/completions", "qwen-image-2.0")
	if got.ErrorCode != "" {
		t.Fatalf("the deep-prefix entry refused its own model: %+v", got)
	}
	if got.Entry == nil || got.Entry.CanonicalID != "bailian-gc/qwen-image-2.0" {
		t.Fatalf("resolved to %+v, want the bailian-gc model", got.Entry)
	}
	if got.FellBack {
		t.Fatalf("the request was served by the fallback: %+v", got)
	}

	// An unknown name there is refused, not answered by the root fallback.
	got = reg.Resolve("/bailian/multimodal-generation/v1/chat/completions", "something-nobody-published")
	if got.ErrorCode == "" || got.FellBack {
		t.Fatalf("an unknown name on a group entry was served by the root fallback: %+v", got)
	}

	// The root path keeps serving the fallback.
	got = reg.Resolve("/v1/chat/completions", "something-nobody-published")
	if got.ErrorCode != "" || !got.FellBack {
		t.Fatalf("the root path lost its fallback: %+v", got)
	}

	// And the compatibility path -- the bare root the no-host route answered
	// on -- is a root path too.
	got = reg.Resolve("/", "something-nobody-published")
	if got.ErrorCode != "" || !got.FellBack {
		t.Fatalf("the bare compatibility path lost its fallback: %+v", got)
	}
}
