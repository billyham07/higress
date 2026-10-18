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
