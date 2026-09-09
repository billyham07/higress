package main

// Redis callouts made by an HTTP context are tied to that context. Envoy may
// destroy it as soon as the response stream ends, which can drop a pending
// HSET callback. The root PluginContext remains alive, so it can finish the
// durability handoff without retaining or dereferencing a destroyed
// HttpContext.
//
// WAITAOF only covers writes issued earlier on its own Redis client
// connection. A read-back on another connection is not a persistence proof.
// Each attempt below therefore queues an idempotent HSET of the exact outbox
// value and WAITAOF back-to-back through the same configured Redis client.
// Higress' Redis async client keeps one RawClient connection per selected host
// and pipelines requests FIFO; if either callback reports a connection or
// fsync failure, the whole pair is retried. The HSET in the pair is itself a
// fresh write, so a reconnect before an attempt still leaves a value whose
// own connection-local WAITAOF can be confirmed.

import (
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/resp"
)

const (
	// realEnvVerifierBuildID is deliberately short and contains no deployment
	// credentials. It lets a real gateway log prove which verifier binary
	// handled a fault-injection request while the image/WASM digests remain in
	// the artifact trace.
	realEnvVerifierBuildID = "umc-real21-verifier-detached-retry"

	// A callback that never arrives must not leave a verifier permanently
	// in-flight. The timeout is expressed in host ticks rather than wall-clock
	// time because the compiled-WASM test host has a fixed wall clock.
	verifierBarrierTimeoutTicks uint64 = 60
	verifierInitialRetryTicks   uint64 = 1
	verifierMaxRetryTicks       uint64 = 50
	verifierTraceQueueLimit            = 64

	// A durable outbox record is the final recovery witness, but a root
	// context can often deliver it without waiting for the replay job. Keep a
	// failed detached HTTP delivery on the root scheduler for a bounded window
	// instead of exhausting all attempts in one callback turn. This covers a
	// backend that is temporarily unready while its Redis dependency returns.
	detachedSettlementRetryWindowTicks uint64 = 600
	detachedSettlementCallTimeoutTicks uint64 = 60
)

var errVerifierBarrierTimeout = errors.New("redis write barrier callback timeout")

type outboxLifecycle struct {
	// live is only read and written by the WASM event loop. It is a pointer so
	// a root callback can observe that the HTTP stream has ended without
	// retaining or dereferencing a destroyed HttpContext.
	live bool
}

type outboxTraceEvent struct {
	event      string
	attempt    uint64
	generation uint64
	ready      bool
	errText    string
}

type outboxVerification struct {
	outbox        *SettlementOutbox
	reservationID string
	raw           string
	finish        func(error)
	rootID        uint64
	generation    uint64

	barrierInFlight  bool
	barrierWriteDone bool
	barrierWriteErr  error
	barrierWaitDone  bool
	barrierWaitErr   error
	barrierAttempt   uint64
	barrierStartTick uint64

	// Reconnect and retry are deliberately scheduled by the root tick. A
	// Redis callback may run while Envoy is closing the RawClient; calling
	// RedisInit/close from that callback re-enters the host lifecycle and was
	// observed to starve an Envoy worker. The callback records state only.
	reconnectPending  bool
	nextRetryTick     uint64
	retryBackoff      uint64
	completionPending bool
	cancelled         bool
	completed         bool

	// Trace output is bounded. The queue is filled by callbacks and drained by
	// the root tick, so logging itself is also kept out of callback execution.
	traceMu      sync.Mutex
	tracePending []outboxTraceEvent
	traceNextLog map[string]time.Time
}

var outboxVerifierState = struct {
	sync.Mutex
	registered bool
	pending    map[string]*outboxVerification
	tick       uint64
	nextRootID uint64
}{
	pending: make(map[string]*outboxVerification),
}

// detachedSettlement is deliberately independent of an HTTP stream context.
// The Redis write-barrier has already completed when one is queued, so the
// exact outbox bytes remain the authority while the root context retries the
// backend delivery. A map keyed by outbox hash and reservation prevents two
// completions from sending duplicate detached requests for the same record.
type detachedSettlement struct {
	key          string
	config       CreditsConfig
	req          SettleRequest
	raw          string
	attempt      int
	startTick    uint64
	nextTick     uint64
	retryBackoff uint64
	inFlight     bool
	callStart    uint64
	sequence     uint64
}

