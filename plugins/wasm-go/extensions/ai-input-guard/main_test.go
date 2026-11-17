package main

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

var guardConfig = func() json.RawMessage {
	data, _ := json.Marshal(map[string]any{
		"maxBodyBytes":            1 << 20,
		"defaultMaxContextTokens": 100000,
		"modelMaxContextTokens": map[string]int{
			"tiny":      1000,
			"unlimited": 0,
		},
	})
	return data
}()

func headers(path string, length int) [][2]string {
	return [][2]string{
		{":authority", "example.com"},
		{":path", path},
		{":method", "POST"},
		{"content-type", "application/json"},
		{"content-length", strconv.Itoa(length)},
	}
}

func chatBody(model, text string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": text}},
	})
	return body
}

func TestParseConfig(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		t.Run("defaults", func(t *testing.T) {
			var cfg InputGuardConfig
			require.NoError(t, parseConfig(gjson.Parse(`{}`), &cfg))
			require.Equal(t, DefaultMaxBodyBytes, cfg.maxBodyBytes)
			require.Equal(t, DefaultMaxContextTokens, cfg.defaultMaxContextTokens)
			require.Equal(t, DefaultMaxContextTokens*minCharsPerToken, cfg.skipBodyBelow)
			require.Contains(t, cfg.enableOnPathSuffix, "/messages")
		})

		t.Run("skip threshold follows the smallest enabled context", func(t *testing.T) {
			var cfg InputGuardConfig
			require.NoError(t, parseConfig(gjson.ParseBytes(guardConfig), &cfg))
			require.Equal(t, 1000*minCharsPerToken, cfg.skipBodyBelow)
			require.Equal(t, 1000, cfg.contextFor("tiny"))
			require.Equal(t, 0, cfg.contextFor("unlimited"))
			require.Equal(t, 100000, cfg.contextFor("anything-else"))
		})

		t.Run("text check disabled everywhere", func(t *testing.T) {
			var cfg InputGuardConfig
			require.NoError(t, parseConfig(gjson.Parse(`{"defaultMaxContextTokens":0}`), &cfg))
			require.Equal(t, -1, cfg.skipBodyBelow)
		})

		t.Run("negative values are rejected", func(t *testing.T) {
			var cfg InputGuardConfig
			require.Error(t, parseConfig(gjson.Parse(`{"maxBodyBytes":-1}`), &cfg))
			require.Error(t, parseConfig(gjson.Parse(`{"modelMaxContextTokens":{"m":-5}}`), &cfg))
		})
	})
}

