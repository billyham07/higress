package main

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

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

// Metering units.
//
// `tokens` prices the four token components. `requests` prices one call
// regardless of what it produced. The other two exist because this gateway
// fronts endpoints that genuinely do not deal in tokens, and the upstreams
// already report the right number:
//
//	seconds     -- transcription. Qwen3-ASR answers
//	               {"usage":{"type":"duration","seconds":1}}, so the billable
//	               quantity arrives in the response like a token count does.
//	characters  -- speech synthesis. The response is audio and carries no
//	               usage at all, so the quantity has to come from the REQUEST
//	               text. This is the only unit metered before the upstream is
//	               even called, and the only one that costs a body buffer.
//
// Inventing a token count for these -- which is what the old fixed-83333
// multimodal rule did -- prices them against a number nobody can check.
const (
	UnitTokens     = "tokens"
	UnitRequests   = "requests"
	UnitSeconds    = "seconds"
	UnitCharacters = "characters"
)

// characterFields are the request fields a `characters` price may count,
// tried in order. `input` is what OpenAI's /v1/audio/speech uses and what
// Qwen3-TTS accepts; `text` covers providers that kept their own name.
//
// If none of them is present the request is reported as UNMETERED rather
// than as zero characters. A missing field is a configuration or provider
// mismatch, and billing it as free would hide that indefinitely.
var characterFields = []string{"input", "text"}

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
	SecondMicros     int64
	CharacterMicros  int64
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
	case UnitTokens, UnitRequests, UnitSeconds, UnitCharacters:
	default:
		return Price{}, fmt.Errorf("%s.unit must be one of %s, %s, %s, %s",
			where, UnitTokens, UnitRequests, UnitSeconds, UnitCharacters)
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
		{"second_micros", &price.SecondMicros},
		{"character_micros", &price.CharacterMicros},
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

// metered is everything about one request that a price might be applied to.
//
// It exists so that adding a unit does not add two more parameters to
// chargeFor. Each quantity carries its own "known" flag rather than relying
// on a zero value, because "the upstream reported zero" and "nothing was
// reported" must lead to different outcomes: the first is a real bill of
// zero, the second leaves the quota untouched.
type metered struct {
	tokens      tokenBreakdown
	tokensKnown bool
	// seconds comes from the response, like tokens do.
	seconds      int64
	secondsKnown bool
	// characters comes from the REQUEST body, so it is known before the
	// upstream answers -- and stays known even when the upstream fails,
	// which is why succeeded still has to gate it.
	characters      int64
	charactersKnown bool
	succeeded       bool
}

// chargeFor converts one request's metered usage into whole credits.
//
// The second return value is "chargeable". False means this request must not
// move the quota at all -- not that it is free. A `tokens` price cannot bill
// a response that reported no usage, and a `characters` price cannot bill a
// request whose text field was not found; in both cases deducting a guess
// would be worse than deducting nothing.
//
// Status gates the two units that bill an ATTEMPT rather than a product:
//
//	requests    -- an upstream 500 would otherwise charge a full request.
//	characters  -- the text was submitted but no audio came back.
//
// It deliberately does not gate `tokens` or `seconds`. Those bill what was
// actually produced, so a stream that answered, billed real tokens and then
// broke stays charged: the tokens were generated, and refunding them because
// the connection died afterwards would be the wrong correction.
//
// Not covered: an upstream that reports failure in the body of a 200.
// Nothing here inspects the body's shape, so such a response is charged.
// That is a real gap, left open rather than guessed at, because the shape
// differs per provider and a heuristic that silently stops billing would be
// worse than one that visibly over-bills.
func chargeFor(price Price, usage metered) (int64, bool, error) {
	var micros int64
	switch price.Unit {
	case UnitRequests:
		if !usage.succeeded {
			return 0, false, nil
		}
		micros = price.RequestMicros
	case UnitCharacters:
		if !usage.charactersKnown {
			return 0, false, nil
		}
		if !usage.succeeded {
			return 0, false, nil
		}
		micros = usage.characters * price.CharacterMicros
	case UnitSeconds:
		if !usage.secondsKnown {
			return 0, false, nil
		}
		micros = usage.seconds * price.SecondMicros
	case UnitTokens:
		if !usage.tokensKnown {
			return 0, false, nil
		}
		micros = usage.tokens.Input*price.InputMicros +
			usage.tokens.Output*price.OutputMicros +
			usage.tokens.CacheRead*price.CacheReadMicros +
			usage.tokens.CacheWrite*price.CacheWriteMicros
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

// countRequestCharacters measures the text a `characters` price bills for.
//
// It counts RUNES, not bytes. The gateway's traffic is largely Chinese, and
// UTF-8 spends three bytes on each of those characters -- billing len() would
// charge a Chinese sentence three times what the same sentence costs in
// English, for identical synthesis work.
//
// The `false` return means no candidate field was present, which the caller
// turns into "not chargeable" rather than into zero. See characterFields.
func countRequestCharacters(body []byte) (int64, bool) {
	for _, field := range characterFields {
		value := gjson.GetBytes(body, field)
		if !value.Exists() {
			continue
		}
		// A provider that accepts an array of strings bills the whole batch.
		if value.IsArray() {
			var total int64
			for _, element := range value.Array() {
				total += int64(utf8.RuneCountInString(element.String()))
			}
			return total, true
		}
		if value.Type != gjson.String {
			continue
		}
		return int64(utf8.RuneCountInString(value.String())), true
	}
	return 0, false
}

// responseSeconds reads the duration a `seconds` price bills for.
//
// Qwen3-ASR answers {"usage":{"type":"duration","seconds":1}}. The type is
// checked rather than assumed: a provider reporting a different kind of
// usage under the same key must not have its number billed as seconds.
func responseSeconds(body []byte) (int64, bool) {
	usage := gjson.GetBytes(body, "usage")
	if !usage.Exists() {
		return 0, false
	}
	if kind := usage.Get("type"); kind.Exists() && kind.String() != "duration" {
		return 0, false
	}
	seconds := usage.Get("seconds")
	if !seconds.Exists() || seconds.Type != gjson.Number {
		return 0, false
	}
	// Durations are reported fractionally by some providers. A partial second
	// is rounded UP: it consumed a second's worth of capacity, and rounding
	// down would make every sub-second clip free.
	value := seconds.Float()
	if value < 0 {
		return 0, false
	}
	rounded := int64(value)
	if float64(rounded) < value {
		rounded++
	}
	return rounded, true
}
