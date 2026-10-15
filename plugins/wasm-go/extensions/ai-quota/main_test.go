// Copyright (c) 2024 Alibaba Group Holding Ltd.
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
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/higress-group/wasm-go/pkg/tokenusage"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/stretchr/testify/require"
)

// 测试配置：基础配置
var basicConfig = func() json.RawMessage {
	data, _ := json.Marshal(map[string]interface{}{
		"enable_path_suffixes": []string{
			"/v1/chat/completions",
			"/v1/messages",
			"/v1/responses",
		},
		"redis": map[string]interface{}{
			"service_name": "redis.static",
			"service_port": 6379,
			"timeout":      1000,
			"database":     0,
		},
	})
	return data
}()

// The production rules still carry the retired per-key counter's settings.
// They must keep loading after the plugin stops reading them.
var retiredSettingsConfig = func() json.RawMessage {
	data, _ := json.Marshal(map[string]interface{}{
		"admin_consumer":   "admin",
		"redis_key_prefix": "chat_quota:",
		"admin_path":       "/quota",
		"redis": map[string]interface{}{
			"service_name": "redis.static",
			"service_port": 6379,
		},
	})
	return data
}()

var missingRedisConfig = func() json.RawMessage {
	data, _ := json.Marshal(map[string]interface{}{
		"enable_path_suffixes": []string{"/v1/chat/completions"},
	})
	return data
}()

func TestParseConfig(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		t.Run("basic config", func(t *testing.T) {
			host, status := test.NewTestHost(basicConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)
			config, err := host.GetMatchConfig()
			require.NoError(t, err)
			quotaConfig := config.(*QuotaConfig)
			require.Equal(t, []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"}, quotaConfig.EnablePathSuffixes)
		})

		t.Run("retired settings still load, with the default suffixes", func(t *testing.T) {
			host, status := test.NewTestHost(retiredSettingsConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)
			config, err := host.GetMatchConfig()
			require.NoError(t, err)
			quotaConfig := config.(*QuotaConfig)
			require.Equal(t, []string{"/v1/chat/completions", "/v1/messages"}, quotaConfig.EnablePathSuffixes)
		})

		t.Run("missing redis", func(t *testing.T) {
			host, status := test.NewTestHost(missingRedisConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusFailed, status)
		})
	})
}

func TestOnHttpRequestHeaders(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("no consumer", func(t *testing.T) {
			host, status := test.NewTestHost(basicConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			action := host.CallOnHttpRequestHeaders([][2]string{
				{":authority", "example.com"},
				{":path", "/v1/chat/completions"},
				{":method", "POST"},
			})
			require.Equal(t, types.ActionContinue, action)
			response := host.GetLocalResponse()
			require.NotNil(t, response)
			require.Equal(t, uint32(http.StatusUnauthorized), response.StatusCode)
		})

		// The old admin endpoints are gone: such a path is just a path no
		// suffix matches, and the gate stays out of it.
		t.Run("retired admin path is not special", func(t *testing.T) {
			host, status := test.NewTestHost(basicConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			action := host.CallOnHttpRequestHeaders([][2]string{
				{":authority", "example.com"},
				{":path", "/v1/chat/completions/quota/refresh"},
				{":method", "POST"},
				{"x-mse-consumer", "admin"},
			})
			require.Equal(t, types.ActionContinue, action)
			require.Nil(t, host.GetLocalResponse())
			calls, _ := redisCalloutCountAndLastQuery(host)
			require.Equal(t, 0, calls)
		})
	})
}

