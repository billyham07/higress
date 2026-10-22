package main

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/resp"

	"github.com/alibaba/higress/plugins/wasm-go/extensions/ai-quota/util"
)

// The quota wallet is the Redis contract this plugin shares with the admin
// console. The console writes both hashes; this plugin only reads them at
// admission and spends them after the response. Every amount is in tokens.
//
//	quota_key:<consumer>     which wallet a key spends, and the key's own cap
//	  owner        wallet id: "u:<user>", "g:<project>" or "p:<consumer>"
//	  mode         none | period | once
//	  limit        the cap in tokens; ignored when mode is none
//	  period       the wallet period period_used was counted in
//	  period_used  spent by this key in that period
//	  once_used    spent by this key since its one-time cap was last set
//
//	quota_wallet:<owner>     what an account has left
//	  period       the current refresh period; the console moves it
//	  allowance    what one period grants
//	  period_left  left of this period; negative is overshoot, which the
//	               next refresh forgives
//	  unlimited    "1" admits the wallet's keys whatever it holds; absent
//	               otherwise. Such a wallet is still charged, so period_left
//	               keeps counting what the period cost.
//
// A key never holds tokens of its own. Its cap only decides whether it may
// keep spending the wallet, so a set of capped keys need not add up to the
// account's allowance, and an uncapped key spends the account directly.
//
// media-gen (infra/media-gen) spends the same wallets for its paid Bailian
// jobs: same admission rules at submit, this charge script plus a per-job
// marker after the job succeeded. A change to the fields or to admission here
// is a change there too.
const (
	quotaKeyPrefix    = "quota_key:"
	quotaWalletPrefix = "quota_wallet:"

	walletOwnerContextKey = "ai-quota-wallet-owner"

	keyLimitNone   = "none"
	keyLimitPeriod = "period"
	keyLimitOnce   = "once"
)

var errNoWalletOwner = errors.New("request was not admitted against a quota wallet")

// chargeScript spends one request's tokens. The whole amount comes off
// period_left even when it was smaller: a request admitted with a little left
// is still charged in full, and the overshoot is the refresh's to forgive.
//
// The key's counters move by the same amount. A key whose recorded period is
// not the wallet's has spent nothing in this one, so its period counter
// restarts from zero here; the console never walks every key when a period
// turns over.
//
// KEYS[1] quota_key:<consumer>, KEYS[2] quota_wallet:<owner>
// ARGV[1] tokens, a non-negative integer
//
// Replies: {'ok', period_left} | {'missing'} | {'invalid'}
const chargeScript = `
local amount = tonumber(ARGV[1])
if not amount or amount < 0 or amount ~= math.floor(amount) then
  return {'invalid'}
end
local period = redis.call('HGET', KEYS[2], 'period')
if not period then
  return {'missing'}
end
local left = redis.call('HINCRBY', KEYS[2], 'period_left', -amount)
if redis.call('EXISTS', KEYS[1]) == 1 then
  if redis.call('HGET', KEYS[1], 'period') ~= period then
    redis.call('HSET', KEYS[1], 'period', period, 'period_used', 0)
  end
  redis.call('HINCRBY', KEYS[1], 'period_used', amount)
  redis.call('HINCRBY', KEYS[1], 'once_used', amount)
end
return {'ok', left}
`

// quotaKey is the admission view of one quota_key hash.
type quotaKey struct {
	owner      string
	mode       string
	limit      int64
	period     string
	periodUsed int64
	onceUsed   int64
}

// quotaWallet is the admission view of one quota_wallet hash.
type quotaWallet struct {
	period     string
	periodLeft int64
	unlimited  bool
}

