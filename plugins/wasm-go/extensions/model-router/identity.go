package main

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	registryContractVersion = "unified-model.v1"
	defaultGroupKey         = "h20"
	groupModelSep           = "\x1f"
)

const (
	errMissingModel          = "missing_model"
	errUnknownModel          = "unknown_model"
	errUnknownGroup          = "unknown_group"
	errPrefixConflict        = "prefix_conflict"
	errUnsupportedEndpoint   = "unsupported_endpoint"
	errAmbiguousModel        = "ambiguous_model"
	errUnauthenticated       = "unauthenticated"
	errUnauthorizedModel     = "unauthorized_model"
	errRegistryUnavailable   = "unified_registry_unavailable"
	capOpenAIChatCompletions = "openai.chat.completions"
	capOpenAICompletions     = "openai.completions"
	capAnthropicMessages     = "anthropic.messages"
	capOpenAIResponses       = "openai.responses"
	capOpenAIEmbeddings      = "openai.embeddings"
	capOpenAIAudio           = "openai.audio"
	capOpenAICountTokens     = "openai.count_tokens"
	capOpenAIModels          = "openai.models"
)

var defaultInternalHeaders = []string{
	"x-higress-llm-model",
	"x-higress-llm-provider",
	"x-ai-credits-budget-subject",
	"x-ai-credits-reservation-id",
	"x-ai-credits-request-id",
	"x-ai-credits-canonical-model",
}

type ModelEntry struct {
	CanonicalID   string
	GroupKey      string
	PublicID      string
	UpstreamModel string
	Aliases       []string
	ListAliases   bool
	RouteValue    string
	ProviderValue string
	Capabilities  map[string]bool
}

type Registry struct {
	Enabled          bool
	Version          string
	ConfigVersion    string
	DefaultGroupKey  string
	StripHeaders     []string
	byCanonical      map[string]*ModelEntry
	byGroupModel     map[string]*ModelEntry
	Groups           map[string]struct{}
	Consumers        map[string]map[string]struct{}
	consumersInvalid bool
	modelsInvalid    bool
}

type ResolveResult struct {
	OK             bool
	Entry          *ModelEntry
	RequestedModel string
	PathGroup      string
	Endpoint       string
	ErrorCode      string
	ErrorMessage   string
}

func parseRegistry(json gjson.Result) (*Registry, error) {
	node := json.Get("registry")
	reg := &Registry{
		DefaultGroupKey: defaultGroupKey,
		StripHeaders:    append([]string(nil), defaultInternalHeaders...),
		byCanonical:     map[string]*ModelEntry{},
		byGroupModel:    map[string]*ModelEntry{},
		Groups:          map[string]struct{}{},
	}
	if !node.Exists() {
		return reg, nil
	}
	if node.Get("enable").Exists() && !node.Get("enable").Bool() {
		return reg, nil
	}
	if v := strings.TrimSpace(node.Get("version").String()); v != "" {
		reg.Version = v
	} else if v := strings.TrimSpace(node.Get("schemaVersion").String()); v != "" {
		reg.Version = v
	} else {
		reg.Version = registryContractVersion
	}
	if !isUnifiedRegistryVersion(reg.Version) {
		reg.Enabled = true
		reg.modelsInvalid = true
		return reg, nil
	}
	models := node.Get("models")
	if !models.Exists() || models.Type == gjson.Null || !models.IsArray() || len(models.Array()) == 0 {
		reg.Enabled = true
		reg.modelsInvalid = true
		return reg, nil
	}
	reg.ConfigVersion = strings.TrimSpace(node.Get("configVersion").String())
	if g := strings.TrimSpace(node.Get("defaultGroupKey").String()); g != "" {
		reg.DefaultGroupKey = g
	}
	if headers := node.Get("stripClientHeaders"); headers.Exists() && headers.IsArray() {
		reg.StripHeaders = nil
		for _, h := range headers.Array() {
			name := strings.ToLower(strings.TrimSpace(h.String()))
			if name != "" {
				reg.StripHeaders = append(reg.StripHeaders, name)
			}
		}
		for _, fallback := range defaultInternalHeaders {
			if !containsFold(reg.StripHeaders, fallback) {
				reg.StripHeaders = append(reg.StripHeaders, fallback)
			}
		}
	}

	for i, item := range models.Array() {
		entry, err := parseModelEntry(item, i)
		if err != nil {
			return nil, err
		}
		if _, dup := reg.byCanonical[entry.CanonicalID]; dup {
			return nil, fmt.Errorf("duplicate canonical model id %q", entry.CanonicalID)
		}
		reg.byCanonical[entry.CanonicalID] = entry
		reg.Groups[entry.GroupKey] = struct{}{}
		names := []string{entry.UpstreamModel, entry.PublicID}
		if stripped := stripGroupPrefix(entry.PublicID, entry.GroupKey); stripped != "" {
			names = append(names, stripped)
		}
		names = append(names, entry.Aliases...)
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			key := groupModelKey(entry.GroupKey, name)
			if existing := reg.byGroupModel[key]; existing != nil && existing != entry {
				return nil, fmt.Errorf("ambiguous model name %q in group %q", name, entry.GroupKey)
			}
			reg.byGroupModel[key] = entry
		}
	}
	reg.Consumers = map[string]map[string]struct{}{}
	markMalformedConsumers(reg, node.Get("consumers"))
	markMalformedConsumers(reg, json.Get("consumers"))
	applyConsumerAllowlist(reg, node.Get("consumers"))
	applyConsumerAllowlist(reg, json.Get("consumers"))
	reg.Enabled = true
	reg.Groups[reg.DefaultGroupKey] = struct{}{}
	return reg, nil
}

