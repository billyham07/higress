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
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
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

// 测试配置：缺少redis
var missingRedisConfig = func() json.RawMessage {
	data, _ := json.Marshal(map[string]interface{}{
		"enable_path_suffixes": []string{"/v1/chat/completions"},
	})
	return data
}()

var defaultPathSuffixesConfig = func() json.RawMessage {
	data, _ := json.Marshal(map[string]interface{}{
		"redis": map[string]interface{}{
			"service_name": "redis.static",
			"service_port": 6379,
		},
	})
	return data
}()

func TestParseConfig(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		// 测试基础配置解析
		t.Run("basic config", func(t *testing.T) {
			host, status := test.NewTestHost(basicConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)
			config, err := host.GetMatchConfig()
			require.NoError(t, err)
			require.NotNil(t, config)

			quotaConfig := config.(*QuotaConfig)
			require.Equal(t, []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"}, quotaConfig.EnablePathSuffixes)
		})

		// 测试缺少redis的配置
		t.Run("missing redis", func(t *testing.T) {
			host, status := test.NewTestHost(missingRedisConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusFailed, status)
		})

		t.Run("default path suffixes", func(t *testing.T) {
			host, status := test.NewTestHost(defaultPathSuffixesConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)
			config, err := host.GetMatchConfig()
			require.NoError(t, err)
			require.NotNil(t, config)

			quotaConfig := config.(*QuotaConfig)
			require.Equal(t, []string{"/v1/chat/completions", "/v1/messages"}, quotaConfig.EnablePathSuffixes)
		})
	})
}

func TestOnHttpRequestHeaders(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// 测试聊天完成模式的请求头处理
		t.Run("chat completion mode", func(t *testing.T) {
			host, status := test.NewTestHost(basicConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			// 设置请求头，包含consumer信息
			action := host.CallOnHttpRequestHeaders([][2]string{
				{":authority", "example.com"},
				{":path", "/v1/chat/completions"},
				{":method", "POST"},
				{"x-mse-consumer", "consumer1"},
			})

			// 由于需要调用Redis检查配额，应该返回HeaderStopAllIterationAndWatermark
			require.Equal(t, types.HeaderStopAllIterationAndWatermark, action)

			// Key 与钱包都在、钱包有余额：放行
			admitFunded(host)
			action = host.GetHttpStreamAction()
			require.Equal(t, types.ActionContinue, action)
			host.CompleteHttp()
		})

		// 测试无consumer的情况
		t.Run("no consumer", func(t *testing.T) {
			host, status := test.NewTestHost(basicConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			// 设置请求头，不包含consumer信息
			action := host.CallOnHttpRequestHeaders([][2]string{
				{":authority", "example.com"},
				{":path", "/v1/chat/completions"},
				{":method", "POST"},
			})

			// 无consumer应该返回ActionContinue
			require.Equal(t, types.ActionContinue, action)
		})
	})
}

func TestOnHttpRequestBody(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		// 测试聊天完成模式的请求体处理
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

			// 设置请求体
			body := `{"model": "gpt-3.5-turbo", "messages": [{"role": "user", "content": "Hello"}]}`
			action := host.CallOnHttpRequestBody([]byte(body))

			// 聊天完成模式应该返回ActionContinue
			require.Equal(t, types.ActionContinue, action)
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

			// 测试流式响应体处理
			data := []byte(`{"choices": [{"delta": {"content": "Hello"}}]}`)
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
		{"chat completion mode", "/v1/chat/completions", []string{"/v1/chat/completions", "/v1/messages"}, ChatModeCompletion},
		{"anthropic messages completion mode", "/v1/messages", []string{"/v1/chat/completions", "/v1/messages"}, ChatModeCompletion},
		{"custom suffix completion mode", "/llm/invoke", []string{"/invoke"}, ChatModeCompletion},
		// The old admin paths no longer exist: they wrote a counter nothing reads.
		{"retired admin path", "/v1/chat/completions/quota", []string{"/v1/chat/completions"}, ChatModeNone},
		{"none mode", "/other/path", []string{"/v1/chat/completions", "/v1/messages"}, ChatModeNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.chatMode, getOperationMode(tt.path, tt.suffixes))
		})
	}
}
