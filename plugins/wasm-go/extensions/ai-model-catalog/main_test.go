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
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

var catalogConfig = mustConfig(map[string]interface{}{
	"created": 1700000000,
	"models": []map[string]interface{}{
		{"id": "qwen3.5", "ownedBy": "yuexiu-private"},
		{"id": "deepseek-v4-flash", "ownedBy": "yuexiu-private"},
		{"id": "PaddleOCR-VL-1.6", "ownedBy": "yuexiu-private", "created": 1750000000},
	},
	"consumers": map[string][]string{
		"u-alice-default-aaaaa": {"qwen3.5", "deepseek-v4-flash"},
		"u-bob-default-bbbbb":   {"PaddleOCR-VL-1.6"},
		"u-carol-default-ccccc": {},
	},
})

func mustConfig(value map[string]interface{}) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func requestHeaders(method, path, consumer string) [][2]string {
	headers := [][2]string{
		{":authority", "aigw.example.com"},
		{":path", path},
		{":method", method},
	}
	if consumer != "" {
		headers = append(headers, [2]string{"x-mse-consumer", consumer})
	}
	return headers
}

func TestMatchPath(t *testing.T) {
	tests := []struct {
		path      string
		wantModel string
		wantMatch bool
	}{
		{"/v1/models", "", true},
		{"/v1/models/", "", true},
		{"/v1/models?limit=10", "", true},
		{"/v1/models/qwen3.5", "qwen3.5", true},
		{"/v1/models/qwen3.5/", "qwen3.5", true},
		{"/v1/models/qwen3.5?x=1", "qwen3.5", true},
		{"/V1/Models", "", true},
		{"/v1/modelsfoo", "", false},
		{"/v1/chat/completions", "", false},
		{"/", "", false},
	}
	for _, tt := range tests {
		model, matched := matchPath("/v1/models", tt.path)
		require.Equal(t, tt.wantMatch, matched, "path %s", tt.path)
		require.Equal(t, tt.wantModel, model, "path %s", tt.path)
	}
}

func TestParseConfig(t *testing.T) {
	test.RunGoTest(t, func(t *testing.T) {
		t.Run("valid config", func(t *testing.T) {
			host, status := test.NewTestHost(catalogConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			parsed, err := host.GetMatchConfig()
			require.NoError(t, err)
			require.NotNil(t, parsed)
		})

		t.Run("entitlement to an unknown model is dropped, not fatal", func(t *testing.T) {
			host, status := test.NewTestHost(mustConfig(map[string]interface{}{
				"models":    []map[string]interface{}{{"id": "qwen3.5"}},
				"consumers": map[string][]string{"u-alice": {"qwen3.5", "model-that-was-deleted"}},
			}))
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			require.Equal(t, types.ActionPause,
				host.CallOnHttpRequestHeaders(requestHeaders("GET", "/v1/models", "u-alice")))
			require.Equal(t, []string{"qwen3.5"}, modelIDs(t, host.GetLocalResponse().Data))
			host.CompleteHttp()
		})

		t.Run("empty models is rejected", func(t *testing.T) {
			host, status := test.NewTestHost(mustConfig(map[string]interface{}{
				"consumers": map[string][]string{"u-alice": {"qwen3.5"}},
			}))
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusFailed, status)
		})

		t.Run("duplicate model id is rejected", func(t *testing.T) {
			host, status := test.NewTestHost(mustConfig(map[string]interface{}{
				"models": []map[string]interface{}{{"id": "qwen3.5"}, {"id": "qwen3.5"}},
			}))
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusFailed, status)
		})
	})
}

