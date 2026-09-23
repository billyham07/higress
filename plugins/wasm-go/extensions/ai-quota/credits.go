package main

import (
	"net/http"
	"strconv"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/resp"

	"github.com/alibaba/higress/plugins/wasm-go/extensions/ai-quota/util"
)

// The credit wallet is the Redis contract this plugin shares with the admin
// console. The console writes both hashes; this plugin only ever reads them at
// admission and spends them after the response.
//
//	credit_key:<consumer>     which wallet a key spends, and the key's own cap
//	  owner        wallet id, e.g. "u:61" or "p:<consumer>"
//	  mode         none | period | once
//	  limit        the cap in milli-credits; ignored when mode is none
//	  period       the wallet period period_used was counted in
//	  period_used  spent by this key in that period
//	  once_used    spent by this key since its one-time cap was last set
//
//	credit_wallet:<owner>     what an account has left
//	  period       the current refresh period; the console moves it
//	  allowance    what one period grants
//	  period_left  left of this period's allowance; negative is overshoot
//	  extra_left   one-time credits, never touched by a refresh
//
// A key never holds credits of its own. Its cap only decides whether it may
// keep spending the wallet, which is why a set of capped keys need not add up
// to the account's allowance.
const (
	creditKeyPrefix    = "credit_key:"
	creditWalletPrefix = "credit_wallet:"

	creditOwnerContextKey = "ai-quota-credit-owner"

	keyLimitNone   = "none"
	keyLimitPeriod = "period"
	keyLimitOnce   = "once"
)

// chargeScript spends one request's cost: this period's allowance first, then
// the one-time credits. Whatever neither can cover lands on period_left as
// overshoot, so a request admitted with a little left is still charged in full
// and the next refresh -- not the extra credits -- absorbs the difference.
//
// The key's counters move by the whole amount whichever bucket paid. A key
// whose recorded period is not the wallet's has not spent anything in this
// one, so its period counter restarts from zero here; the console never has
// to walk every key when a period turns over.
//
// KEYS[1] credit_key:<consumer>, KEYS[2] credit_wallet:<owner>
// ARGV[1] amount in milli-credits, a non-negative integer
//
// Replies: {'ok', from_period, from_extra, overshoot} | {'missing'} | {'invalid'}
const chargeScript = `
local amount = tonumber(ARGV[1])
if not amount or amount < 0 or amount ~= math.floor(amount) then
  return {'invalid'}
end
local wallet = redis.call('HMGET', KEYS[2], 'period', 'period_left', 'extra_left')
if not wallet[1] then
  return {'missing'}
end
local period_left = tonumber(wallet[2]) or 0
local extra_left = tonumber(wallet[3]) or 0
local from_period = math.min(math.max(period_left, 0), amount)
local rest = amount - from_period
local from_extra = math.min(math.max(extra_left, 0), rest)
local overshoot = rest - from_extra
if from_period + overshoot > 0 then
  redis.call('HINCRBY', KEYS[2], 'period_left', -(from_period + overshoot))
end
if from_extra > 0 then
  redis.call('HINCRBY', KEYS[2], 'extra_left', -from_extra)
end
if redis.call('EXISTS', KEYS[1]) == 1 then
  if redis.call('HGET', KEYS[1], 'period') ~= wallet[1] then
    redis.call('HSET', KEYS[1], 'period', wallet[1], 'period_used', 0)
  end
  redis.call('HINCRBY', KEYS[1], 'period_used', amount)
  redis.call('HINCRBY', KEYS[1], 'once_used', amount)
end
return {'ok', from_period, from_extra, overshoot}
`

// creditKey is the admission view of one credit_key hash.
type creditKey struct {
	owner      string
	mode       string
	limit      int64
	period     string
	periodUsed int64
	onceUsed   int64
}

// creditWallet is the admission view of one credit_wallet hash.
type creditWallet struct {
	period     string
	periodLeft int64
	extraLeft  int64
}

// available is what the wallet can still pay for. Overshoot on the period
// bucket does not eat into the extra credits: it is settled by the refresh.
func (wallet creditWallet) available() int64 {
	return max(wallet.periodLeft, 0) + max(wallet.extraLeft, 0)
}

// keyAllows reports whether a key's own cap still lets it spend.
func (key creditKey) allows(wallet creditWallet) bool {
	switch key.mode {
	case keyLimitPeriod:
		used := key.periodUsed
		if key.period != wallet.period {
			used = 0
		}
		return used < key.limit
	case keyLimitOnce:
		return key.onceUsed < key.limit
	default:
		return true
	}
}

