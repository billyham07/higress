package main

import (
	"errors"

	"github.com/tidwall/gjson"
)

// The credit multiplier.
//
// A price says what a model costs; the multiplier says how much of that a
// route passes on. They are separate on purpose. Self-hosted capacity -- the
// H20 group -- has a real cost worth writing down, and it is offered free:
// pricing it at zero would lose the first fact, and would not be enough
// anyway, because admission refuses an empty wallet whatever the price.
//
// `multiplier_percent` is a whole number on the route's rule, 100 when
// absent. An integer percent keeps floats out of the configuration for the
// same reason rates are integers: 0.5 read by a strict parser as 0 would make
// a route free and look like it worked.
//
// 0 is the one value admission reads. A request on a free route spends
// nothing, so an empty or overdrawn wallet and a key past its own cap are no
// reason to refuse it: someone who has spent their credits on paid vendors
// keeps the self-hosted models. Every other value only scales the charge, and
// the wallet is gated exactly as before.
const (
	defaultMultiplierPercent = 100
	// maxMultiplierPercent is 100x. Anything above it is a keyboard slip, and
	// is refused at plugin start rather than discovered in a bill.
	maxMultiplierPercent = 10_000
)

func parseMultiplier(json gjson.Result, config *QuotaConfig) error {
	config.MultiplierPercent = defaultMultiplierPercent
	raw := json.Get("multiplier_percent")
	if !raw.Exists() {
		return nil
	}
	if raw.Type != gjson.Number || raw.Num != float64(raw.Int()) {
		return errors.New("multiplier_percent must be a whole number of percent")
	}
	if raw.Int() < 0 || raw.Int() > maxMultiplierPercent {
		return errors.New("multiplier_percent must be between 0 and 10000")
	}
	config.MultiplierPercent = raw.Int()
	return nil
}

// free reports a route whose requests are charged nothing at all.
func (config QuotaConfig) free() bool {
	return config.MultiplierPercent == 0
}

// scale applies the multiplier to a charge in milli-credits, rounding half up
// like the price itself does. It runs after the price's min_charge_millis
// floor, so a free route stays at zero however the price is floored.
func (config QuotaConfig) scale(millis int64) int64 {
	if config.MultiplierPercent == defaultMultiplierPercent {
		return millis
	}
	return (millis*config.MultiplierPercent + defaultMultiplierPercent/2) / defaultMultiplierPercent
}