var detachedSettlementState = struct {
	sync.Mutex
	pending map[string]*detachedSettlement
	nextSeq uint64
}{
	pending: make(map[string]*detachedSettlement),
}

// outboxReconnect is a narrow seam for state-machine tests. Production uses
// SettlementOutbox.reconnect; tests replace it to prove that callbacks never
// perform reconnect work synchronously.
var outboxReconnect = func(outbox *SettlementOutbox) error {
	return outbox.reconnect()
}

var verifierLogWarnf = log.Warnf

func resetOutboxVerifier() {
	outboxVerifierState.Lock()
	defer outboxVerifierState.Unlock()
	outboxVerifierState.registered = false
	outboxVerifierState.pending = make(map[string]*outboxVerification)
	outboxVerifierState.tick = 0
	outboxVerifierState.nextRootID = 0
	detachedSettlementState.Lock()
	detachedSettlementState.pending = make(map[string]*detachedSettlement)
	detachedSettlementState.nextSeq = 0
	detachedSettlementState.Unlock()
}

func ensureOutboxVerifier() {
	outboxVerifierState.Lock()
	defer outboxVerifierState.Unlock()
	if outboxVerifierState.registered {
		return
	}
	// Run on every host tick rather than adding another wall-clock gate. The
	// compiled-WASM test host has a fixed wall clock, and a second gate would
	// prevent retries there even though Envoy continues to deliver ticks.
	wrapper.RegisterTickFunc(0, processOutboxVerifications)
	outboxVerifierState.registered = true
}

func outboxVerificationKey(outbox *SettlementOutbox, reservationID string) string {
	return outbox.Key + "\x00" + reservationID
}

func enqueueOutboxVerification(outbox *SettlementOutbox, reservationID string, raw []byte, finish func(error)) *outboxVerification {
	if outbox == nil || outbox.Client == nil || reservationID == "" {
		return nil
	}
	outboxVerifierState.Lock()
	outboxVerifierState.nextRootID++
	v := &outboxVerification{
		outbox:        outbox,
		reservationID: reservationID,
		raw:           string(raw),
		finish:        finish,
		rootID:        outboxVerifierState.nextRootID,
		generation:    1,
		traceNextLog:  make(map[string]time.Time),
	}
	outboxVerifierState.pending[outboxVerificationKey(outbox, reservationID)] = v
	outboxVerifierState.Unlock()
	return v
}

func currentVerifierTick() uint64 {
	outboxVerifierState.Lock()
	tick := outboxVerifierState.tick
	outboxVerifierState.Unlock()
	return tick
}

func detachedSettlementKey(config CreditsConfig, reservationID string) string {
	key := ""
	if config.Outbox != nil {
		key = config.Outbox.Key
	}
	cluster := ""
	if config.Client != nil {
		cluster = config.Client.ClusterName()
	}
	return key + "\x00" + cluster + "\x00" + reservationID
}

// enqueueDetachedSettlement moves delivery onto the root scheduler. The
// caller may be running from a Redis callback, or from the final root-tick
// callback of an HTTP stream; neither path should recursively dispatch a new
// HTTP callout and exhaust all retries in that same callback turn.
func enqueueDetachedSettlement(config CreditsConfig, req SettleRequest, raw string, attempt int) {
	if config.Client == nil || config.Outbox == nil || req.ReservationID == "" {
		return
	}
	if attempt < 0 {
		attempt = 0
	}
	tick := currentVerifierTick()
	key := detachedSettlementKey(config, req.ReservationID)
	detachedSettlementState.Lock()
	defer detachedSettlementState.Unlock()
	if _, exists := detachedSettlementState.pending[key]; exists {
		return
	}
	detachedSettlementState.nextSeq++
	detachedSettlementState.pending[key] = &detachedSettlement{
		key:       key,
		config:    config,
		req:       req,
		raw:       raw,
		attempt:   attempt,
		startTick: tick,
		nextTick:  tick,
		sequence:  detachedSettlementState.nextSeq,
	}
}

func detachedRetryableStatus(statusCode int, err error) bool {
	if err != nil {
		return true
	}
	return statusCode >= 500 || statusCode == 429 || statusCode == 408
}