// allows reports whether a key's own cap still lets it spend.
func (key quotaKey) allows(wallet quotaWallet) bool {
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

var quotaKeyFields = []string{"owner", "mode", "limit", "period", "period_used", "once_used"}
var quotaWalletFields = []string{"period", "period_left", "unlimited"}

func parseQuotaKey(response resp.Value) (quotaKey, bool) {
	values := response.Array()
	if response.Error() != nil || len(values) != len(quotaKeyFields) || values[0].IsNull() || values[0].String() == "" {
		return quotaKey{}, false
	}
	key := quotaKey{
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

func parseQuotaWallet(response resp.Value) (quotaWallet, bool) {
	values := response.Array()
	if response.Error() != nil || len(values) != len(quotaWalletFields) || values[0].IsNull() {
		return quotaWallet{}, false
	}
	return quotaWallet{
		period:     values[0].String(),
		periodLeft: respInt(values[1]),
		unlimited:  !values[2].IsNull() && values[2].String() == "1",
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

// admitQuota decides whether a consumer may start a request, and remembers
// which wallet it spends so the charge does not have to look it up again.
//
// Every failure denies. A key with no quota record was not issued by the
// console, and a Redis error is not evidence that the account can pay: this
// gate has always been fail-closed and stays that way.
func admitQuota(ctx wrapper.HttpContext, config QuotaConfig, consumer string) error {
	return config.redisClient.HMGet(quotaKeyPrefix+consumer, quotaKeyFields, func(response resp.Value) {
		key, ok := parseQuotaKey(response)
		if !ok {
			log.Debugf("consumer %s has no quota key: %v", consumer, response.Error())
			deniedNoQuotaAccount()
			return
		}
		err := config.redisClient.HMGet(quotaWalletPrefix+key.owner, quotaWalletFields, func(response resp.Value) {
			wallet, ok := parseQuotaWallet(response)
			if !ok {
				log.Debugf("consumer %s spends missing wallet %s: %v", consumer, key.owner, response.Error())
				deniedNoQuotaAccount()
				return
			}
			log.Debugf("consumer:%s wallet:%s period_left:%d unlimited:%t key_mode:%s",
				consumer, key.owner, wallet.periodLeft, wallet.unlimited, key.mode)
			if !wallet.unlimited && wallet.periodLeft <= 0 {
				util.SendResponse(http.StatusForbidden, "ai-quota.noquota", "text/plain", "Request denied by ai quota check, No quota left")
				return
			}
			if !key.allows(wallet) {
				util.SendResponse(http.StatusForbidden, "ai-quota.key_limit", "text/plain", "Request denied by ai quota check, this key has reached its own token limit")
				return
			}
			ctx.SetContext(walletOwnerContextKey, key.owner)
			proxywasm.ResumeHttpRequest()
		})
		if err != nil {
			log.Errorf("failed to read wallet %s: %v", key.owner, err)
			deniedNoQuotaAccount()
		}
	})
}

// spendQuota charges the wallet admission resolved.
func spendQuota(ctx wrapper.HttpContext, config QuotaConfig, consumer string, tokens int64) error {
	owner := ctx.GetStringContext(walletOwnerContextKey, "")
	if owner == "" {
		// Admission always records the owner before resuming, so this is a
		// request that was never admitted by this plugin.
		return errNoWalletOwner
	}
	keys := []interface{}{quotaKeyPrefix + consumer, quotaWalletPrefix + owner}
	args := []interface{}{tokens}
	return config.redisClient.Eval(chargeScript, len(keys), keys, args, func(response resp.Value) {
		values := response.Array()
		if response.Error() != nil || len(values) == 0 || values[0].String() != "ok" {
			log.Errorf("quota charge for %s on %s was not applied: %v %v", consumer, owner, response.Error(), values)
			return
		}
		log.Debugf("charged %s on %s: %d tokens, period_left %s", consumer, owner, tokens, values[1].String())
	})
}

func deniedNoQuotaAccount() {
	util.SendResponse(http.StatusForbidden, "ai-quota.no_account", "text/plain", "Request denied by ai quota check, this key has no quota account")
}
