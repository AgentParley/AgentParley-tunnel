package cliround

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

type State int

const (
	StateStarting State = iota
	StateRunning
	StateWaitingOnPlatform
	StateIdle
	StateDirty
)

// Runner is one warm CLI session. State, Frame and Consumed may be called from any goroutine; Extend, Close and
// TakeCarried are only called by the manager, never concurrently with each other except Close interrupting Extend.
type Runner interface {
	Frame() Frame
	// Consumed returns the runner's live record of what it has taken in; the manager updates it after each round.
	Consumed() *Consumed
	State() State
	Extend(ctx context.Context, plan Plan, request *Request) (*Result, error)
	// TakeCarried returns the usage and events gathered between rounds and clears them.
	TakeCarried() (Usage, []Event)
	// Close interrupts the CLI and returns the usage that interrupt flushed.
	Close() (Usage, error)
}

// Budgets are the caps for one round: Rebuild for bringing a session up to date, Step for the model step itself.
// Both are already clamped to what is left of the operation's deadline.
type Budgets struct {
	Rebuild, Step time.Duration
}

// Factory starts a session, brings it up to date with the request and runs the round. On error it cleans up after
// itself and returns no runner. It reports how long the rebuild part took in Result.RebuildMs; when it leaves that
// zero, the manager records the factory's whole run time.
type Factory func(ctx context.Context, model string, request *Request, budgets Budgets) (Runner, *Result, error)

// OneShot runs a round that has no conversation: no session is kept and no fence applies.
type OneShot func(ctx context.Context, model string, request *Request) (*Result, error)

// Deliver hands a finished round's result on towards Platform. The manager keeps the usage it carried into the result
// until Deliver succeeds.
type Deliver func(result *Result) error

type Limits struct {
	MaxWarm    int
	MaxOneShot int
	IdleLimit  time.Duration
}

const (
	defaultMaxWarm     = 8
	defaultMaxOneShot  = 4
	defaultIdleLimit   = 30 * time.Minute
	maxRebuildBudget   = 180 * time.Second
	maxStepBudget      = 600 * time.Second
	fallbackRemaining  = maxRebuildBudget + maxStepBudget
	slotWait           = 30 * time.Second
	tombstoneTTL       = 24 * time.Hour
	errShuttingDownMsg = "the daemon is shutting down"
)

var errShuttingDown = &HarnessError{Message: errShuttingDownMsg}

type entry struct {
	conversation string
	table        *table
	lock         chan struct{}

	runner        Runner
	lastUsed      time.Time
	highestFence  int64
	cancelRound   context.CancelCauseFunc
	carriedUsage  Usage
	carriedEvents []Event
	dirty         bool
	removed       bool
}

type table struct {
	entries   map[string]*entry
	live      int
	reserved  int
	slotFreed chan struct{}
}

type Manager struct {
	logger *log.Logger
	limits map[string]Limits

	mutex    sync.Mutex
	tables   map[string]*table
	oneShots map[string]chan struct{}
	closed   bool
}

func NewManager(limits map[string]Limits, logger *log.Logger) *Manager {
	return &Manager{logger: logger, limits: limits, tables: map[string]*table{}, oneShots: map[string]chan struct{}{}}
}

func (m *Manager) limitsFor(harness string) Limits {
	limits := m.limits[harness]
	if limits.MaxWarm <= 0 {
		limits.MaxWarm = defaultMaxWarm
	}
	if limits.MaxOneShot <= 0 {
		limits.MaxOneShot = defaultMaxOneShot
	}
	if limits.IdleLimit <= 0 {
		limits.IdleLimit = defaultIdleLimit
	}
	return limits
}

