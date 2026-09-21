package main

import (
	"encoding/json"
	"strings"
)

// Frozen plugin-facing private contract. v2 must implement these paths and
// JSON fields; the default WasmPlugin template wires them as-is.
const (
	CreditsContractVersion = "ai-credits.v1"
	DefaultAdmitPath       = "/internal/ai-credits/v1/admit"
	DefaultSettlePath      = "/internal/ai-credits/v1/settle"
	DefaultServiceName     = "higress-ai-key-admin-v2.higress-system.svc.cluster.local"
	DefaultServicePort     = 80
	DefaultAuthHeader      = "X-AI-Credits-Service-Key"
	DefaultConsumerHeader  = "x-mse-consumer"
	ModeOff                = "off"
	ModeShadow             = "shadow"
	ModeEnforce            = "enforce"
	DecisionAllow          = "allow"
	DecisionDeny           = "deny"
	SettleSettled          = "settled"
	SettlePendingVerify    = "pending_verify"
	SettleReleased         = "released"
	ReasonEOS              = "eos"
	ReasonSemanticEnd      = "semantic_end"
	ReasonDisconnect       = "disconnect"
	ReasonMissingUsage     = "missing_usage"
	ReasonUpstreamError    = "upstream_error"
	ReasonDenied           = "denied"
)

type UsagePayload struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	CachedReadTokens  int64 `json:"cachedReadTokens"`
	CachedWriteTokens int64 `json:"cachedWriteTokens"`
	TotalTokens       int64 `json:"totalTokens"`
	Complete          bool  `json:"complete"`
}

type AdmitRequest struct {
	ContractVersion      string `json:"contractVersion"`
	RequestID            string `json:"requestId"`
	AttemptID            string `json:"attemptId"`
	Consumer             string `json:"consumer"`
	EntryPath            string `json:"entryPath"`
	EntryProtocol        string `json:"entryProtocol"`
	Endpoint             string `json:"endpoint"`
	RequestedModel       string `json:"requestedModel"`
	CanonicalModel       string `json:"canonicalModel"`
	UpstreamModel        string `json:"upstreamModel"`
	GroupKey             string `json:"groupKey"`
	Stream               bool   `json:"stream"`
	EstimatedInputTokens int64  `json:"estimatedInputTokens"`
	MaxOutputTokens      *int64 `json:"maxOutputTokens,omitempty"`
	ConfigVersion        string `json:"configVersion"`
}

type AdmitResponse struct {
	Decision                 string    `json:"decision"`
	Mode                     string    `json:"mode"`
	ReservationID            string    `json:"reservationId"`
	RequestID                string    `json:"requestId"`
	SettlementID             string    `json:"settlementId"`
	CanonicalModel           string    `json:"canonicalModel"`
	PriceVersion             string    `json:"priceVersion"`
	BudgetSubject            string    `json:"budgetSubject"`
	Period                   string    `json:"period"`
	BatchID                  string    `json:"batchId"`
	ReservedCreditsMicros    int64     `json:"reservedCreditsMicros"`
	MaxBillableCreditsMicros int64     `json:"maxBillableCreditsMicros"`
	MaxOutputTokens          *int64    `json:"maxOutputTokens"`
	StrictCap                bool      `json:"strictCap"`
	Error                    *APIError `json:"error,omitempty"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type SettleRequest struct {
	ContractVersion string       `json:"contractVersion"`
	RequestID       string       `json:"requestId"`
	AttemptID       string       `json:"attemptId"`
	ReservationID   string       `json:"reservationId"`
	Consumer        string       `json:"consumer"`
	CanonicalModel  string       `json:"canonicalModel"`
	Status          string       `json:"status"`
	HTTPStatus      int          `json:"httpStatus"`
	Stream          bool         `json:"stream"`
	Usage           UsagePayload `json:"usage"`
	Reason          string       `json:"reason"`
}

func marshalJSON(value interface{}) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"error":{"code":"encode_failed","message":"failed to encode request"}}`)
	}
	return data
}

func parseAdmitResponse(body []byte) (AdmitResponse, error) {
	var parsed AdmitResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return AdmitResponse{}, err
	}
	parsed.Decision = strings.ToLower(strings.TrimSpace(parsed.Decision))
	return parsed, nil
}