func scheduleDetachedSettlementRetryLocked(item *detachedSettlement, tick uint64) uint64 {
	delay := item.retryBackoff
	if delay == 0 {
		delay = verifierInitialRetryTicks
	}
	item.nextTick = tick + delay
	if delay < verifierMaxRetryTicks {
		delay *= 2
		if delay > verifierMaxRetryTicks {
			delay = verifierMaxRetryTicks
		}
	}
	item.retryBackoff = delay
	item.attempt++
	return item.nextTick - tick
}

func takeDetachedSettlement(tick uint64) *detachedSettlement {
	detachedSettlementState.Lock()
	var selected *detachedSettlement
	var expiredReservations []string
	for key, item := range detachedSettlementState.pending {
		if item == nil {
			delete(detachedSettlementState.pending, key)
			continue
		}
		// A lost HTTP callback must not strand the record in memory forever.
		// The durable Redis value remains untouched and will be picked up by the
		// replay worker if this bounded in-memory delivery window expires.
		if item.inFlight && tick >= item.callStart+detachedSettlementCallTimeoutTicks {
			item.inFlight = false
			if tick >= item.startTick+detachedSettlementRetryWindowTicks {
				delete(detachedSettlementState.pending, key)
				expiredReservations = append(expiredReservations, item.req.ReservationID)
				continue
			}
			_ = scheduleDetachedSettlementRetryLocked(item, tick)
		}
		if item.inFlight || tick < item.nextTick {
			continue
		}
		if tick >= item.startTick+detachedSettlementRetryWindowTicks {
			delete(detachedSettlementState.pending, key)
			expiredReservations = append(expiredReservations, item.req.ReservationID)
			continue
		}
		if selected == nil || item.sequence < selected.sequence {
			selected = item
		}
	}
	if selected != nil {
		selected.inFlight = true
		selected.callStart = tick
	}
	detachedSettlementState.Unlock()
	for _, reservationID := range expiredReservations {
		log.Warnf("ai-credits detached settlement retry window expired reservation=%s; retained in outbox", reservationID)
	}
	return selected
}

func finishDetachedSettlement(key string, callAttempt int, statusCode int, err error) {
	tick := currentVerifierTick()
	var (
		item       *detachedSettlement
		succeeded  bool
		retry      bool
		retryDelay uint64
		exhausted  bool
		permanent  bool
	)
	detachedSettlementState.Lock()
	item = detachedSettlementState.pending[key]
	if item == nil || !item.inFlight || item.attempt != callAttempt {
		detachedSettlementState.Unlock()
		return
	}
	item.inFlight = false
	if err == nil && statusCode/100 == 2 {
		delete(detachedSettlementState.pending, key)
		succeeded = true
	} else if detachedRetryableStatus(statusCode, err) && tick < item.startTick+detachedSettlementRetryWindowTicks {
		retryDelay = scheduleDetachedSettlementRetryLocked(item, tick)
		retry = true
	} else {
		delete(detachedSettlementState.pending, key)
		exhausted = err != nil || statusCode >= 500 || statusCode == 429 || statusCode == 408
		permanent = !exhausted
	}
	detachedSettlementState.Unlock()

	if succeeded {
		// The backend's 2xx is the only authority that permits dropping the
		// durable record. Compare-delete still protects a newer value for the
		// same reservation field.
		outboxForget(item.config, item.req.ReservationID, item.raw)
		return
	}
	if retry {
		log.Warnf("ai-credits detached settlement retry scheduled reservation=%s attempt=%d delay_ticks=%d status=%d error=%v",
			item.req.ReservationID, item.attempt, retryDelay, statusCode, err)
		return
	}
	if exhausted {
		log.Errorf("ai-credits detached settle exhausted reservation=%s attempt=%d status=%d error=%v; retained in outbox",
			item.req.ReservationID, item.attempt, statusCode, err)
		return
	}
	if permanent {
		log.Errorf("ai-credits detached settle rejected reservation=%s attempt=%d status=%d; retained in outbox",
			item.req.ReservationID, item.attempt, statusCode)
	}
}

func dispatchNextDetachedSettlement(tick uint64) {
	item := takeDetachedSettlement(tick)
	if item == nil {
		return
	}
	callAttempt := item.attempt
	headers := [][2]string{
		{"content-type", "application/json"},
		{item.config.AuthHeader, item.config.AuthKey},
		{"x-ai-credits-contract", CreditsContractVersion},
	}
	payload := marshalJSON(item.req)
	err := item.config.Client.Call("POST", item.config.SettlePath, headers, payload,
		func(statusCode int, _ http.Header, _ []byte) {
			finishDetachedSettlement(item.key, callAttempt, statusCode, nil)
		}, item.config.Timeout)
	if err != nil {
		finishDetachedSettlement(item.key, callAttempt, 0, err)
	}
}

