package main

import "testing"

// Cache tokens are usually the largest component of a cached request and the
// one with the deepest discount, so whether the provider already counted them
// inside the input count decides the bill more than anything else about the
// response.
//
// Providers disagree, and both shapes reach the same model here, so the answer
// cannot come from the model, the route, or the key name. It comes from the
// arithmetic ai-statistics already settled on: compare the reported total
// against input + output.
//
// Every case below is a real record. Production ones are from SLS
// cloudeyeforai/ai-accesslog and test ones from ai-accesslog-test, 2026-09-20
// to 2026-09-22.
func TestSplitTokensSeparatesCacheFromInput(t *testing.T) {
	tests := []struct {
		name                 string
		details              map[string]int64
		input, output, total int64
		want                 tokenBreakdown
	}{{
		// /bailian/v1/chat/completions/v1/messages, production.
		// 6 + 226 = 232, and 47741 - 232 = 47509 -- exactly the reported
		// cache read. The cache is OUTSIDE input, so input stays as reported.
		name:    "anthropic counts cache alongside input",
		details: map[string]int64{"cache_read_input_tokens": 47_509, "cache_creation_input_tokens": 0},
		input:   6, output: 226, total: 47_741,
		want: tokenBreakdown{Input: 6, Output: 226, CacheRead: 47_509},
	}, {
		// /bailian/v1/v1/messages, production. Same shape, cache creation.
		name:    "anthropic counts cache creation alongside input",
		details: map[string]int64{"cache_read_input_tokens": 0, "cache_creation_input_tokens": 55_480},
		input:   2, output: 904, total: 56_386,
		want: tokenBreakdown{Input: 2, Output: 904, CacheWrite: 55_480},
	}, {
		// glm-5.3-flash, test cluster. 90569 + 76 == 90645, so the 89920
		// cached tokens are counted in NEITHER sum -- they are part of the
		// 90569. Billing all of it as fresh input charges 99.3% of this
		// prompt at the full rate.
		name:    "openai counts cache inside input",
		details: map[string]int64{"cached_tokens": 89_920},
		input:   90_569, output: 76, total: 90_645,
		want: tokenBreakdown{Input: 649, Output: 76, CacheRead: 89_920},
	}, {
		// /v1/chat/completions, production. Cache reported as zero, and
		// image_tokens is another subdivision of the prompt -- already inside
		// input, not separately priced, so it must not be moved out.
		name:    "other prompt subdivisions stay inside input",
		details: map[string]int64{"cached_tokens": 0, "image_tokens": 5_040},
		input:   5_938, output: 8_026, total: 13_964,
		want: tokenBreakdown{Input: 5_938, Output: 8_026},
	}, {
		name:    "no cache detail at all leaves input untouched",
		details: map[string]int64{},
		input:   75, output: 236, total: 311,
		want: tokenBreakdown{Input: 75, Output: 236},
	}, {
		// Gemini counts cached content inside promptTokenCount.
		name:    "gemini counts cache inside input",
		details: map[string]int64{"cached_content_token_count": 500},
		input:   800, output: 40, total: 840,
		want: tokenBreakdown{Input: 300, Output: 40, CacheRead: 500},
	}, {
		// More cache than the prompt it is part of is a provider bug. The
		// excess is dropped rather than billed: charging 5,000 cache tokens
		// against a 1,200-token prompt would overcharge in the name of
		// fixing an overcharge. The components still sum to the reported
		// total.
		name:    "a cache count larger than the prompt is clamped",
		details: map[string]int64{"cached_tokens": 5_000},
		input:   1_200, output: 30, total: 1_230,
		want: tokenBreakdown{Input: 0, Output: 30, CacheRead: 1_200},
	}, {
		// A total inflated beyond what the cache detail explains must not
		// manufacture cache tokens: the excess is clamped to what was
		// actually reported, as ai-statistics clamps cacheDetailInTotal.
		name:    "an unexplained excess in total is clamped to the reported cache",
		details: map[string]int64{"cache_read_input_tokens": 100},
		input:   50, output: 20, total: 9_999,
		want: tokenBreakdown{Input: 50, Output: 20, CacheRead: 100},
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := splitTokens(test.details, test.input, test.output, test.total)
			if got != test.want {
				t.Errorf("splitTokens() = %+v, want %+v", got, test.want)
			}
		})
	}
}

// Whatever shape the provider used, the four components must add up to the
// total it reported. This is the property that catches a cache count billed
// twice (sum too high) or dropped out of the bill (sum too low) -- and it held
// on all 100 production records carrying cache.
func TestSplitTokensConservesTheReportedTotal(t *testing.T) {
	tests := []struct {
		name                 string
		details              map[string]int64
		input, output, total int64
	}{{
		name:    "alongside",
		details: map[string]int64{"cache_read_input_tokens": 47_509},
		input:   6, output: 226, total: 47_741,
	}, {
		name:    "alongside, cache creation",
		details: map[string]int64{"cache_creation_input_tokens": 55_480},
		input:   2, output: 904, total: 56_386,
	}, {
		name:    "inside",
		details: map[string]int64{"cached_tokens": 89_920},
		input:   90_569, output: 76, total: 90_645,
	}, {
		name:    "no cache",
		details: map[string]int64{},
		input:   5_938, output: 8_026, total: 13_964,
	}, {
		// Even when the provider contradicts itself, the bill must not
		// exceed the tokens it reported.
		name:    "cache larger than the prompt",
		details: map[string]int64{"cached_tokens": 5_000},
		input:   1_200, output: 30, total: 1_230,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := splitTokens(test.details, test.input, test.output, test.total).total()
			if got != test.total {
				t.Errorf("components sum to %d, but the provider reported %d", got, test.total)
			}
		})
	}
}

// What the bug cost, at this cluster's configured price for glm-5.3-flash:
// 80 credits per million input tokens, 23 per million cache reads, 280 per
// million output.
func TestCachedRequestIsBilledAtTheCacheRate(t *testing.T) {
	price := Price{
		Unit:            UnitTokens,
		Per:             1_000_000,
		InputMicros:     80_000_000,
		CacheReadMicros: 23_000_000,
		OutputMicros:    280_000_000,
	}
	charge := func(usage tokenBreakdown) int64 {
		millis, chargeable, err := chargeFor(price, metered{tokens: usage, tokensKnown: true, succeeded: true})
		if err != nil || !chargeable {
			t.Fatalf("chargeFor(%+v) = %d, %v, %v", usage, millis, chargeable, err)
		}
		return millis
	}

	// 649 fresh input + 89,920 cache reads + 76 output.
	correct := charge(splitTokens(map[string]int64{"cached_tokens": 89_920}, 90_569, 76, 90_645))
	if correct != 2141 {
		t.Errorf("charge = %d milli-credits, want 2141", correct)
	}

	// What this request was actually billed before the split: the whole
	// prompt at the fresh input rate, 3.4x as much.
	unsplit := charge(tokenBreakdown{Input: 90_569, Output: 76})
	if unsplit != 7267 {
		t.Errorf("unsplit charge = %d, want 7267", unsplit)
	}
	if unsplit <= correct {
		t.Fatalf("unsplit %d should exceed correct %d", unsplit, correct)
	}

	// An Anthropic-shaped request was already billed correctly, and must stay
	// that way: 6 input + 47,509 cache reads + 226 output.
	alongside := charge(splitTokens(map[string]int64{"cache_read_input_tokens": 47_509}, 6, 226, 47_741))
	if want := int64(6*80+47_509*23+226*280) / 1000; alongside != want {
		t.Errorf("alongside charge = %d milli-credits, want %d", alongside, want)
	}
}