var creditKeyFields = []string{"owner", "mode", "limit", "period", "period_used", "once_used"}
var creditWalletFields = []string{"period", "period_left", "extra_left"}

func parseCreditKey(response resp.Value) (creditKey, bool) {
	values := response.Array()
	if response.Error() != nil || len(values) != len(creditKeyFields) || values[0].IsNull() || values[0].String() == "" {
		return creditKey{}, false
	}
	key := creditKey{
		owner:      values[0].String(),
		mode:       values[1].String(),
		limit:      respInt(values[2]),
		period:     values[3].String(),
		periodUsed: respInt(values[4]),
		onceUsed:   respInt(values[5]),
	}
	if key.mode == "" {
		key.mode = keyLimitNone
	}
	return key, true
}

func parseCreditWallet(response resp.Value) (creditWallet, bool) {
	values := response.Array()
	if response.Error() != nil || len(values) != len(creditWalletFields) || values[0].IsNull() {
		return creditWallet{}, false
	}
	return creditWallet{
		period:     values[0].String(),
		periodLeft: respInt(values[1]),
		extraLeft:  respInt(values[2]),
	}, true
}

// respInt reads a hash field. HMGET answers with bulk strings, and a missing
// field is null, which reads as zero -- a counter nobody has moved yet.
func respInt(value resp.Value) int64 {
	if value.IsNull() {
		return 0
	}
	parsed, err := strconv.ParseInt(value.String(), 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// admitCredits decides whether a consumer may start a request, and remembers
// which wallet it spends so the charge does not have to look it up again.
//
// Every failure denies. A key with no credit record was not issued by the
// console, and a Redis error is not evidence that the account can pay: this
// gate has always been fail-closed and stays that way.
func admitCredits(ctx wrapper.HttpContext, config QuotaConfig, consumer string) error {
	return config.redisClient.HMGet(creditKeyPrefix+consumer, creditKeyFields, func(response resp.Value) {
		key, ok := parseCreditKey(response)
		if !ok {
			log.Debugf("consumer %s has no credit key: %v", consumer, response.Error())
			deniedNoCreditAccount()
			return
		}
		err := config.redisClient.HMGet(creditWalletPrefix+key.owner, creditWalletFields, func(response resp.Value) {
			wallet, ok := parseCreditWallet(response)
			if !ok {
				log.Debugf("consumer %s spends missing wallet %s: %v", consumer, key.owner, response.Error())
				deniedNoCreditAccount()
				return
			}
			log.Debugf("consumer:%s wallet:%s period_left:%d extra_left:%d key_mode:%s",
				consumer, key.owner, wallet.periodLeft, wallet.extraLeft, key.mode)
			if wallet.available() <= 0 {
				util.SendResponse(http.StatusForbidden, "ai-quota.noquota", "text/plain", "Request denied by ai quota check, No quota left")
				return
			}
			if !key.allows(wallet) {
				util.SendResponse(http.StatusForbidden, "ai-quota.key_limit", "text/plain", "Request denied by ai quota check, this key has reached its own credit limit")
				return
			}
			ctx.SetContext(creditOwnerContextKey, key.owner)
			proxywasm.ResumeHttpRequest()
		})
		if err != nil {
			log.Errorf("failed to read wallet %s: %v", key.owner, err)
			deniedNoCreditAccount()
		}
	})
}

// spendCredits charges the wallet admission resolved.
func spendCredits(ctx wrapper.HttpContext, config QuotaConfig, consumer string, amount int64) error {
	owner := ctx.GetStringContext(creditOwnerContextKey, "")
	if owner == "" {
		// Admission always records the owner before resuming, so this is a
		// request that was never admitted by this plugin.
		return errNoCreditOwner
	}
	keys := []interface{}{creditKeyPrefix + consumer, creditWalletPrefix + owner}
	args := []interface{}{amount}
	return config.redisClient.Eval(chargeScript, len(keys), keys, args, func(response resp.Value) {
		values := response.Array()
		if response.Error() != nil || len(values) == 0 || values[0].String() != "ok" {
			log.Errorf("credit charge for %s on %s was not applied: %v %v", consumer, owner, response.Error(), values)
			return
		}
		log.Debugf("charged %s on %s: period %s extra %s overshoot %s",
			consumer, owner, values[1].String(), values[2].String(), values[3].String())
	})
}

func deniedNoCreditAccount() {
	util.SendResponse(http.StatusForbidden, "ai-quota.no_account", "text/plain", "Request denied by ai quota check, this key has no credit account")
}