// traceOutboxVerification only appends an event. In particular, it does not
// call log.Warnf or inspect a host-side client from a Redis callback.
func traceOutboxVerification(v *outboxVerification, event string, attempt uint64, ready bool,
	err error) {
	if v == nil || event == "" {
		return
	}
	outboxVerifierState.Lock()
	generation := v.generation
	outboxVerifierState.Unlock()
	item := outboxTraceEvent{
		event:      event,
		attempt:    attempt,
		generation: generation,
		ready:      ready,
	}
	if err != nil {
		item.errText = err.Error()
	}
	v.traceMu.Lock()
	if len(v.tracePending) < verifierTraceQueueLimit {
		v.tracePending = append(v.tracePending, item)
	}
	v.traceMu.Unlock()
}

func emitOutboxVerificationTraces(v *outboxVerification) {
	if v == nil {
		return
	}
	v.traceMu.Lock()
	items := append([]outboxTraceEvent(nil), v.tracePending...)
	v.tracePending = nil
	if v.traceNextLog == nil {
		v.traceNextLog = make(map[string]time.Time)
	}
	v.traceMu.Unlock()
	if len(items) == 0 {
		return
	}
	now := time.Now()
	outboxVerifierState.Lock()
	rootID := v.rootID
	reservationID := v.reservationID
	outboxVerifierState.Unlock()
	for _, item := range items {
		v.traceMu.Lock()
		next := v.traceNextLog[item.event]
		if now.Before(next) {
			v.traceMu.Unlock()
			continue
		}
		v.traceNextLog[item.event] = now.Add(time.Second)
		v.traceMu.Unlock()
		verifierLogWarnf("ai-credits real verifier build=%s root_id=%d generation=%d reservation=%s attempt=%d event=%s ready=%t error=%s",
			realEnvVerifierBuildID, rootID, item.generation, reservationID, item.attempt,
			item.event, item.ready, item.errText)
	}
}

// cancelOutboxVerification removes a task only when the initial dispatch was
// rejected synchronously and no Redis command can still answer. A Redis error
// callback is handled as a retryable pair failure instead: the root tick gets
// another chance to write the exact evidence.
func cancelOutboxVerification(v *outboxVerification) {
	if v == nil {
		return
	}
	outboxVerifierState.Lock()
	defer outboxVerifierState.Unlock()
	v.cancelled = true
	key := outboxVerificationKey(v.outbox, v.reservationID)
	if current := outboxVerifierState.pending[key]; current == v {
		delete(outboxVerifierState.pending, key)
	}
}

// scheduleReconnectLocked records a bounded retry delay. The task remains in
// the pending map until a pair is durably confirmed; backoff bounds host work
// without discarding the settlement evidence.
func scheduleReconnectLocked(v *outboxVerification, tick uint64) {
	delay := v.retryBackoff
	if delay == 0 {
		delay = verifierInitialRetryTicks
	}
	v.nextRetryTick = tick + delay
	v.reconnectPending = true
	if delay < verifierMaxRetryTicks {
		delay *= 2
		if delay > verifierMaxRetryTicks {
			delay = verifierMaxRetryTicks
		}
	}
	v.retryBackoff = delay
}

func barrierError(v *outboxVerification) error {
	if v.barrierWriteErr != nil {
		return v.barrierWriteErr
	}
	return v.barrierWaitErr
}

