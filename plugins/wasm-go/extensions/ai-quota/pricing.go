package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// Credit pricing.
//
// The quota counter in Redis has always held one number that a request
// decrements. What changes here is only how that number is produced: instead
// of "one token, one unit", a request's cost is its metered usage multiplied
// by the price configured for its model.
//
// Rates are integers in MICRO-CREDITS (1e6 micros == 1 credit) so no float
// ever appears in the configuration or in the arithmetic. `per` says how many
// metered units one rate covers, which is what makes a readable price like
// "0.02 credits per 1000 tokens" expressible as `per: 1000, input_micros:
// 20000`.
//
// All money arithmetic is int64. On wasm32 a Go `int` is 32 bits, so the
// charge is bounds-checked before it reaches the Redis client's `int`
// parameter.
const microsPerCredit = 1_000_000

// Metering units. `tokens` prices the four token components; `requests`
// prices one call regardless of what it produced, which is the honest unit
// for an endpoint whose response carries no usage at all.
const (
	UnitTokens   = "tokens"
	UnitRequests = "requests"
)

// maxCharge is the largest charge that can be handed to the Redis client's
// int parameter on a 32-bit target. A request costing more than this is a
// configuration error rather than a real bill, and it is refused loudly
// instead of wrapping around into a credit.
const maxCharge = int64(1<<31 - 1)

// Price is one model's rate. The four token components are priced separately
// because they are disjoint counts, not because anyone wants four numbers: a
// cache read costs a fraction of a fresh input token, and collapsing them to
// one multiplier makes that discount unexpressible.
type Price struct {
	Unit             string
	Per              int64
	InputMicros      int64
	OutputMicros     int64
	CacheReadMicros  int64
	CacheWriteMicros int64
	RequestMicros    int64
	// MinCharge is the floor, in whole credits, for a request whose computed
	// cost rounded down to nothing. Zero -- the default -- means a rate of
	// zero really does charge zero, which is what a free self-hosted model
	// wants. Set it to 1 where "too small to bill" must not become "free".
	MinCharge int64
}

// tokenBreakdown is one request's metered token usage. The four fields are
// treated as disjoint and summed, which is the convention getQuotaToken
// already uses: Anthropic reports cache reads and cache creations alongside
// input_tokens rather than inside it.
type tokenBreakdown struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

func (u tokenBreakdown) total() int64 {
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

// parsePrices reads `default_price` and `model_prices`. Both are optional:
// a configuration with neither leaves pricing off and the plugin charges one
// credit per token exactly as it did before, so rolling this build out to a
// route whose config has not been updated changes nothing.
func parsePrices(json gjson.Result, config *QuotaConfig) error {
	if value := json.Get("default_price"); value.Exists() {
		price, err := parsePrice(value, "default_price")
		if err != nil {
			return err
		}
		config.DefaultPrice = &price
	}

	models := json.Get("model_prices")
	if !models.Exists() {
		return nil
	}
	if !models.IsObject() {
		return errors.New("model_prices must be an object keyed by model name")
	}
	config.ModelPrices = make(map[string]Price)
	var parseErr error
	models.ForEach(func(key, value gjson.Result) bool {
		name := strings.TrimSpace(key.String())
		if name == "" {
			parseErr = errors.New("model_prices contains an empty model name")
			return false
		}
		price, err := parsePrice(value, "model_prices."+name)
		if err != nil {
			parseErr = err
			return false
		}
		config.ModelPrices[name] = price
		return true
	})
	return parseErr
}

func parsePrice(value gjson.Result, where string) (Price, error) {
	if !value.IsObject() {
		return Price{}, fmt.Errorf("%s must be an object", where)
	}
	price := Price{Unit: strings.TrimSpace(value.Get("unit").String())}
	if price.Unit == "" {
		price.Unit = UnitTokens
	}
	switch price.Unit {
	case UnitTokens, UnitRequests:
	default:
		return Price{}, fmt.Errorf("%s.unit must be %s or %s", where, UnitTokens, UnitRequests)
	}

	price.Per = value.Get("per").Int()
	if price.Per == 0 {
		price.Per = 1
	}
	if price.Per < 0 {
		return Price{}, fmt.Errorf("%s.per must be positive", where)
	}

	fields := []struct {
		key    string
		target *int64
	}{
		{"input_micros", &price.InputMicros},
		{"output_micros", &price.OutputMicros},
		{"cache_read_micros", &price.CacheReadMicros},
		{"cache_write_micros", &price.CacheWriteMicros},
		{"request_micros", &price.RequestMicros},
		{"min_charge", &price.MinCharge},
	}
	for _, field := range fields {
		raw := value.Get(field.key)
		if !raw.Exists() {
			continue
		}
		// A rate is an integer count of micro-credits. Accepting a float here
		// would silently truncate 0.5 to 0 and price a model at zero, so a
		// non-integer is refused rather than rounded.
		if raw.Type != gjson.Number || raw.Num != float64(raw.Int()) {
			return Price{}, fmt.Errorf("%s.%s must be an integer", where, field.key)
		}
		if raw.Int() < 0 {
			return Price{}, fmt.Errorf("%s.%s must not be negative", where, field.key)
		}
		*field.target = raw.Int()
	}
	return price, nil
}

// priceFor picks the rate for one model: its own entry, otherwise the route's
// default. A nil result means this route has no pricing configured at all.
func priceFor(config QuotaConfig, model string) *Price {
	if len(config.ModelPrices) > 0 {
		if price, ok := config.ModelPrices[strings.TrimSpace(model)]; ok {
			return &price
		}
	}
	return config.DefaultPrice
}

// chargeFor converts one request's usage into whole credits.
//
// `usageKnown` is false when the response carried no usage at all. That is
// not an error and it is not free: a `requests` price still charges, which is
// the whole point of having that unit. A `tokens` price cannot bill an
// unknown token count, so it reports that nothing is chargeable and the
// caller leaves the quota alone rather than deducting a guess.
func chargeFor(price Price, usage tokenBreakdown, usageKnown bool) (int64, bool, error) {
	var micros int64
	switch price.Unit {
	case UnitRequests:
		micros = price.RequestMicros
	case UnitTokens:
		if !usageKnown {
			return 0, false, nil
		}
		micros = usage.Input*price.InputMicros +
			usage.Output*price.OutputMicros +
			usage.CacheRead*price.CacheReadMicros +
			usage.CacheWrite*price.CacheWriteMicros
	default:
		return 0, false, fmt.Errorf("unknown unit %q", price.Unit)
	}

	per := price.Per
	if per <= 0 {
		per = 1
	}
	// Round half up on the divided amount so a price quoted per 1000 tokens
	// does not lose its last digit on every request.
	credits := (micros + per*microsPerCredit/2) / (per * microsPerCredit)

	if credits == 0 && micros > 0 && price.MinCharge > 0 {
		credits = price.MinCharge
	}
	if credits < 0 || credits > maxCharge {
		return 0, false, fmt.Errorf("charge %d is out of range", credits)
	}
	return credits, true, nil
}
