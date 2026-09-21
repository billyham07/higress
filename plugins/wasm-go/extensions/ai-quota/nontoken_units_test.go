package main

import "testing"

// TestCharactersAreCountedAsRunesNotBytes: this gateway's synthesis traffic is
// largely Chinese, and UTF-8 spends three bytes on each of those characters.
// Counting len() would charge a Chinese sentence three times what the
// identical English one costs for the same work.
func TestCharactersAreCountedAsRunesNotBytes(t *testing.T) {
	body := []byte(`{"model":"Qwen3-TTS","input":"你好世界","voice":"default"}`)

	characters, known := countRequestCharacters(body)
	if !known {
		t.Fatal("expected the input field to be found")
	}
	if characters != 4 {
		t.Fatalf("expected 4 characters, got %d (bytes would be 12)", characters)
	}
}

// TestCharactersWithNoTextFieldAreNotBillable: a provider using a field name
// this plugin does not know must not be billed as zero. Zero is a price an
// operator set; a missing field is a mismatch, and reporting it as free would
// hide it for as long as the route runs.
func TestCharactersWithNoTextFieldAreNotBillable(t *testing.T) {
	characters, known := countRequestCharacters([]byte(`{"model":"x","prompt":"hello"}`))
	if known || characters != 0 {
		t.Fatalf("expected an unknown count, got %d known=%v", characters, known)
	}

	price := Price{Unit: UnitCharacters, Per: 1000, CharacterMicros: 1_000_000}
	amount, chargeable, err := chargeFor(price, metered{succeeded: true})
	if err != nil {
		t.Fatal(err)
	}
	if chargeable || amount != 0 {
		t.Fatalf("an unmetered request must not move the quota, got %d chargeable=%v", amount, chargeable)
	}
}

// TestCharactersAreNotChargedWhenSynthesisFailed: a per-character price bills
// the submitted text, which exists whether or not audio came back. Without the
// status gate an upstream 500 would charge for speech the caller never got.
func TestCharactersAreNotChargedWhenSynthesisFailed(t *testing.T) {
	price := Price{Unit: UnitCharacters, Per: 1000, CharacterMicros: 1_000_000}
	usage := metered{characters: 4000, charactersKnown: true}

	usage.succeeded = false
	if amount, chargeable, err := chargeFor(price, usage); err != nil || chargeable || amount != 0 {
		t.Fatalf("a failed synthesis must cost nothing, got %d chargeable=%v err=%v", amount, chargeable, err)
	}

	usage.succeeded = true
	amount, chargeable, err := chargeFor(price, usage)
	if err != nil || !chargeable || amount != 4_000 {
		t.Fatalf("expected 4 credits (4000 millis) for 4000 characters, got %d chargeable=%v err=%v", amount, chargeable, err)
	}
}

// TestSecondsComeFromADurationUsage: Qwen3-ASR reports neither input nor
// output tokens, so the token path leaves it unbillable. The duration it does
// report is the only honest quantity for that route.
func TestSecondsComeFromADurationUsage(t *testing.T) {
	seconds, known := responseSeconds([]byte(`{"text":"","usage":{"type":"duration","seconds":1}}`))
	if !known || seconds != 1 {
		t.Fatalf("expected 1 second, got %d known=%v", seconds, known)
	}

	price := Price{Unit: UnitSeconds, Per: 60, SecondMicros: 6_000_000}
	amount, chargeable, err := chargeFor(price, metered{seconds: 60, secondsKnown: true, succeeded: true})
	if err != nil || !chargeable || amount != 6_000 {
		t.Fatalf("expected 6 credits (6000 millis) a minute, got %d chargeable=%v err=%v", amount, chargeable, err)
	}
}

// TestAPartialSecondRoundsUp: a half-second clip consumed a second's worth of
// capacity. Truncating would make every sub-second request free, which is a
// rate nobody configured.
func TestAPartialSecondRoundsUp(t *testing.T) {
	seconds, known := responseSeconds([]byte(`{"usage":{"type":"duration","seconds":0.4}}`))
	if !known || seconds != 1 {
		t.Fatalf("expected 0.4s to round up to 1, got %d known=%v", seconds, known)
	}
	if seconds, _ := responseSeconds([]byte(`{"usage":{"type":"duration","seconds":2}}`)); seconds != 2 {
		t.Fatalf("expected a whole second to stay whole, got %d", seconds)
	}
}

// TestADifferentKindOfUsageIsNotBilledAsSeconds: `usage` is a shared key. A
// provider reporting something else under it must not have that number
// multiplied by a per-second rate.
func TestADifferentKindOfUsageIsNotBilledAsSeconds(t *testing.T) {
	if _, known := responseSeconds([]byte(`{"usage":{"type":"pages","seconds":9}}`)); known {
		t.Fatal("a non-duration usage must not be read as seconds")
	}
	if _, known := responseSeconds([]byte(`{"usage":{"prompt_tokens":15,"total_tokens":15}}`)); known {
		t.Fatal("a token usage must not be read as seconds")
	}
}

// TestTokenRoutesAreUnaffectedByTheNewUnits: embedding and rerank report a
// plain token usage, and the units added for audio must not change how that is
// billed.
func TestTokenRoutesAreUnaffectedByTheNewUnits(t *testing.T) {
	price := Price{Unit: UnitTokens, Per: 1000, InputMicros: 1_000_000}
	usage := metered{
		tokens:      tokenBreakdown{Input: 15},
		tokensKnown: true,
		succeeded:   true,
		// An audio quantity that happens to be in context must be ignored.
		seconds: 9999, secondsKnown: true,
		characters: 9999, charactersKnown: true,
	}
	amount, chargeable, err := chargeFor(price, usage)
	// 15 tokens at 1 credit per 1000 is 0.015 credits. A whole-credit ledger
	// rounded that to nothing; the milli-credit ledger bills it as 15.
	if err != nil || !chargeable || amount != 15 {
		t.Fatalf("expected 15 tokens at 1 credit/1000 to cost 15 millis, got %d chargeable=%v err=%v", amount, chargeable, err)
	}
}