func markMalformedConsumers(reg *Registry, node gjson.Result) {
	if !node.Exists() || node.Type == gjson.Null {
		return
	}
	if !node.IsObject() {
		reg.consumersInvalid = true
	}
}

func isUnifiedRegistryVersion(version string) bool {
	v := strings.TrimSpace(version)
	return v == "" || v == registryContractVersion || strings.HasPrefix(v, "unified-model.")
}

func (reg *Registry) unifiedMode() bool {
	return reg != nil && reg.Enabled && (reg.modelsInvalid || isUnifiedRegistryVersion(reg.Version))
}

func (reg *Registry) publicationBlocked() bool {
	return reg != nil && reg.Enabled && (reg.modelsInvalid || len(reg.byCanonical) == 0)
}

func applyConsumerAllowlist(reg *Registry, node gjson.Result) {
	if !node.Exists() || !node.IsObject() {
		return
	}
	node.ForEach(func(name, models gjson.Result) bool {
		consumer := strings.TrimSpace(name.String())
		if consumer == "" {
			return true
		}
		if models.Exists() && models.Type != gjson.Null && !models.IsArray() {
			reg.consumersInvalid = true
			return true
		}
		allowed := reg.Consumers[consumer]
		if allowed == nil {
			allowed = map[string]struct{}{}
		}
		for _, item := range models.Array() {
			id := strings.TrimSpace(item.String())
			if id == "" {
				continue
			}
			if entry := reg.byCanonical[id]; entry != nil {
				allowed[entry.CanonicalID] = struct{}{}
				continue
			}
			if entry := reg.lookup(reg.DefaultGroupKey, id); entry != nil {
				allowed[entry.CanonicalID] = struct{}{}
			}
		}
		reg.Consumers[consumer] = allowed
		return true
	})
}

func (reg *Registry) Authorize(consumer, canonicalID string) (ok bool, code, message string) {
	if reg == nil || !reg.Enabled {
		return true, "", ""
	}
	if reg.publicationBlocked() {
		return false, errRegistryUnavailable, "unified registry has no published models"
	}
	if !reg.unifiedMode() && !reg.consumersInvalid && len(reg.Consumers) == 0 {
		return true, "", ""
	}
	consumer = strings.TrimSpace(consumer)
	if consumer == "" {
		return false, errUnauthenticated, "request is not authenticated"
	}
	if reg.consumersInvalid || len(reg.Consumers) == 0 {
		return false, errUnauthorizedModel, "unified registry has no consumer grants"
	}
	allowed := reg.Consumers[consumer]
	if _, ok := allowed[canonicalID]; !ok {
		return false, errUnauthorizedModel, fmt.Sprintf("consumer %q is not entitled to model %q", consumer, canonicalID)
	}
	return true, "", ""
}