func TestRequestHeaders(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("oversized body is rejected before it is read", func(t *testing.T) {
			host, status := test.NewTestHost(guardConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(headers("/v1/chat/completions", 2<<20))
			resp := host.GetLocalResponse()
			require.NotNil(t, resp)
			require.Equal(t, uint32(413), resp.StatusCode)
			require.Equal(t, "request_too_large", gjson.GetBytes(resp.Data, "error.code").String())
			require.Contains(t, gjson.GetBytes(resp.Data, "error.message").String(), "2097152 bytes")
		})

		t.Run("anthropic clients get the anthropic error shape", func(t *testing.T) {
			host, status := test.NewTestHost(guardConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(headers("/v1/messages?beta=true", 2<<20))
			resp := host.GetLocalResponse()
			require.NotNil(t, resp)
			require.Equal(t, uint32(413), resp.StatusCode)
			require.Equal(t, "error", gjson.GetBytes(resp.Data, "type").String())
			require.Equal(t, "request_too_large", gjson.GetBytes(resp.Data, "error.type").String())
		})

		t.Run("small body is not read", func(t *testing.T) {
			host, status := test.NewTestHost(guardConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			action := host.CallOnHttpRequestHeaders(headers("/v1/chat/completions", 3000))
			require.Equal(t, types.ActionContinue, action)
			require.Nil(t, host.GetLocalResponse())
		})

		t.Run("large json body is buffered for the text check", func(t *testing.T) {
			host, status := test.NewTestHost(guardConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			action := host.CallOnHttpRequestHeaders(headers("/v1/chat/completions", 3001))
			require.Equal(t, types.HeaderStopIteration, action)
		})

		t.Run("embeddings get the size cap but no text check", func(t *testing.T) {
			host, status := test.NewTestHost(guardConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			require.Equal(t, types.ActionContinue, host.CallOnHttpRequestHeaders(headers("/v1/embeddings", 500000)))
			require.Nil(t, host.GetLocalResponse())
			host.CallOnHttpRequestHeaders(headers("/v1/embeddings", 2<<20))
			require.NotNil(t, host.GetLocalResponse())
		})

		t.Run("other paths are untouched", func(t *testing.T) {
			host, status := test.NewTestHost(guardConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			action := host.CallOnHttpRequestHeaders(headers("/api/upload", 50<<20))
			require.Equal(t, types.ActionContinue, action)
			require.Nil(t, host.GetLocalResponse())
		})
	})
}

func TestRequestBody(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		run := func(t *testing.T, path string, body []byte) *int {
			host, status := test.NewTestHost(guardConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)
			require.Equal(t, types.HeaderStopIteration, host.CallOnHttpRequestHeaders(headers(path, len(body))))
			host.CallOnHttpRequestBody(body)
			if resp := host.GetLocalResponse(); resp != nil {
				code := int(resp.StatusCode)
				if code == 400 {
					if strings.HasSuffix(path, "/messages") {
						require.Equal(t, "invalid_request_error", gjson.GetBytes(resp.Data, "error.type").String())
					} else {
						require.Equal(t, "context_length_exceeded", gjson.GetBytes(resp.Data, "error.code").String())
					}
				}
				return &code
			}
			return nil
		}

		t.Run("text far beyond the context is rejected", func(t *testing.T) {
			// 4000 distinct-ish CJK chars -> lower bound ~1333 tokens > 1000.
			text := strings.Repeat("上下文窗口限制测试", 500)
			code := run(t, "/v1/chat/completions", chatBody("tiny", text))
			require.NotNil(t, code)
			require.Equal(t, 400, *code)
		})

		t.Run("anthropic shape for the context error", func(t *testing.T) {
			code := run(t, "/v1/messages", chatBody("tiny", strings.Repeat("上下文窗口限制测试", 500)))
			require.NotNil(t, code)
			require.Equal(t, 400, *code)
		})

		t.Run("same text is fine for a larger model", func(t *testing.T) {
			require.Nil(t, run(t, "/v1/chat/completions", chatBody("big", strings.Repeat("上下文窗口限制测试", 500))))
		})

		t.Run("model with the check disabled passes", func(t *testing.T) {
			require.Nil(t, run(t, "/v1/chat/completions", chatBody("unlimited", strings.Repeat("上下文窗口限制测试", 500))))
		})

		t.Run("image payloads are not counted as text", func(t *testing.T) {
			img := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x89PNG\r\n\x1a\n random pixels ", 20000)))
			body, _ := json.Marshal(map[string]any{
				"model": "tiny",
				"messages": []map[string]any{{"role": "user", "content": []map[string]any{
					{"type": "text", "text": "describe"},
					{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + img[:300000]}},
					{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": img[:300000]}},
				}}},
			})
			require.Nil(t, run(t, "/v1/chat/completions", body))
		})
	})
}

var dryRunConfig = func() json.RawMessage {
	data, _ := json.Marshal(map[string]any{
		"dryRun":                  true,
		"maxBodyBytes":            1 << 20,
		"defaultMaxContextTokens": 100000,
		"modelMaxContextTokens":   map[string]int{"tiny": 1000},
	})
	return data
}()

// aiLog reads the ai_log filter state, which is stored as an escaped JSON string.
func aiLog(t *testing.T, host test.TestHost) gjson.Result {
	raw, _ := host.GetProperty([]string{"ai_log"})
	var unescaped string
	require.NoError(t, json.Unmarshal([]byte(`"`+string(raw)+`"`), &unescaped))
	return gjson.Parse(unescaped)
}

func TestDryRun(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("oversized body is recorded and let through", func(t *testing.T) {
			host, status := test.NewTestHost(dryRunConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			action := host.CallOnHttpRequestHeaders(headers("/v1/chat/completions", 2<<20))
			require.Equal(t, types.ActionContinue, action)
			require.Nil(t, host.GetLocalResponse())
			require.Equal(t, "dry_run:body_too_large", aiLog(t, host).Get("input_guard").String())
		})

		t.Run("over-context input is recorded and let through", func(t *testing.T) {
			host, status := test.NewTestHost(dryRunConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			body := chatBody("tiny", strings.Repeat("上下文窗口限制测试", 500))
			require.Equal(t, types.HeaderStopIteration, host.CallOnHttpRequestHeaders(headers("/v1/chat/completions", len(body))))
			require.Equal(t, types.ActionContinue, host.CallOnHttpRequestBody(body))
			require.Nil(t, host.GetLocalResponse())
			log := aiLog(t, host)
			require.Equal(t, "dry_run:context_length_exceeded", log.Get("input_guard").String())
			require.Greater(t, log.Get("input_guard_tokens").Int(), int64(1000))
		})

		t.Run("requests within limits leave ai_log alone", func(t *testing.T) {
			host, status := test.NewTestHost(dryRunConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			body := chatBody("big", strings.Repeat("上下文窗口限制测试", 500))
			host.CallOnHttpRequestHeaders(headers("/v1/chat/completions", len(body)))
			host.CallOnHttpRequestBody(body)
			raw, _ := host.GetProperty([]string{"ai_log"})
			require.Empty(t, raw)
		})
	})
}

func TestEnforceRecordsVerdict(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(guardConfig)
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		// Fields written by other plugins must survive.
		require.NoError(t, host.SetProperty([]string{"ai_log"}, []byte(`{\"consumer\":\"maxkb\"}`)))
		host.CallOnHttpRequestHeaders(headers("/v1/chat/completions", 2<<20))
		require.NotNil(t, host.GetLocalResponse())
		log := aiLog(t, host)
		require.Equal(t, "body_too_large", log.Get("input_guard").String())
		require.Equal(t, "maxkb", log.Get("consumer").String())
	})
}

func TestEstimateIsALowerBound(t *testing.T) {
	// Pairs of (text, true token count measured with the GLM-5.3-Flash
	// tokenizer, the most compressive of the three calibrated tokenizers).
	cases := []struct {
		name   string
		text   string
		tokens int
	}{
		{"spaces", strings.Repeat(" ", 200000), 394},
		{"dashes", strings.Repeat("-", 200000), 3124},
		{"newlines", strings.Repeat("\n", 200000), 6250},
		{"same char", strings.Repeat("a", 200000), 25000},
		{"repeated word", strings.Repeat("the ", 50000), 50002},
		{"single cjk char", strings.Repeat("的", 200000), 100000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			est, _ := estimateInputTokens(chatBody("m", c.text), 1<<30)
			require.LessOrEqual(t, est, c.tokens, "estimate must not exceed the real token count")
		})
	}
}