func (e *entry) acquire(ctx context.Context) error {
	select {
	case e.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *entry) tryAcquire() bool {
	select {
	case e.lock <- struct{}{}:
		return true
	default:
		return false
	}
}

func (e *entry) release() {
	<-e.lock
}

// Round runs one round. A conversation round is fenced: a lower fence than one already seen is superseded at once,
// and a fence at least as high aborts a round still running for that conversation. A round the manager cannot admit
// (every warm slot busy, or every one-shot slot) ends with ErrBusy after a short wait: the manager is the only place
// that answers it.
func (m *Manager) Round(ctx context.Context, harness, model string, request *Request, factory Factory, oneShot OneShot, deliver Deliver) error {
	if request.Conversation == "" {
		return m.runOneShot(ctx, harness, model, request, oneShot, deliver)
	}
	if request.Fence == nil {
		return &InvalidRequestError{Message: "conversation and fence must be given together"}
	}
	fence := *request.Fence

	var current *entry
	for {
		candidate, err := m.enter(harness, request.Conversation, fence)
		if err != nil {
			return err
		}
		if err := candidate.acquire(ctx); err != nil {
			return err
		}
		m.mutex.Lock()
		removed := candidate.removed
		superseded := fence < candidate.highestFence
		m.mutex.Unlock()
		if superseded {
			candidate.release()
			return ErrSuperseded
		}
		if removed {
			candidate.release()
			continue
		}
		current = candidate
		break
	}
	defer current.release()

	return m.runRound(ctx, harness, model, request, factory, deliver, current)
}

func (m *Manager) runOneShot(ctx context.Context, harness, model string, request *Request, oneShot OneShot, deliver Deliver) error {
	release, err := m.admitOneShot(ctx, harness)
	if err != nil {
		return err
	}
	defer release()
	result, err := oneShot(ctx, model, request)
	if err != nil {
		return err
	}
	return deliver(result)
}

func (m *Manager) admitOneShot(ctx context.Context, harness string) (func(), error) {
	m.mutex.Lock()
	slots := m.oneShots[harness]
	if slots == nil {
		slots = make(chan struct{}, m.limitsFor(harness).MaxOneShot)
		m.oneShots[harness] = slots
	}
	m.mutex.Unlock()

	timer := time.NewTimer(slotWait)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-timer.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

func (m *Manager) enter(harness, conversation string, fence int64) (*entry, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if m.closed {
		return nil, errShuttingDown
	}
	conversations := m.tables[harness]
	if conversations == nil {
		conversations = &table{entries: map[string]*entry{}, slotFreed: make(chan struct{})}
		m.tables[harness] = conversations
	}
	existing := conversations.entries[conversation]
	if existing == nil {
		existing = &entry{conversation: conversation, table: conversations, lock: make(chan struct{}, 1), lastUsed: time.Now()}
		conversations.entries[conversation] = existing
	}
	if fence < existing.highestFence {
		return nil, ErrSuperseded
	}
	existing.highestFence = fence
	if existing.cancelRound != nil {
		existing.cancelRound(ErrSuperseded)
	}
	return existing, nil
}

// roundState is what one round knows about the conversation's runner while it plans, rebuilds and retries.
// holdsSlot means the round freed its own runner and keeps that warm slot reserved for the replacement.
type roundState struct {
	runner            Runner
	plan              Plan
	reason            RebuildReason
	holdsSlot         bool
	contextFullEvents []Event
}

func (m *Manager) runRound(ctx context.Context, harness, model string, request *Request, factory Factory, deliver Deliver, current *entry) error {
	roundCtx, endRound := m.beginRound(ctx, current)
	defer endRound()

	state := m.planRound(current, FrameOf(model, request), request)
	defer m.releaseHeldSlot(current, &state)

	result, err := m.runSteps(roundCtx, harness, model, request, factory, current, &state)
	if err != nil {
		return m.failRound(roundCtx, current, &state, err)
	}
	if result == nil || state.runner == nil {
		return &HarnessError{Message: "the CLI produced no result"}
	}

	result.RebuildReason = state.reason
	if state.reason == RebuildNone {
		result.RebuildMs = 0
	}
	m.finishRound(current, state.runner, request, result, state.contextFullEvents)
	if err := deliver(result); err != nil {
		m.markDirty(current)
		return err
	}
	m.settleCarry(current, result)
	m.logger.Printf("cli round: harness=%s conversation=%s outcome=%s rebuild=%s rebuildMs=%d", harness, request.Conversation, result.Outcome, result.RebuildReason, result.RebuildMs)
	return nil
}

func (m *Manager) beginRound(ctx context.Context, current *entry) (context.Context, func()) {
	roundCtx, cancel := context.WithCancelCause(ctx)
	m.mutex.Lock()
	current.cancelRound = cancel
	m.mutex.Unlock()
	return roundCtx, func() {
		m.mutex.Lock()
		current.cancelRound = nil
		current.lastUsed = time.Now()
		m.mutex.Unlock()
		cancel(nil)
	}
}

// planRound keeps the warm runner when the request continues it, and otherwise swaps it out and says why.
func (m *Manager) planRound(current *entry, frame Frame, request *Request) roundState {
	m.mutex.Lock()
	runner := current.runner
	isDirty := current.dirty
	m.mutex.Unlock()
	if runner == nil {
		return roundState{reason: RebuildNoSession}
	}

	usage, events := runner.TakeCarried()
	m.addCarry(current, usage, events)
	if isDirty || runner.State() == StateDirty {
		return roundState{reason: RebuildDirty, holdsSlot: m.replaceRunner(current)}
	}
	sessionFrame := runner.Frame()
	plan := Reconcile(frame, &sessionFrame, runner.Consumed(), request.Items, request.ReplyDirectives)
	if plan.Reason != RebuildNone {
		return roundState{reason: plan.Reason, holdsSlot: m.replaceRunner(current)}
	}
	return roundState{runner: runner, plan: plan, reason: RebuildNone}
}

// runSteps extends the warm runner or rebuilds one, and rebuilds once more when the CLI's context fills up.
func (m *Manager) runSteps(roundCtx context.Context, harness, model string, request *Request, factory Factory, current *entry, state *roundState) (*Result, error) {
	limits := m.limitsFor(harness)
	contextFullRetried := false
	for {
		budgets, err := budgetsFor(roundCtx)
		if err != nil {
			return nil, err
		}
		var result *Result
		if state.runner != nil {
			result, err = m.extend(roundCtx, state.runner, state.plan, request, budgets.Step)
		} else {
			state.runner, result, err = m.rebuild(roundCtx, limits, current, model, request, factory, budgets, state.holdsSlot)
			state.holdsSlot = false
		}
		if errors.Is(err, ErrContextFull) && !contextFullRetried {
			contextFullRetried = true
			state.holdsSlot = m.replaceRunner(current)
			state.runner = nil
			state.reason = RebuildContextFull
			state.contextFullEvents = append(state.contextFullEvents, Event{Kind: EventContextFull, Detail: "the CLI's context filled up, so the session was rebuilt"})
			continue
		}
		return result, err
	}
}

func (m *Manager) failRound(roundCtx context.Context, current *entry, state *roundState, err error) error {
	if state.runner != nil {
		m.markDirty(current)
	}
	if errors.Is(context.Cause(roundCtx), ErrSuperseded) {
		return ErrSuperseded
	}
	return err
}

func (m *Manager) markDirty(current *entry) {
	m.mutex.Lock()
	current.dirty = true
	m.mutex.Unlock()
}

func (m *Manager) releaseHeldSlot(current *entry, state *roundState) {
	if !state.holdsSlot {
		return
	}
	state.holdsSlot = false
	m.mutex.Lock()
	current.table.reserved--
	m.freeSlotLocked(current.table)
	m.mutex.Unlock()
}

func (m *Manager) finishRound(current *entry, runner Runner, request *Request, result *Result, extraEvents []Event) {
	consumed := runner.Consumed()
	consumed.Keys = make([]string, 0, len(request.Items))
	for _, item := range request.Items {
		consumed.Keys = append(consumed.Keys, item.Key)
	}
	consumed.Pending = nil
	switch result.Outcome {
	case OutcomeToolCalls:
		callIDs := make([]string, 0, len(result.Calls))
		for _, call := range result.Calls {
			callIDs = append(callIDs, call.ID)
		}
		consumed.Pending = &Output{CallIDs: callIDs}
	case OutcomeAnswer:
		consumed.Pending = &Output{IsAnswer: true, AnswerText: result.Text}
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()
	current.dirty = result.Outcome != OutcomeToolCalls && result.Outcome != OutcomeAnswer
	result.Usage.Add(current.carriedUsage)
	merged := append(append([]Event{}, current.carriedEvents...), extraEvents...)
	result.Events = append(merged, result.Events...)
}

// settleCarry forgets the usage and events the delivered result carried. A refused result is thrown away by Platform,
// so its carry stays for the conversation's next round.
func (m *Manager) settleCarry(current *entry, result *Result) {
	if result.Outcome == OutcomeRefused {
		return
	}
	m.mutex.Lock()
	defer m.mutex.Unlock()
	current.carriedUsage = Usage{}
	current.carriedEvents = nil
}

func (m *Manager) extend(ctx context.Context, runner Runner, plan Plan, request *Request, step time.Duration) (*Result, error) {
	stepCtx, cancel := context.WithTimeout(ctx, step)
	defer cancel()
	result, err := runner.Extend(stepCtx, plan, request)
	if err != nil && stepCtx.Err() != nil && ctx.Err() == nil {
		return nil, fmt.Errorf("the CLI did not finish the step within %s: %w", step, context.DeadlineExceeded)
	}
	return result, err
}

func (m *Manager) rebuild(ctx context.Context, limits Limits, current *entry, model string, request *Request, factory Factory, budgets Budgets, holdsSlot bool) (Runner, *Result, error) {
	if !holdsSlot {
		if err := m.reserveSlot(ctx, current.table, limits, current); err != nil {
			return nil, nil, err
		}
	}
	started := time.Now()
	runner, result, err := factory(ctx, model, request, budgets)

	m.mutex.Lock()
	current.table.reserved--
	if err == nil && runner != nil {
		current.runner = runner
		current.table.live++
	} else {
		m.freeSlotLocked(current.table)
	}
	m.mutex.Unlock()

	if err != nil {
		if runner != nil {
			_, _ = runner.Close()
		}
		return nil, nil, err
	}
	if result != nil && result.RebuildMs == 0 {
		result.RebuildMs = time.Since(started).Milliseconds()
	}
	return runner, result, nil
}

func budgetsFor(ctx context.Context) (Budgets, error) {
	remaining := fallbackRemaining
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	if remaining <= 0 {
		return Budgets{}, context.DeadlineExceeded
	}
	return Budgets{Rebuild: min(maxRebuildBudget, remaining), Step: min(maxStepBudget, remaining)}, nil
}

func (m *Manager) addCarry(current *entry, usage Usage, events []Event) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	current.carriedUsage.Add(usage)
	current.carriedEvents = append(current.carriedEvents, events...)
}

// dropRunner closes the entry's runner, frees its warm slot and keeps whatever usage the close flushed for the
// conversation's next round. The caller holds the entry's lock.
func (m *Manager) dropRunner(current *entry) {
	m.detachRunner(current, false)
}

// replaceRunner is dropRunner for a round that is about to rebuild: the warm slot stays reserved for the rebuild, so a
// chat waiting for a slot cannot take it in between. It reports whether a slot is now held.
func (m *Manager) replaceRunner(current *entry) bool {
	return m.detachRunner(current, true)
}

func (m *Manager) detachRunner(current *entry, keepSlot bool) bool {
	m.mutex.Lock()
	runner := current.runner
	current.runner = nil
	current.dirty = false
	if runner != nil {
		current.table.live--
		if keepSlot {
			current.table.reserved++
		} else {
			m.freeSlotLocked(current.table)
		}
	}
	m.mutex.Unlock()
	if runner == nil {
		return false
	}
	usage, events := runner.TakeCarried()
	flushed, err := runner.Close()
	if err != nil {
		m.logger.Printf("cli round: closing the session for %s: %v", current.conversation, err)
	}
	usage.Add(flushed)
	m.addCarry(current, usage, events)
	return keepSlot
}

func (m *Manager) freeSlotLocked(conversations *table) {
	close(conversations.slotFreed)
	conversations.slotFreed = make(chan struct{})
}

// reserveSlot makes room for one more warm session: evict the least recently used Idle (or Dirty) one, else the
// least recently used one waiting on Platform, never one that is starting or running; with none to evict, wait up
// to 30 s for a slot and then give up with ErrBusy.
func (m *Manager) reserveSlot(ctx context.Context, conversations *table, limits Limits, own *entry) error {
	timer := time.NewTimer(slotWait)
	defer timer.Stop()
	for {
		m.mutex.Lock()
		if conversations.live+conversations.reserved < limits.MaxWarm {
			conversations.reserved++
			m.mutex.Unlock()
			return nil
		}
		victim := pickVictimLocked(conversations, own)
		slotFreed := conversations.slotFreed
		m.mutex.Unlock()

		if victim != nil {
			m.dropRunner(victim)
			victim.release()
			continue
		}
		select {
		case <-slotFreed:
		case <-timer.C:
			return ErrBusy
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// pickVictimLocked returns an evictable entry with its lock already taken, or nil.
func pickVictimLocked(conversations *table, own *entry) *entry {
	var evictable, waiting []*entry
	for _, candidate := range conversations.entries {
		if candidate == own || candidate.runner == nil {
			continue
		}
		switch candidate.runner.State() {
		case StateIdle, StateDirty:
			evictable = append(evictable, candidate)
		case StateWaitingOnPlatform:
			waiting = append(waiting, candidate)
		}
	}
	for _, group := range [][]*entry{evictable, waiting} {
		sort.Slice(group, func(i, j int) bool { return group[i].lastUsed.Before(group[j].lastUsed) })
		for _, candidate := range group {
			if candidate.tryAcquire() {
				return candidate
			}
		}
	}
	return nil
}

// Sweep closes sessions idle past their harness's limit and forgets conversations whose runner has been gone, with
// no round, for 24 hours (their carried usage and their highest fence go with them).
func (m *Manager) Sweep(now time.Time) {
	m.mutex.Lock()
	type sweepTarget struct {
		harness string
		entry   *entry
	}
	var targets []sweepTarget
	for harness, conversations := range m.tables {
		for _, candidate := range conversations.entries {
			targets = append(targets, sweepTarget{harness: harness, entry: candidate})
		}
	}
	m.mutex.Unlock()

	for _, target := range targets {
		if !target.entry.tryAcquire() {
			continue
		}
		limits := m.limitsFor(target.harness)
		m.mutex.Lock()
		hasRunner := target.entry.runner != nil
		idleFor := now.Sub(target.entry.lastUsed)
		m.mutex.Unlock()

		if hasRunner && idleFor > limits.IdleLimit {
			m.dropRunner(target.entry)
			m.mutex.Lock()
			target.entry.lastUsed = now
			m.mutex.Unlock()
		} else if !hasRunner && idleFor > tombstoneTTL {
			m.mutex.Lock()
			target.entry.removed = true
			delete(target.entry.table.entries, target.entry.conversation)
			m.mutex.Unlock()
		}
		target.entry.release()
	}
}

// Close ends every warm session and aborts the rounds still running; their child process groups are killed.
func (m *Manager) Close() {
	m.mutex.Lock()
	m.closed = true
	var runners []Runner
	for _, conversations := range m.tables {
		for _, candidate := range conversations.entries {
			if candidate.cancelRound != nil {
				candidate.cancelRound(errShuttingDown)
			}
			if candidate.runner != nil {
				runners = append(runners, candidate.runner)
				candidate.runner = nil
			}
		}
		conversations.live = 0
	}
	m.mutex.Unlock()

	for _, runner := range runners {
		_, _ = runner.Close()
	}
}
