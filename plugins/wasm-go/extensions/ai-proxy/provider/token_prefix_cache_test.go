package provider

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTokenPrefixCacheNormalizePromptContent(t *testing.T) {
	config := TokenPrefixCacheConfig{
		trimSpace:          true,
		collapseWhitespace: true,
		lowercase:          true,
	}

	require.Equal(t, "hello world", config.normalizePromptContent("  Hello \n\t World  "))
}

func TestTokenPrefixCachePromptPrefixHashes(t *testing.T) {
	config := TokenPrefixCacheConfig{
		trimSpace:          true,
		collapseWhitespace: true,
	}
	body := []byte(`{
		"messages": [
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"hello"},
			{"role":"user","content":"write a poem"}
		]
	}`)

	hashes := config.promptPrefixHashes("provider-a", body)

	require.Len(t, hashes, 2)
	require.Len(t, hashes[0].(string), 40)
	require.Len(t, hashes[1].(string), 40)
	require.NotEqual(t, hashes[0], hashes[1])
	require.Equal(t, hashes, config.promptPrefixHashes("provider-a", body))
	require.NotEqual(t, hashes, config.promptPrefixHashes("provider-b", body))
}

func TestProviderConfigTokenIDs(t *testing.T) {
	config := ProviderConfig{
		apiTokens: []string{"sk-a", "sk-b", "sk-c"},
	}

	ids, tokenByID := config.tokenIDs([]string{"sk-c", "sk-a"})

	require.Equal(t, []string{"2", "0"}, ids)
	require.Equal(t, "sk-c", tokenByID["2"])
	require.Equal(t, "sk-a", tokenByID["0"])
}