func parseModelEntry(item gjson.Result, index int) (*ModelEntry, error) {
	canonical := strings.TrimSpace(item.Get("canonicalId").String())
	if canonical == "" {
		canonical = strings.TrimSpace(item.Get("id").String())
	}
	if canonical == "" {
		return nil, fmt.Errorf("registry.models[%d] missing canonicalId", index)
	}
	group := strings.TrimSpace(item.Get("groupKey").String())
	if group == "" {
		if prefix, rest, ok := splitKnownPrefix(canonical, nil); ok && rest != "" {
			group = prefix
		} else {
			group = defaultGroupKey
		}
	}
	upstream := strings.TrimSpace(item.Get("upstreamModel").String())
	if upstream == "" {
		if _, rest, ok := splitKnownPrefix(canonical, map[string]struct{}{group: {}}); ok && rest != "" {
			upstream = rest
		} else {
			upstream = canonical
		}
	}
	publicID := strings.TrimSpace(item.Get("publicId").String())
	if publicID == "" {
		publicID = canonical
	}
	entry := &ModelEntry{
		CanonicalID:   canonical,
		GroupKey:      group,
		PublicID:      publicID,
		UpstreamModel: upstream,
		RouteValue:    strings.TrimSpace(item.Get("routeValue").String()),
		ProviderValue: strings.TrimSpace(item.Get("providerValue").String()),
		ListAliases:   item.Get("listAliases").Bool(),
		Capabilities:  map[string]bool{},
	}
	if entry.RouteValue == "" {
		entry.RouteValue = upstream
	}
	for _, alias := range item.Get("aliases").Array() {
		value := strings.TrimSpace(alias.String())
		if value != "" {
			entry.Aliases = append(entry.Aliases, value)
		}
	}
	caps := item.Get("capabilities")
	if caps.Exists() && caps.IsObject() {
		caps.ForEach(func(key, value gjson.Result) bool {
			entry.Capabilities[strings.TrimSpace(key.String())] = value.Bool()
			return true
		})
	}
	return entry, nil
}

func (reg *Registry) Resolve(path, requestedModel string) ResolveResult {
	result := ResolveResult{
		RequestedModel: strings.TrimSpace(requestedModel),
		Endpoint:       endpointFromPath(path),
	}
	if !reg.Enabled {
		return result
	}
	pathGroup, errCode, errMsg := parsePathGroup(path, reg.Groups)
	result.PathGroup = pathGroup
	if errCode != "" {
		result.ErrorCode = errCode
		result.ErrorMessage = errMsg
		return result
	}
	if result.RequestedModel == "" {
		result.ErrorCode = errMissingModel
		result.ErrorMessage = "request is missing model"
		return result
	}
	if pathGroup != "" && (result.RequestedModel == pathGroup || strings.HasPrefix(result.RequestedModel, pathGroup+"/")) {
		result.ErrorCode = errPrefixConflict
		result.ErrorMessage = fmt.Sprintf("model %q conflicts with path group %q", result.RequestedModel, pathGroup)
		return result
	}

	if entry := reg.byCanonical[result.RequestedModel]; entry != nil {
		if pathGroup != "" && entry.GroupKey != pathGroup {
			result.ErrorCode = errUnknownModel
			result.ErrorMessage = fmt.Sprintf("model %q is not in group %q", result.RequestedModel, pathGroup)
			return result
		}
		return reg.finish(result, entry)
	}

	if i := strings.Index(result.RequestedModel, "/"); i > 0 {
		group := result.RequestedModel[:i]
		rest := result.RequestedModel[i+1:]
		if _, known := reg.Groups[group]; known {
			if pathGroup != "" && pathGroup != group {
				result.ErrorCode = errPrefixConflict
				result.ErrorMessage = fmt.Sprintf("model prefix %q conflicts with path group %q", group, pathGroup)
				return result
			}
			if entry := reg.lookup(group, rest); entry != nil {
				return reg.finish(result, entry)
			}
			result.ErrorCode = errUnknownModel
			result.ErrorMessage = fmt.Sprintf("unknown model %q", result.RequestedModel)
			return result
		}
	}

	group := pathGroup
	if group == "" {
		group = reg.DefaultGroupKey
	}
	if _, known := reg.Groups[group]; pathGroup != "" && !known {
		result.ErrorCode = errUnknownGroup
		result.ErrorMessage = fmt.Sprintf("unknown group %q", pathGroup)
		return result
	}
	if entry := reg.lookup(group, result.RequestedModel); entry != nil {
		return reg.finish(result, entry)
	}
	result.ErrorCode = errUnknownModel
	result.ErrorMessage = fmt.Sprintf("unknown model %q", result.RequestedModel)
	return result
}