func TestEstimateDecoding(t *testing.T) {
	t.Run("unicode escapes count as one character", func(t *testing.T) {
		// json.Marshal leaves CJK as UTF-8; force \u escapes as Python's
		// json.dumps does by default.
		escaped := []byte(`{"messages":[{"content":"` + strings.Repeat("\\u4e0a\\u4e0b", 300) + `"}]}`)
		plain := []byte(`{"messages":[{"content":"` + strings.Repeat("上下", 300) + `"}]}`)
		a, _ := estimateInputTokens(escaped, 1<<30)
		b, _ := estimateInputTokens(plain, 1<<30)
		require.Equal(t, b, a)
		// 600 wide chars plus "messages" (7 after collapsing "ss") and "content".
		require.Equal(t, (600*otherCharsPerToken+14*wideCharsPerToken+23)/24, a)
	})

	t.Run("binary detection", func(t *testing.T) {
		b64 := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\xff\xfe\xfd binary ", 300)))
		require.True(t, looksBinary([]byte(b64)))
		require.True(t, looksBinary([]byte(strings.ReplaceAll(b64, "/", "\\/"))))
		require.True(t, looksBinary([]byte("data:image/png;base64,AAAA")))
		require.False(t, looksBinary([]byte(strings.Repeat("\\u4e0a", 300))))
		require.False(t, looksBinary([]byte(strings.Repeat("plain words ", 300))))
		require.False(t, looksBinary([]byte(b64[:1000])))
	})

	t.Run("surrogate pairs decode to one rune", func(t *testing.T) {
		r, n := decodeJSONRune([]byte("\\ud840\\udc00"))
		require.Equal(t, rune(0x20000), r)
		require.Equal(t, 12, n)
	})

	t.Run("escaped quotes do not end the string", func(t *testing.T) {
		body := []byte(`{"a":"x\"y\\","b":"z"}`)
		require.Equal(t, 3, stringEnd(body, 2))
		require.Equal(t, len(`{"a":"x\"y\\`), stringEnd(body, 6))
	})

	t.Run("stops early once the limit is exceeded", func(t *testing.T) {
		text := strings.Repeat("上下文", 1_000_000)
		est, over := estimateInputTokens(chatBody("m", text), 1000)
		require.True(t, over)
		require.Less(t, est, 1100)
	})

	t.Run("incident-sized chinese text", func(t *testing.T) {
		// 81 MB of mostly Chinese text was 31.7M tokens on GLM.
		text := strings.Repeat("知识库文档片段，包含检索结果与历史消息。", 1_500_000)
		est, over := estimateInputTokens(chatBody("m", text), DefaultMaxContextTokens)
		require.True(t, over)
		require.Greater(t, est, DefaultMaxContextTokens)
	})
}
