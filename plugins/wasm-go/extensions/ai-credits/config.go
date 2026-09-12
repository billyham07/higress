package main

import (
	"errors"
	"strings"

	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

type CreditsConfig struct {
	Mode           string
	ConsumerHeader string
	AuthHeader     string
	AuthKey        string
	AdmitPath      string
	SettlePath     string
	Timeout        uint32
	EnableSuffixes []string
	Client         wrapper.HttpClient
	Registry       *Registry
	SkipWhenOff    bool
	// UnboundedReferenceTokens is the operator-declared token allowance for
	// one content part whose cost the request bytes cannot bound (a remote
	// image URL, a file id). Zero refuses such requests instead of admitting
	// them against an invented estimate.
	UnboundedReferenceTokens int64
	// SettleRetries is how many delivery attempts one settlement gets before
	// its payload is written to the durable log. One means no retry.
	SettleRetries int
	// Outbox is the durable settlement channel. Nil means the deployment
	// configured none, and undelivered settlements fall back to the log line.
	Outbox *SettlementOutbox
}

func parseConfig(json gjson.Result, config *CreditsConfig) error {
	mode := strings.ToLower(strings.TrimSpace(json.Get("mode").String()))
	if mode == "" {
		mode = ModeOff
	}
	switch mode {
	case ModeOff, ModeShadow, ModeEnforce:
		config.Mode = mode
	default:
		return errors.New("mode must be off, shadow, or enforce")
	}
	config.SkipWhenOff = true
	config.ConsumerHeader = strings.TrimSpace(json.Get("consumerHeader").String())
	if config.ConsumerHeader == "" {
		config.ConsumerHeader = DefaultConsumerHeader
	}
	config.AuthHeader = strings.TrimSpace(json.Get("authHeader").String())
	if config.AuthHeader == "" {
		config.AuthHeader = DefaultAuthHeader
	}
	config.AuthKey = strings.TrimSpace(json.Get("serviceKey").String())
	if config.Mode != ModeOff && config.AuthKey == "" {
		return errors.New("serviceKey is required when mode is shadow or enforce")
	}
	config.AdmitPath = strings.TrimSpace(json.Get("admitPath").String())
	if config.AdmitPath == "" {
		config.AdmitPath = DefaultAdmitPath
	}
	config.SettlePath = strings.TrimSpace(json.Get("settlePath").String())
	if config.SettlePath == "" {
		config.SettlePath = DefaultSettlePath
	}
	config.Timeout = uint32(json.Get("timeout").Uint())
	if config.Timeout == 0 {
		config.Timeout = 1000
	}
	suffixes := json.Get("enablePathSuffixes")
	if !suffixes.Exists() {
		config.EnableSuffixes = []string{
			"/v1/chat/completions",
			"/v1/messages",
			"/v1/responses",
			"/chat/completions",
			"/messages",
			"/responses",
		}
	} else if !suffixes.IsArray() {
		return errors.New("enablePathSuffixes must be an array")
	} else {
		for _, item := range suffixes.Array() {
			value := strings.TrimSpace(item.String())
			if value != "" {
				config.EnableSuffixes = append(config.EnableSuffixes, value)
			}
		}
	}

	serviceName := strings.TrimSpace(json.Get("service.name").String())
	if serviceName == "" {
		serviceName = strings.TrimSpace(json.Get("service_name").String())
	}
	if serviceName == "" {
		serviceName = DefaultServiceName
	}
	servicePort := int(json.Get("service.port").Int())
	if servicePort == 0 {
		servicePort = int(json.Get("service_port").Int())
	}
	if servicePort == 0 {
		servicePort = DefaultServicePort
	}
	config.Client = wrapper.NewClusterClient(wrapper.FQDNCluster{
		FQDN: serviceName,
		Port: int64(servicePort),
	})

	config.SettleRetries = int(json.Get("settleRetries").Int())
	if config.SettleRetries <= 0 {
		config.SettleRetries = 3
	}

	config.UnboundedReferenceTokens = json.Get("unboundedReferenceTokens").Int()
	if config.UnboundedReferenceTokens < 0 {
		return errors.New("unboundedReferenceTokens must not be negative")
	}

	outbox, err := parseSettlementOutbox(json)
	if err != nil {
		return err
	}
	config.Outbox = outbox
	if outbox != nil {
		// Redis callouts issued from an HTTP context can lose their callbacks
		// when Envoy tears down a completed stream. Install one root-context
		// verifier so the exact HSET -> WAITAOF write barrier can finish
		// independently.
		ensureOutboxVerifier()
	}

	registry, err := parseRegistry(json)
	if err != nil {
		return err
	}
	config.Registry = registry
	return nil
}

func pathEnabled(path string, suffixes []string) bool {
	if cut := strings.IndexAny(path, "?#"); cut >= 0 {
		path = path[:cut]
	}
	for _, suffix := range suffixes {
		if suffix == "*" || strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}