func (reg *Registry) finish(result ResolveResult, entry *ModelEntry) ResolveResult {
	// A group is addressed by its own path prefix; the default group is what the
	// root path serves. Resolving another group from the root path would emit
	// that group's routeValue into the model header and let Envoy pick whichever
	// route matches it -- and a routeValue is only unique when every group's
	// Ingress carries a distinct predicate. Where it does not, the caller
	// silently receives the default group's model and is billed for it. Refusing
	// keeps a mis-addressed request from being answered by the wrong model.
	if result.PathGroup == "" && entry.GroupKey != reg.DefaultGroupKey {
		result.ErrorCode = errUnknownModel
		result.ErrorMessage = fmt.Sprintf("model %q is served by group %q, which is reached at /%s/; this endpoint serves group %q",
			result.RequestedModel, entry.GroupKey, entry.GroupKey, reg.DefaultGroupKey)
		return result
	}
	result.Entry = entry
	if result.Endpoint != "" && len(entry.Capabilities) > 0 {
		if allowed, present := entry.Capabilities[result.Endpoint]; present && !allowed {
			result.ErrorCode = errUnsupportedEndpoint
			result.ErrorMessage = fmt.Sprintf("model %q does not support %s", entry.CanonicalID, result.Endpoint)
			return result
		}
		if _, present := entry.Capabilities[result.Endpoint]; !present {
			result.ErrorCode = errUnsupportedEndpoint
			result.ErrorMessage = fmt.Sprintf("model %q does not support %s", entry.CanonicalID, result.Endpoint)
			return result
		}
	}
	result.OK = true
	return result
}

func (reg *Registry) lookup(group, name string) *ModelEntry {
	if entry := reg.byGroupModel[groupModelKey(group, name)]; entry != nil {
		return entry
	}
	return nil
}

func groupModelKey(group, name string) string {
	return group + groupModelSep + name
}

func parsePathGroup(rawPath string, groups map[string]struct{}) (string, string, string) {
	path := rawPath
	if cut := strings.IndexAny(path, "?#"); cut >= 0 {
		path = path[:cut]
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", ""
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", "", ""
	}
	if strings.EqualFold(parts[0], "v1") {
		return "", "", ""
	}
	if len(parts) >= 2 && strings.EqualFold(parts[1], "v1") {
		group := parts[0]
		if _, known := groups[group]; !known {
			return "", errUnknownGroup, fmt.Sprintf("unknown group %q", group)
		}
		return group, "", ""
	}
	return "", "", ""
}

func endpointFromPath(rawPath string) string {
	path := rawPath
	if cut := strings.IndexAny(path, "?#"); cut >= 0 {
		path = path[:cut]
	}
	path = strings.ToLower(strings.TrimSuffix(path, "/"))
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return capOpenAIChatCompletions
	case strings.HasSuffix(path, "/count_tokens"):
		return capOpenAICountTokens
	case strings.HasSuffix(path, "/embeddings"):
		return capOpenAIEmbeddings
	case strings.Contains(path, "/audio/"):
		return capOpenAIAudio
	case strings.HasSuffix(path, "/responses"):
		return capOpenAIResponses
	case strings.HasSuffix(path, "/messages"):
		return capAnthropicMessages
	case strings.HasSuffix(path, "/completions"):
		return capOpenAICompletions
	case strings.Contains(path, "/models"):
		return capOpenAIModels
	default:
		return ""
	}
}

func stripGroupPrefix(id, group string) string {
	prefix := group + "/"
	if strings.HasPrefix(id, prefix) {
		return strings.TrimPrefix(id, prefix)
	}
	return ""
}

func splitKnownPrefix(id string, groups map[string]struct{}) (string, string, bool) {
	i := strings.Index(id, "/")
	if i <= 0 {
		return "", "", false
	}
	group := id[:i]
	rest := id[i+1:]
	if groups != nil {
		if _, ok := groups[group]; !ok {
			return "", "", false
		}
	}
	return group, rest, rest != ""
}

func containsFold(list []string, want string) bool {
	for _, item := range list {
		if strings.EqualFold(item, want) {
			return true
		}
	}
	return false
}

func protocolErrorType(path string) string {
	if endpointFromPath(path) == capAnthropicMessages {
		return "anthropic"
	}
	return "openai"
}