func TestOnHttpStreamingResponseBody(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// 测试聊天完成模式的流式响应体处理
		t.Run("chat completion mode", func(t *testing.T) {
			host, status := test.NewTestHost(basicConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			// 先设置请求头
			host.CallOnHttpRequestHeaders([][2]string{
				{":authority", "example.com"},
				{":path", "/v1/chat/completions"},
				{":method", "POST"},
				{"x-mse-consumer", "consumer1"},
			})
			admitFunded(host)

			// 测试流式响应体处理
			data := []byte(`{"choices": [{"delta": {"content": "Hello"}}], "usage": {"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7}}`)
			action := host.CallOnHttpStreamingResponseBody(data, false)

			require.Equal(t, types.ActionContinue, action)
			result := host.GetResponseBody()
			// 非结束流应该返回原始数据
			require.Equal(t, data, result)

			// 测试结束流
			action = host.CallOnHttpStreamingResponseBody(data, true)

			require.Equal(t, types.ActionContinue, action)
			result = host.GetResponseBody()
			// 结束流应该返回原始数据
			require.Equal(t, data, result)

			calls, query := redisCalloutCountAndLastQuery(host)
			require.Equal(t, 1, calls)
			require.Contains(t, query, "quota_wallet:u:1")
			host.CallOnRedisCall(0, test.CreateRedisRespArray([]interface{}{"ok", 30}))

			host.CompleteHttp()
		})

		// 测试非聊天完成模式的流式响应体处理
		t.Run("non-chat completion mode", func(t *testing.T) {
			host, status := test.NewTestHost(basicConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			// 先设置请求头
			host.CallOnHttpRequestHeaders([][2]string{
				{":authority", "example.com"},
				{":path", "/other/path"},
				{":method", "GET"},
				{"x-mse-consumer", "consumer1"},
			})

			// 测试流式响应体处理
			data := []byte("response data")
			action := host.CallOnHttpStreamingResponseBody(data, false)

			// 非聊天完成模式应该返回原始数据
			require.Equal(t, types.ActionContinue, action)
			result := host.GetResponseBody()
			require.Equal(t, data, result)
		})
	})
}