// finishOutboxBarrierStage records one stage of a pair. It deliberately does
// not reconnect or finish the HTTP settlement. A pair is finalized only after
// both dispatched stages have either answered or been marked as not
// dispatched. This prevents a late callback from an old attempt from being
// confused with a new generation.
func finishOutboxBarrierStage(v *outboxVerification, attempt uint64, wait bool, err error,
	ready bool, traceEvent string) {
	if v == nil {
		return
	}
	var (
		retry    bool
		complete bool
		retryErr error
	)
	outboxVerifierState.Lock()
	if !v.cancelled && !v.completed && v.barrierInFlight && v.barrierAttempt == attempt {
		if wait {
			v.barrierWaitDone = true
			v.barrierWaitErr = err
		} else {
			v.barrierWriteDone = true
			v.barrierWriteErr = err
		}
		if v.barrierWriteDone && v.barrierWaitDone {
			v.barrierInFlight = false
			if v.barrierWriteErr == nil && v.barrierWaitErr == nil {
				v.completed = true
				v.completionPending = true
				complete = true
			} else {
				scheduleReconnectLocked(v, outboxVerifierState.tick)
				retryErr = barrierError(v)
				retry = true
			}
		}
	}
	outboxVerifierState.Unlock()

	if traceEvent != "" {
		traceOutboxVerification(v, traceEvent, attempt, ready, err)
	}
	if retry {
		traceOutboxVerification(v, "barrier_retry_scheduled", attempt, ready, retryErr)
	}
	if complete {
		traceOutboxVerification(v, "pair_complete", attempt, ready, nil)
	}
}

// beginOutboxBarrier claims one HSET -> WAITAOF pair. Both commands are
// dispatched by dispatchOutboxBarrier before either response is required, so
// Envoy's FIFO Redis connection is the ordering boundary rather than a
// read-only command on an unknown connection.
func beginOutboxBarrier(v *outboxVerification) (uint64, bool) {
	if v == nil {
		return 0, false
	}
	outboxVerifierState.Lock()
	defer outboxVerifierState.Unlock()
	if v.cancelled || v.barrierInFlight || v.reconnectPending || v.completed ||
		outboxVerifierState.tick < v.nextRetryTick {
		return 0, false
	}
	v.barrierInFlight = true
	v.barrierWriteDone = false
	v.barrierWriteErr = nil
	v.barrierWaitDone = false
	v.barrierWaitErr = nil
	v.barrierAttempt++
	v.barrierStartTick = outboxVerifierState.tick
	return v.barrierAttempt, true
}

func dispatchOutboxBarrier(v *outboxVerification) {
	attempt, ok := beginOutboxBarrier(v)
	if !ok {
		return
	}
	if v.outbox == nil || v.outbox.Client == nil {
		err := errors.New("redis outbox client is unavailable")
		traceOutboxVerification(v, "hset_dispatch_error", attempt, false, err)
		finishOutboxBarrierStage(v, attempt, false, err, false, "")
		finishOutboxBarrierStage(v, attempt, true, err, false, "")
		return
	}
	client := v.outbox.Client
	ready := client.Ready()
	traceOutboxVerification(v, "dispatch_start", attempt, ready, nil)
	// This duplicate HSET is intentional. It is an idempotent rewrite of the
	// exact field/value and gives WAITAOF a write offset on the same FIFO
	// connection. It also repairs the record if the original HTTP-context
	// callout was sent just before a reconnect.
	err := client.HSet(v.outbox.Key, v.reservationID, v.raw, func(response resp.Value) {
		finishOutboxBarrierStage(v, attempt, false, response.Error(), ready, "hset_callback_error")
	})
	if err != nil {
		finishOutboxBarrierStage(v, attempt, false, err, ready, "hset_dispatch_error")
		// No WAITAOF was dispatched, so close this pair explicitly. The next
		// root tick will reconnect and a later tick will dispatch a fresh pair.
		finishOutboxBarrierStage(v, attempt, true, err, ready, "")
		return
	}
	// Dispatch immediately after HSET, without waiting for its callback. The
	// Redis client queues both requests on its selected RawClient in FIFO order;
	// a connection failure makes the pending callbacks fail and causes a full
	// pair retry on a later root tick.
	err = waitForAOF(client, v.outbox.Timeout, func(waitErr error) {
		finishOutboxBarrierStage(v, attempt, true, waitErr, ready, "waitaof_callback_error")
	})
	if err != nil {
		finishOutboxBarrierStage(v, attempt, true, err, ready, "waitaof_dispatch_error")
	}
}

