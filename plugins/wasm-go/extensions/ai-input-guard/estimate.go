package main

import (
	"encoding/json"
	"unicode/utf8"
)

// Characters per token, calibrated on 2026-10-29 against the real GLM-5.3-Flash,
// DeepSeek-V4-Flash and Qwen3.8 tokenizers. Natural text (Chinese and English
// docs, Go code, pretty JSON, logs, base64, hex) never exceeded 0.54x of the
// true token count with these divisors, i.e. roughly a 2x safety margin. The
// estimate must stay a lower bound: a request is rejected only when even the
// lower bound exceeds the context window, which the backend would reject too.
const (
	wideCharsPerToken  = 3 // CJK ideographs, kana, hangul, full-width forms
	otherCharsPerToken = 8 // everything else
	minCharsPerToken   = wideCharsPerToken
)

// Strings at least this long whose prefix is entirely base64 alphabet are
// treated as binary payloads (images, audio, files). Those reach the model as
// a handful of media tokens, not as text.
const (
	binaryMinLen    = 1024
	binaryPrefixLen = 256
)

// estimateInputTokens returns a lower bound on the prompt tokens in a JSON
// request body by counting characters in every string literal, stopping early
// once the bound exceeds limit.
//
// Runs of one repeated character are counted once: tokenizers merge them
// heavily (a run of 200k spaces is under 400 GLM tokens).
func estimateInputTokens(body []byte, limit int) (int, bool) {
	var c charCounter
	budget := limit * wideCharsPerToken * otherCharsPerToken
	for i := 0; i < len(body); i++ {
		if body[i] != '"' {
			continue
		}
		end := stringEnd(body, i+1)
		s := body[i+1 : end]
		i = end
		if looksBinary(s) {
			continue
		}
		if c.add(s, budget) {
			return c.tokens(), true
		}
	}
	return c.tokens(), false
}

type charCounter struct {
	wide, other int
}

// weight is tokens scaled by wideCharsPerToken*otherCharsPerToken, so the
// comparison against the budget needs no division.
func (c *charCounter) weight() int {
	return c.wide*otherCharsPerToken + c.other*wideCharsPerToken
}

// tokens rounds up so that a count over budget always reports more than limit.
func (c *charCounter) tokens() int {
	unit := wideCharsPerToken * otherCharsPerToken
	return (c.weight() + unit - 1) / unit
}

// add counts the characters of one raw (still escaped) JSON string literal and
// reports whether the running weight passed budget.
func (c *charCounter) add(s []byte, budget int) bool {
	prev := rune(-1)
	for k := 0; k < len(s); {
		r, size := decodeJSONRune(s[k:])
		k += size
		if r == prev {
			continue
		}
		prev = r
		if isWide(r) {
			c.wide++
		} else {
			c.other++
		}
		if c.weight() > budget {
			return true
		}
	}
	return false
}

// stringEnd returns the index of the closing quote of the string literal
// starting at start, or len(body) if it is unterminated.
func stringEnd(body []byte, start int) int {
	for j := start; j < len(body); j++ {
		switch body[j] {
		case '\\':
			j++
		case '"':
			return j
		}
	}
	return len(body)
}

func decodeJSONRune(s []byte) (rune, int) {
	if s[0] != '\\' {
		if s[0] < utf8.RuneSelf {
			return rune(s[0]), 1
		}
		return utf8.DecodeRune(s)
	}
	if len(s) < 2 {
		return '\\', 1
	}
	if s[1] != 'u' {
		// \n, \t, \", \\, \/ ... each is one character.
		return rune(s[1]), 2
	}
	r, ok := hex4(s[2:])
	if !ok {
		return '\\', 2
	}
	if r >= 0xD800 && r < 0xDC00 && len(s) >= 12 && s[6] == '\\' && s[7] == 'u' {
		if lo, ok := hex4(s[8:]); ok && lo >= 0xDC00 && lo < 0xE000 {
			return (r-0xD800)<<10 + (lo - 0xDC00) + 0x10000, 12
		}
	}
	return r, 6
}

func hex4(s []byte) (rune, bool) {
	if len(s) < 4 {
		return 0, false
	}
	var r rune
	for _, b := range s[:4] {
		switch {
		case b >= '0' && b <= '9':
			r = r<<4 | rune(b-'0')
		case b >= 'a' && b <= 'f':
			r = r<<4 | rune(b-'a'+10)
		case b >= 'A' && b <= 'F':
			r = r<<4 | rune(b-'A'+10)
		default:
			return 0, false
		}
	}
	return r, true
}

func isWide(r rune) bool {
	return (r >= 0x1100 && r <= 0x11FF) ||
		(r >= 0x2E80 && r <= 0x9FFF) ||
		(r >= 0xA960 && r <= 0xA97F) ||
		(r >= 0xAC00 && r <= 0xD7FF) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0xFE30 && r <= 0xFE4F) ||
		(r >= 0xFF00 && r <= 0xFFEF) ||
		(r >= 0x20000 && r <= 0x3FFFF)
}

// looksBinary reports whether a raw string literal is a data URI or a long
// base64 payload. "\/" is allowed because some encoders escape slashes.
func looksBinary(s []byte) bool {
	if len(s) >= 5 && string(s[:5]) == "data:" {
		return true
	}
	if len(s) < binaryMinLen {
		return false
	}
	for k := 0; k < binaryPrefixLen; k++ {
		b := s[k]
		switch {
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9',
			b == '+', b == '/', b == '=', b == '-', b == '_':
		case b == '\\' && s[k+1] == '/':
			k++
		default:
			// Includes \uXXXX: text serialized with ensure_ascii (Python's
			// json.dumps default) is all hex digits and backslashes.
			return false
		}
	}
	return true
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
