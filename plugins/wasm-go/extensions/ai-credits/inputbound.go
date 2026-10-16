package main

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Input metering boundary.
//
// A strict cap needs an upper bound on the WHOLE billable request, not just on
// the output. The gateway is the only place that holds the request body, so it
// is the only place that can produce one, and the backend refuses an admission
// that carries no bound (`unbounded_estimate`).
//
// The bound this file computes is provable, not estimated:
//
//   - Every token a tokenizer emits consumes at least one byte of the request
//     body it came from, so the body's byte length is an upper bound on the
//     input tokens of any text request. It is a loose bound (roughly 3x for
//     Chinese, 4x for English), and that looseness is deliberate: an
//     over-reservation is released at settlement, an under-reservation is a
//     budget that was never really enforced.
//
//   - A part whose token cost does NOT scale with the bytes in the body -- a
//     remote image URL, an uploaded file id, a document reference -- breaks
//     the argument: a 60-byte URL can expand into thousands of tokens
//     upstream. Those are counted separately and charged an operator-declared
//     per-reference allowance. With no allowance declared, the request has no
//     provable bound and is refused rather than admitted against an invented
//     default.
//
// Inline payloads (`data:` URIs, base64 `source.data`, `input_audio.data`)
// stay inside the byte bound: their encoded bytes are present in the body and
// exceed the token cost of the media they carry.

// unboundedInputReason names the capability that could not be bounded. It is
// reported to the caller so the refusal is diagnosable rather than opaque.
type inputBoundResult struct {
	// TokensUpperBound is the provable ceiling on input tokens.
	TokensUpperBound int64
	// Provable is false when the request carries a reference whose token cost
	// the gateway cannot bound and the operator declared no allowance for.
	Provable bool
	// Reason names the unbounded capability (e.g. "remote image reference").
	Reason string
	// ExternalReferences is how many unbounded references were found.
	ExternalReferences int
}

// externalPartTypes are content part types whose token cost may be unrelated
// to the bytes they occupy in the request body.
var externalPartTypes = map[string]string{
	"image_url":    "remote image reference",
	"input_image":  "remote image reference",
	"image":        "remote image reference",
	"file":         "file reference",
	"input_file":   "file reference",
	"document":     "document reference",
	"video_url":    "remote video reference",
	"input_video":  "remote video reference",
	"audio_url":    "remote audio reference",
	"input_audio":  "audio reference",
	"audio":        "audio reference",
	"container":    "container reference",
	"file_search":  "file search reference",
	"tool_outputs": "tool output reference",
}

// computeInputBound derives the provable input-token ceiling for one request
// body. allowancePerReference is the operator-declared token allowance for a
// single unbounded reference; zero means such requests are refused.
func computeInputBound(body []byte, allowancePerReference int64) inputBoundResult {
	result := inputBoundResult{TokensUpperBound: int64(len(body)), Provable: true}
	parsed := gjson.ParseBytes(body)
	scanExternalReferences(parsed, &result)
	if result.ExternalReferences > 0 {
		if allowancePerReference <= 0 {
			result.Provable = false
			return result
		}
		result.TokensUpperBound += allowancePerReference * int64(result.ExternalReferences)
	}
	return result
}

// scanExternalReferences walks the decoded body and counts content parts whose
// token cost the byte bound does not cover.
func scanExternalReferences(value gjson.Result, result *inputBoundResult) {
	switch {
	case value.IsArray():
		value.ForEach(func(_, item gjson.Result) bool {
			scanExternalReferences(item, result)
			return true
		})
	case value.IsObject():
		if kind := strings.TrimSpace(value.Get("type").String()); kind != "" {
			if reason, external := externalPartTypes[kind]; external && !inlinePayload(value, kind) {
				result.ExternalReferences++
				if result.Reason == "" {
					result.Reason = reason
				}
			}
		}
		value.ForEach(func(_, item gjson.Result) bool {
			scanExternalReferences(item, result)
			return true
		})
	}
}

// inlinePayload reports whether a non-text part carries its payload inline, so
// the body's byte length already bounds it.
func inlinePayload(value gjson.Result, kind string) bool {
	// Anthropic shape: {"type":"image","source":{"type":"base64","data":"..."}}
	if source := value.Get("source"); source.Exists() {
		switch strings.TrimSpace(source.Get("type").String()) {
		case "base64":
			return true
		case "url", "file":
			return false
		}
		if source.Get("data").Exists() {
			return true
		}
		return false
	}
	// OpenAI audio shape: {"type":"input_audio","input_audio":{"data":"..."}}
	if data := value.Get(kind + ".data"); data.Exists() {
		return true
	}
	// OpenAI image shapes: image_url as an object or as a bare string.
	for _, path := range []string{kind + ".url", kind, "url", "image_url", "file_url"} {
		candidate := value.Get(path)
		if !candidate.Exists() || candidate.Type != gjson.String {
			continue
		}
		return strings.HasPrefix(strings.TrimSpace(candidate.String()), "data:")
	}
	// A part that names no payload at all (a file id, a container id) is a
	// reference by definition.
	return false
}