func expireOutboxBarrier(v *outboxVerification, tick uint64) {
	if v == nil {
		return
	}
	outboxVerifierState.Lock()
	expired := !v.cancelled && !v.completed && v.barrierInFlight &&
		tick >= v.barrierStartTick+verifierBarrierTimeoutTicks
	attempt := v.barrierAttempt
	outboxVerifierState.Unlock()
	if !expired {
		return
	}
	traceOutboxVerification(v, "barrier_timeout", attempt,
		v.outbox != nil && v.outbox.Client != nil && v.outbox.Client.Ready(), errVerifierBarrierTimeout)
	// Mark every still-pending stage. A late callback is ignored by the
	// attempt/in-flight checks after this pair is finalized.
	finishOutboxBarrierStage(v, attempt, false, errVerifierBarrierTimeout, false, "")
	finishOutboxBarrierStage(v, attempt, true, errVerifierBarrierTimeout, false, "")
}

func sortedVerifications() []*outboxVerification {
	outboxVerifierState.Lock()
	work := make([]*outboxVerification, 0, len(outboxVerifierState.pending))
	for _, v := range outboxVerifierState.pending {
		if v != nil && !v.cancelled {
			work = append(work, v)
		}
	}
	outboxVerifierState.Unlock()
	sort.Slice(work, func(i, j int) bool { return work[i].rootID < work[j].rootID })
	return work
}

func takeCompletedVerifications() []*outboxVerification {
	outboxVerifierState.Lock()
	defer outboxVerifierState.Unlock()
	completed := make([]*outboxVerification, 0)
	for key, v := range outboxVerifierState.pending {
		if v == nil || !v.completed || !v.completionPending {
			continue
		}
		v.completionPending = false
		delete(outboxVerifierState.pending, key)
		completed = append(completed, v)
	}
	return completed
}

func processOutboxVerifications() {
	outboxVerifierState.Lock()
	outboxVerifierState.tick++
	tick := outboxVerifierState.tick
	outboxVerifierState.Unlock()

	work := sortedVerifications()
	for _, v := range work {
		expireOutboxBarrier(v, tick)
	}
	work = sortedVerifications()

	// One host-side RedisInit is enough for all verifiers sharing an outbox in
	// this tick. This also prevents a large pending map from turning recovery
	// into a burst of re-entrant connection teardown.
	reconnected := make(map[*SettlementOutbox]bool)
	for _, v := range work {
		outboxVerifierState.Lock()
		eligible := !v.cancelled && !v.completed && v.reconnectPending &&
			tick >= v.nextRetryTick
		attempt := v.barrierAttempt
		ready := v.outbox != nil && v.outbox.Client != nil && v.outbox.Client.Ready()
		outbox := v.outbox
		outboxVerifierState.Unlock()
		if !eligible {
			continue
		}
		if outbox == nil || reconnected[outbox] {
			continue
		}
		reconnected[outbox] = true
		traceOutboxVerification(v, "reconnect_start", attempt, ready, nil)
		err := outboxReconnect(outbox)
		outboxVerifierState.Lock()
		if !v.cancelled && !v.completed && v.reconnectPending {
			if err == nil {
				v.reconnectPending = false
				v.nextRetryTick = tick + 1
				v.retryBackoff = verifierInitialRetryTicks
				v.generation++
			} else {
				scheduleReconnectLocked(v, tick)
			}
		}
		outboxVerifierState.Unlock()
		newReady := outbox != nil && outbox.Client != nil && outbox.Client.Ready()
		traceOutboxVerification(v, "reconnect_result", attempt, newReady, err)
	}

	// A successful reconnect gets one full root tick before a new pair. This
	// avoids RedisInit and DispatchRedisCall sharing one host callback turn.
	for _, v := range sortedVerifications() {
		outboxVerifierState.Lock()
		eligible := !v.cancelled && !v.completed && !v.reconnectPending &&
			!v.barrierInFlight && tick >= v.nextRetryTick
		outboxVerifierState.Unlock()
		if eligible {
			dispatchOutboxBarrier(v)
		}
	}

	for _, v := range work {
		emitOutboxVerificationTraces(v)
	}
	for _, v := range takeCompletedVerifications() {
		emitOutboxVerificationTraces(v)
		if v.finish != nil {
			// Completion is intentionally delivered from the root tick. Redis
			// callbacks only record state, and HTTP settlement therefore cannot
			// re-enter the connection lifecycle that produced the callback.
			v.finish(nil)
		}
		emitOutboxVerificationTraces(v)
	}
	// Detached settlement delivery is also root-context work. Dispatch at
	// most one attempt per scheduler turn so a backend that is recovering does
	// not receive a burst of recursive callouts from one Redis callback turn.
	dispatchNextDetachedSettlement(tick)
}