func TestListModels(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("returns only the models the consumer is entitled to", func(t *testing.T) {
			host, status := test.NewTestHost(catalogConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			action := host.CallOnHttpRequestHeaders(requestHeaders("GET", "/v1/models", "u-alice-default-aaaaa"))
			require.Equal(t, types.ActionPause, action)

			response := host.GetLocalResponse()
			require.Equal(t, uint32(200), response.StatusCode)
			require.Contains(t, response.Headers, [2]string{"content-type", "application/json; charset=utf-8"})

			var body listResponse
			require.NoError(t, json.Unmarshal(response.Data, &body))
			require.Equal(t, "list", body.Object)
			require.Len(t, body.Data, 2)
			require.Equal(t, "qwen3.5", body.Data[0].ID)
			require.Equal(t, "model", body.Data[0].Object)
			require.Equal(t, "yuexiu-private", body.Data[0].OwnedBy)
			require.Equal(t, int64(1700000000), body.Data[0].Created)
			require.Equal(t, "deepseek-v4-flash", body.Data[1].ID)

			host.CompleteHttp()
		})

		t.Run("response order follows the configured catalog order", func(t *testing.T) {
			host, status := test.NewTestHost(catalogConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.CallOnHttpRequestHeaders(requestHeaders("GET", "/v1/models", "u-bob-default-bbbbb"))
			require.Equal(t, []string{"PaddleOCR-VL-1.6"}, modelIDs(t, host.GetLocalResponse().Data))
			host.CompleteHttp()
		})

		t.Run("per-model created overrides the catalog default", func(t *testing.T) {
			host, status := test.NewTestHost(catalogConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.CallOnHttpRequestHeaders(requestHeaders("GET", "/v1/models", "u-bob-default-bbbbb"))
			var body listResponse
			require.NoError(t, json.Unmarshal(host.GetLocalResponse().Data, &body))
			require.Equal(t, int64(1750000000), body.Data[0].Created)
			host.CompleteHttp()
		})

		t.Run("entitled to nothing yields an empty list, not an error", func(t *testing.T) {
			host, status := test.NewTestHost(catalogConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.CallOnHttpRequestHeaders(requestHeaders("GET", "/v1/models", "u-carol-default-ccccc"))

			response := host.GetLocalResponse()
			require.Equal(t, uint32(200), response.StatusCode)
			require.JSONEq(t, `{"object":"list","data":[]}`, string(response.Data))
			host.CompleteHttp()
		})

		t.Run("consumer the catalog has never heard of yields an empty list", func(t *testing.T) {
			host, status := test.NewTestHost(catalogConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.CallOnHttpRequestHeaders(requestHeaders("GET", "/v1/models", "u-not-synced-yet"))

			response := host.GetLocalResponse()
			require.Equal(t, uint32(200), response.StatusCode)
			require.JSONEq(t, `{"object":"list","data":[]}`, string(response.Data))
			host.CompleteHttp()
		})
	})
}

func TestRetrieveModel(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("entitled model is returned as a bare model object", func(t *testing.T) {
			host, status := test.NewTestHost(catalogConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			action := host.CallOnHttpRequestHeaders(
				requestHeaders("GET", "/v1/models/deepseek-v4-flash", "u-alice-default-aaaaa"))
			require.Equal(t, types.ActionPause, action)

			response := host.GetLocalResponse()
			require.Equal(t, uint32(200), response.StatusCode)
			require.JSONEq(t,
				`{"id":"deepseek-v4-flash","object":"model","created":1700000000,"owned_by":"yuexiu-private"}`,
				string(response.Data))
			host.CompleteHttp()
		})

		t.Run("a model that exists but is not entitled is indistinguishable from a missing one", func(t *testing.T) {
			host, status := test.NewTestHost(catalogConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.CallOnHttpRequestHeaders(
				requestHeaders("GET", "/v1/models/PaddleOCR-VL-1.6", "u-alice-default-aaaaa"))
			notEntitled := host.GetLocalResponse()
			host.CompleteHttp()

			host.InitHttp()
			host.CallOnHttpRequestHeaders(
				requestHeaders("GET", "/v1/models/no-such-model", "u-alice-default-aaaaa"))
			missing := host.GetLocalResponse()
			host.CompleteHttp()

			require.Equal(t, uint32(404), notEntitled.StatusCode)
			require.Equal(t, uint32(404), missing.StatusCode)
			require.Equal(t, errorType(t, notEntitled.Data), errorType(t, missing.Data))
		})
	})
}

func TestRejectedRequests(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("missing consumer header is unauthenticated", func(t *testing.T) {
			host, status := test.NewTestHost(catalogConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			action := host.CallOnHttpRequestHeaders(requestHeaders("GET", "/v1/models", ""))
			require.Equal(t, types.ActionPause, action)

			response := host.GetLocalResponse()
			require.Equal(t, uint32(401), response.StatusCode)
			// The catalog must not leak to a caller key-auth never identified.
			require.NotContains(t, string(response.Data), "qwen3.5")
			host.CompleteHttp()
		})

		t.Run("blank consumer header is unauthenticated", func(t *testing.T) {
			host, status := test.NewTestHost(catalogConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.CallOnHttpRequestHeaders(requestHeaders("GET", "/v1/models", "   "))
			require.Equal(t, uint32(401), host.GetLocalResponse().StatusCode)
			host.CompleteHttp()
		})

		t.Run("POST to the catalog is rejected before authentication is considered", func(t *testing.T) {
			host, status := test.NewTestHost(catalogConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.CallOnHttpRequestHeaders(requestHeaders("POST", "/v1/models", "u-alice-default-aaaaa"))
			require.Equal(t, uint32(405), host.GetLocalResponse().StatusCode)
			host.CompleteHttp()
		})

		t.Run("requests off the catalog path are passed through untouched", func(t *testing.T) {
			host, status := test.NewTestHost(catalogConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			action := host.CallOnHttpRequestHeaders(
				requestHeaders("POST", "/v1/chat/completions", "u-alice-default-aaaaa"))
			require.Equal(t, types.ActionContinue, action)
			require.Nil(t, host.GetLocalResponse())
			host.CompleteHttp()
		})
	})
}

func TestCustomConfiguration(t *testing.T) {
	test.RunTest(t, func(t *testing.T) {
		t.Run("path and consumerHeader are configurable", func(t *testing.T) {
			host, status := test.NewTestHost(mustConfig(map[string]interface{}{
				"path":           "/private/v1/models",
				"consumerHeader": "x-custom-consumer",
				"created":        1700000000,
				"models":         []map[string]interface{}{{"id": "qwen3.5"}},
				"consumers":      map[string][]string{"u-alice": {"qwen3.5"}},
			}))
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			require.Equal(t, types.ActionContinue,
				host.CallOnHttpRequestHeaders(requestHeaders("GET", "/v1/models", "u-alice")))
			host.CompleteHttp()

			host.InitHttp()
			action := host.CallOnHttpRequestHeaders([][2]string{
				{":authority", "aigw.example.com"},
				{":path", "/private/v1/models"},
				{":method", "GET"},
				{"x-custom-consumer", "u-alice"},
			})
			require.Equal(t, types.ActionPause, action)
			require.Equal(t, []string{"qwen3.5"}, modelIDs(t, host.GetLocalResponse().Data))
			host.CompleteHttp()
		})

		t.Run("ownedBy defaults when unset", func(t *testing.T) {
			host, status := test.NewTestHost(mustConfig(map[string]interface{}{
				"models":    []map[string]interface{}{{"id": "qwen3.5"}},
				"consumers": map[string][]string{"u-alice": {"qwen3.5"}},
			}))
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.InitHttp()
			host.CallOnHttpRequestHeaders(requestHeaders("GET", "/v1/models", "u-alice"))
			var body listResponse
			require.NoError(t, json.Unmarshal(host.GetLocalResponse().Data, &body))
			require.Equal(t, defaultOwnedBy, body.Data[0].OwnedBy)
			host.CompleteHttp()
		})
	})
}

func modelIDs(t *testing.T, data []byte) []string {
	t.Helper()
	var body listResponse
	require.NoError(t, json.Unmarshal(data, &body))
	ids := make([]string, 0, len(body.Data))
	for _, item := range body.Data {
		ids = append(ids, item.ID)
	}
	return ids
}

func errorType(t *testing.T, data []byte) string {
	t.Helper()
	var body errorResponse
	require.NoError(t, json.Unmarshal(data, &body))
	return body.Error.Type
}
