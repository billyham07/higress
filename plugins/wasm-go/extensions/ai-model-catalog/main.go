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
	"fmt"
	"strings"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

const (
	pluginName            = "ai-model-catalog"
	defaultConsumerHeader = "x-mse-consumer"
	defaultBasePath       = "/v1/models"
	defaultOwnedBy        = "higress"
)

var jsonHeaders = [][2]string{{"content-type", "application/json; charset=utf-8"}}

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
	)
}

type Model struct {
	ID      string
	OwnedBy string
	Created int64
}

type Config struct {
	consumerHeader string
	basePath       string
	// models keeps declaration order, which is also the response order: the
	// control plane decides how models are sorted, the gateway does not re-sort.
	models     []Model
	modelIndex map[string]int
	consumers  map[string]map[string]struct{}
}

func parseConfig(cfg gjson.Result, config *Config) error {
	config.consumerHeader = strings.TrimSpace(cfg.Get("consumerHeader").String())
	if config.consumerHeader == "" {
		config.consumerHeader = defaultConsumerHeader
	}

	basePath := strings.TrimSpace(cfg.Get("path").String())
	if basePath == "" {
		basePath = defaultBasePath
	}
	config.basePath = "/" + strings.Trim(basePath, "/")

	// A single timestamp for the whole catalog keeps the response stable across
	// requests; clients treat `created` as cache metadata, not as a real event.
	created := cfg.Get("created").Int()
	if created <= 0 {
		created = time.Now().Unix()
	}

	config.modelIndex = make(map[string]int)
	for _, item := range cfg.Get("models").Array() {
		id := strings.TrimSpace(item.Get("id").String())
		if id == "" {
			return errors.New("models[].id must not be empty")
		}
		if _, dup := config.modelIndex[id]; dup {
			return fmt.Errorf("duplicate model id %q", id)
		}
		ownedBy := strings.TrimSpace(item.Get("ownedBy").String())
		if ownedBy == "" {
			ownedBy = defaultOwnedBy
		}
		modelCreated := item.Get("created").Int()
		if modelCreated <= 0 {
			modelCreated = created
		}
		config.modelIndex[id] = len(config.models)
		config.models = append(config.models, Model{ID: id, OwnedBy: ownedBy, Created: modelCreated})
	}
	if len(config.models) == 0 {
		return errors.New("at least one entry in models is required")
	}

	config.consumers = make(map[string]map[string]struct{})
	cfg.Get("consumers").ForEach(func(name, models gjson.Result) bool {
		consumer := strings.TrimSpace(name.String())
		if consumer == "" {
			return true
		}
		allowed := make(map[string]struct{})
		for _, entry := range models.Array() {
			id := strings.TrimSpace(entry.String())
			if id == "" {
				continue
			}
			if _, known := config.modelIndex[id]; !known {
				// An entitlement pointing at a model we were never told about is a
				// sync bug upstream. Dropping the entry degrades one consumer's
				// list; rejecting the config would take the whole route down.
				log.Warnf("consumer %q references unknown model %q, ignored", consumer, id)
				continue
			}
			allowed[id] = struct{}{}
		}
		config.consumers[consumer] = allowed
		return true
	})
	return nil
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	requestedModel, matched := matchPath(config.basePath, ctx.Path())
	if !matched {
		return types.ActionContinue
	}

	if method := ctx.Method(); method != "GET" && method != "HEAD" {
		return sendError(405, "invalid_request_error",
			fmt.Sprintf("method %s is not supported on %s", method, config.basePath))
	}

	consumer, err := proxywasm.GetHttpRequestHeader(config.consumerHeader)
	if err != nil || strings.TrimSpace(consumer) == "" {
		// key-auth injects this header once it has authenticated the caller. Its
		// absence means the request was never authenticated, and answering would
		// hand the whole catalog to an anonymous client.
		return sendError(401, "invalid_request_error", "request is not authenticated")
	}

	// A consumer with no entry is authenticated but not yet entitled to anything
	// (new key, or the catalog sync has not caught up). That is an empty list,
	// not an error.
	allowed := config.consumers[strings.TrimSpace(consumer)]

	if requestedModel == "" {
		return sendModelList(config, allowed)
	}
	return sendSingleModel(config, allowed, requestedModel)
}

// matchPath reports whether path targets the catalog endpoint. It returns the
// requested model id for the single-model form (`/v1/models/<id>`) and an empty
// id for the list form (`/v1/models`).
func matchPath(basePath, rawPath string) (string, bool) {
	path := rawPath
	if cut := strings.IndexAny(path, "?#"); cut >= 0 {
		path = path[:cut]
	}
	if trimmed := strings.TrimSuffix(path, "/"); trimmed != "" {
		path = trimmed
	}
	if strings.EqualFold(path, basePath) {
		return "", true
	}
	prefix := basePath + "/"
	if len(path) > len(prefix) && strings.EqualFold(path[:len(prefix)], prefix) {
		return path[len(prefix):], true
	}
	return "", false
}

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type listResponse struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type errorResponse struct {
	Error errorDetail `json:"error"`
}

func sendModelList(config Config, allowed map[string]struct{}) types.Action {
	body := listResponse{Object: "list", Data: make([]modelObject, 0, len(allowed))}
	for _, model := range config.models {
		if _, ok := allowed[model.ID]; !ok {
			continue
		}
		body.Data = append(body.Data, toModelObject(model))
	}
	return sendJSON(200, body)
}

func sendSingleModel(config Config, allowed map[string]struct{}, id string) types.Action {
	if _, ok := allowed[id]; ok {
		return sendJSON(200, toModelObject(config.models[config.modelIndex[id]]))
	}
	// Unknown and not-entitled are deliberately the same answer: telling an
	// unentitled caller that a model exists leaks the private catalog.
	return sendError(404, "invalid_request_error",
		fmt.Sprintf("the model '%s' does not exist or you do not have access to it", id))
}

func toModelObject(model Model) modelObject {
	return modelObject{ID: model.ID, Object: "model", Created: model.Created, OwnedBy: model.OwnedBy}
}

func sendError(status uint32, errorType, message string) types.Action {
	return sendJSON(status, errorResponse{Error: errorDetail{Message: message, Type: errorType}})
}

func sendJSON(status uint32, payload interface{}) types.Action {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Errorf("marshal response failed: %v", err)
		status = 500
		data = []byte(`{"error":{"message":"internal error","type":"server_error"}}`)
	}
	if err := proxywasm.SendHttpResponseWithDetail(status, pluginName, jsonHeaders, data, -1); err != nil {
		log.Errorf("send http response failed: %v", err)
	}
	return types.ActionPause
}