func TestSemanticStreamEndChargesExactlyOnce(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		tests := []struct {
			name          string
			path          string
			requestBody   []byte
			chunks        [][]byte
			terminalChunk []byte
			wantTotal     string
		}{
			{
				name:        "openai chat final usage without transport eof",
				path:        "/v1/chat/completions",
				requestBody: []byte(`{"model":"glm-5.2","stream":true}`),
				chunks: [][]byte{
					[]byte(`data: {"model":"glm-5.2","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`),
				},
				terminalChunk: []byte(`data: {"model":"glm-5.2","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`),
				wantTotal:     "12",
			},
			{
				name:        "openai responses incomplete without transport eof",
				path:        "/v1/responses",
				requestBody: []byte(`{"model":"gpt-5","stream":true}`),
				chunks: [][]byte{
					[]byte("event: response.incomplete\n"),
				},
				terminalChunk: []byte(`data: {"response":{"id":"resp_1","model":"gpt-5","usage":{"input_tokens":9,"output_tokens":3,"total_tokens":12}}}`),
				wantTotal:     "12",
			},
			{
				name:        "anthropic final delta includes cache tokens",
				path:        "/v1/messages",
				requestBody: []byte(`{"model":"qwen3.8-max-preview","stream":true}`),
				chunks: [][]byte{
					[]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"qwen3.8-max-preview","usage":{"input_tokens":20,"output_tokens":0,"cache_read_input_tokens":80,"cache_creation_input_tokens":15}}}`),
				},
				terminalChunk: []byte(`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}`),
				wantTotal: "125",
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				host, status := test.NewTestHost(basicConfig)
				defer host.Reset()
				require.Equal(t, types.OnPluginStartStatusOK, status)

				host.CallOnHttpRequestHeaders([][2]string{
					{":authority", "example.com"},
					{":path", tc.path},
					{":method", "POST"},
					{"x-mse-consumer", "consumer1"},
				})
				admitFunded(host)
				host.CallOnHttpRequestBody(tc.requestBody)

				for _, chunk := range tc.chunks {
					host.CallOnHttpStreamingResponseBody(chunk, false)
				}
				calls, _ := redisCalloutCountAndLastQuery(host)
				require.Equal(t, 0, calls)

				host.CallOnHttpStreamingResponseBody(tc.terminalChunk, false)
				calls, query := redisCalloutCountAndLastQuery(host)
				require.Equal(t, 1, calls)
				require.Contains(t, query, tc.wantTotal)

				// A later protocol sentinel and transport EOF must not charge again.
				host.CallOnHttpStreamingResponseBody([]byte("data: [DONE]\n\n"), false)
				host.CallOnHttpStreamingResponseBody(nil, true)
				calls, _ = redisCalloutCountAndLastQuery(host)
				require.Equal(t, 1, calls)
			})
		}
	})
}

// A non-streaming body arrives in as many chunks as Envoy likes, and the usage
// object at its end cannot be parsed out of a fragment. The body is buffered and
// charged whole, exactly once.
func TestNonStreamingResponseIsChargedWhole(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		tests := []struct {
			name        string
			path        string
			contentType string
			body        string
			wantTotal   int
		}{
			{
				name:        "openai chat completion",
				path:        "/v1/chat/completions",
				contentType: "application/json; charset=utf-8",
				body:        `{"id":"chatcmpl-1","object":"chat.completion","model":"qwen3.8-flash","choices":[{"index":0,"message":{"role":"assistant","content":"a long answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"total_tokens":3385,"completion_tokens":3335}}`,
				wantTotal:   3385,
			},
			{
				name:        "anthropic message includes cache tokens",
				path:        "/v1/messages",
				contentType: "application/json",
				body:        `{"id":"msg_1","type":"message","model":"qwen3.8-max-preview","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":10,"cache_read_input_tokens":80,"cache_creation_input_tokens":15}}`,
				wantTotal:   125,
			},
			{
				name:      "no content type is not taken for a stream",
				path:      "/v1/chat/completions",
				body:      `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
				wantTotal: 7,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				host, status := test.NewTestHost(basicConfig)
				defer host.Reset()
				require.Equal(t, types.OnPluginStartStatusOK, status)

				host.CallOnHttpRequestHeaders([][2]string{
					{":authority", "example.com"},
					{":path", tc.path},
					{":method", "POST"},
					{"x-mse-consumer", "consumer1"},
				})
				admitFunded(host)
				headers := [][2]string{{":status", "200"}}
				if tc.contentType != "" {
					headers = append(headers, [2]string{"content-type", tc.contentType})
				}
				require.Equal(t, types.ActionContinue, host.CallOnHttpResponseHeaders(headers))

				// Three chunks, the last cut through the middle of the usage object.
				body := []byte(tc.body)
				usageAt := strings.Index(tc.body, `"usage"`)
				cuts := []int{usageAt / 2, usageAt + 12, len(body)}
				previous := 0
				for i, cut := range cuts {
					endOfStream := i == len(cuts)-1
					action := host.CallOnHttpStreamingResponseBody(body[previous:cut], endOfStream)
					previous = cut
					if !endOfStream {
						require.Equal(t, types.ActionPause, action, "a partial body is held back")
						calls, _ := redisCalloutCountAndLastQuery(host)
						require.Equal(t, 0, calls, "nothing is charged before the body is whole")
						continue
					}
					require.Equal(t, types.ActionContinue, action)
				}
				require.Equal(t, body, host.GetResponseBody(), "the client gets the body unchanged")

				calls, query := redisCalloutCountAndLastQuery(host)
				require.Equal(t, 1, calls)
				total := strconv.Itoa(tc.wantTotal)
				require.Contains(t, query, fmt.Sprintf("$%d\r\n%s\r\n", len(total), total))
				host.CallOnRedisCall(0, test.CreateRedisRespArray([]interface{}{"ok", 30}))
				host.CompleteHttp()
			})
		}
	})
}

// An SSE response is still passed through chunk by chunk and charged on its
// semantic end, not held back until the stream closes.
func TestEventStreamResponseIsNotBuffered(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(basicConfig)
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		completionRequest(host)
		admitFunded(host)
		host.CallOnHttpResponseHeaders([][2]string{
			{":status", "200"},
			{"content-type", "text/event-stream; charset=utf-8"},
		})

		first := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		require.Equal(t, types.ActionContinue, host.CallOnHttpStreamingResponseBody(first, false))
		require.Equal(t, first, host.GetResponseBody())

		host.CallOnHttpStreamingResponseBody([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n"), false)
		calls, query := redisCalloutCountAndLastQuery(host)
		require.Equal(t, 1, calls)
		require.Contains(t, query, "$1\r\n7\r\n")

		host.CallOnHttpStreamingResponseBody([]byte("data: [DONE]\n\n"), true)
		calls, _ = redisCalloutCountAndLastQuery(host)
		require.Equal(t, 1, calls)
	})
}

// Responses on a path the quota does not govern are never read.
func TestUngovernedResponseIsNotRead(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		host, status := test.NewTestHost(basicConfig)
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		host.CallOnHttpRequestHeaders([][2]string{
			{":authority", "example.com"},
			{":path", "/v1/models"},
			{":method", "GET"},
			{"x-mse-consumer", "consumer1"},
		})
		host.CallOnHttpResponseHeaders([][2]string{{":status", "200"}, {"content-type", "application/json"}})
		data := []byte(`{"data":[],"usage":{"total_tokens":7}}`)
		require.Equal(t, types.ActionContinue, host.CallOnHttpStreamingResponseBody(data, false))
		host.CallOnHttpStreamingResponseBody(nil, true)
		calls, _ := redisCalloutCountAndLastQuery(host)
		require.Equal(t, 0, calls)
	})
}

type contextOnlyHTTPContext struct {
	wrapper.HttpContext
	values map[string]interface{}
}

func (c *contextOnlyHTTPContext) GetContext(key string) interface{} { return c.values[key] }

func (c *contextOnlyHTTPContext) GetBoolContext(key string, defaultValue bool) bool {
	if value, ok := c.values[key].(bool); ok {
		return value
	}
	return defaultValue
}

// A consumer that is missing or not a string is skipped, not a panic.
func TestChargeSkipsAnUnusableConsumer(t *testing.T) {
	for _, consumer := range []interface{}{nil, 42, ""} {
		ctx := &contextOnlyHTTPContext{values: map[string]interface{}{
			"consumer":                   consumer,
			tokenusage.CtxKeyTotalToken:  int64(7),
			tokenusage.CtxKeyInputToken:  int64(3),
			tokenusage.CtxKeyOutputToken: int64(4),
		}}
		require.NotPanics(t, func() { chargeQuotaOnce(ctx, QuotaConfig{}) }, "consumer %v", consumer)
	}
}

func redisCalloutCountAndLastQuery(host test.TestHost) (int, string) {
	count := 0
	lastQuery := ""
	// proxytest uses a process-global monotonically increasing context ID, so
	// earlier tests may have advanced it well beyond the first few values.
	for contextID := uint32(0); contextID < 1<<16; contextID++ {
		for _, callout := range host.GetRedisCalloutAttributesFromContext(contextID) {
			count++
			lastQuery = string(callout.Query)
		}
	}
	return count, lastQuery
}

func TestGetQuotaToken(t *testing.T) {
	tests := []struct {
		name        string
		totalToken  any
		inputToken  any
		outputToken any
		wantToken   int64
		wantOK      bool
	}{
		{
			name:        "prefer total token",
			totalToken:  int64(7),
			inputToken:  int64(1),
			outputToken: int64(2),
			wantToken:   7,
			wantOK:      true,
		},
		{
			name:        "fallback to input plus output",
			totalToken:  int64(0),
			inputToken:  int64(1),
			outputToken: int64(2),
			wantToken:   3,
			wantOK:      true,
		},
		{
			name:        "missing input output",
			totalToken:  nil,
			inputToken:  nil,
			outputToken: int64(2),
			wantToken:   0,
			wantOK:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, ok := getQuotaToken(tt.totalToken, tt.inputToken, tt.outputToken)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantToken, token)
		})
	}
}

func TestGetOperationMode(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		suffixes []string
		chatMode ChatMode
	}{
		{"chat completion", "/v1/chat/completions", []string{"/v1/chat/completions", "/v1/messages"}, ChatModeCompletion},
		{"anthropic messages", "/v1/messages", []string{"/v1/chat/completions", "/v1/messages"}, ChatModeCompletion},
		{"custom suffix", "/llm/invoke", []string{"/invoke"}, ChatModeCompletion},
		{"retired admin path", "/v1/chat/completions/quota", []string{"/v1/chat/completions"}, ChatModeNone},
		{"other path", "/other/path", []string{"/v1/chat/completions", "/v1/messages"}, ChatModeNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.chatMode, getOperationMode(tt.path, tt.suffixes))
		})
	}
}
