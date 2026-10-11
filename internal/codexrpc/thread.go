package codexrpc

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/agentparley/tunnel/internal/cliauth"
	"github.com/agentparley/tunnel/internal/cliround"
)

const (
	// LegacyModelID is the registration id that means "whatever model the signed-in account defaults to": thread/start
	// then names no model.
	LegacyModelID = "codex"

	// batchQuietWindow closes a batch of tool calls. Codex 0.150.1 has no end-of-batch signal (verified), so a batch
	// is the calls that arrive with no gap longer than this.
	batchQuietWindow = 300 * time.Millisecond

	closeInterruptWait = 5 * time.Second
	requestTimeout     = 30 * time.Second
	injectBatchBytes   = 4 << 20
)

type waitingCall struct {
	rpcID     json.RawMessage
	callID    string
	tool      string
	arguments json.RawMessage
	reported  bool
}

type roundEnd struct {
	outcome string
	err     error
}

type round struct {
	texts    []string
	thinking []string
	events   []cliround.Event
	usage    cliround.Usage
	timer    *time.Timer
	done     chan roundEnd
	finished bool
}

// Thread is one warm Codex conversation: an app-server thread whose model-visible history is what AgentParley has fed
// it. It implements cliround.Runner.
type Thread struct {
	id        string
	server    *server
	client    *client
	logger    *log.Logger
	frame     cliround.Frame
	consumed  cliround.Consumed
	toolNames map[string]bool

	mutex          sync.Mutex
	state          cliround.State
	round          *round
	turnID         string
	waiting        []*waitingCall
	carriedUsage   cliround.Usage
	carriedEvents  []cliround.Event
	closed         bool
	offRoundLogged bool
}

func (d *Driver) startThread(ctx context.Context, running *server, model string, request *cliround.Request, withTools bool) (*Thread, error) {
	params := map[string]any{
		"cwd":                   cliauth.CodexWorkDir(),
		"approvalPolicy":        "never",
		"sandbox":               "read-only",
		"developerInstructions": "",
		"ephemeral":             true,
		"environments":          []any{},
	}
	if model != "" && model != LegacyModelID {
		params["model"] = model
	}
	if request.System != nil {
		params["baseInstructions"] = *request.System
	}
	toolNames := map[string]bool{}
	if withTools && len(request.Tools) > 0 {
		specs, err := toolSpecs(request.Tools)
		if err != nil {
			return nil, err
		}
		params["dynamicTools"] = specs
		for _, tool := range request.Tools {
			toolNames[tool.Name] = true
		}
	}
	if request.Effort != "" {
		params["config"] = map[string]any{"model_reasoning_effort": request.Effort, "model_reasoning_summary": "detailed"}
	}

	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := running.client.call(callCtx, "thread/start", params, &started); err != nil {
		return nil, classifyCallError(err)
	}
	if started.Thread.ID == "" {
		return nil, &cliround.HarnessError{Message: "codex started a thread without an id"}
	}

	thread := &Thread{
		id:        started.Thread.ID,
		server:    running,
		client:    running.client,
		logger:    d.logger,
		frame:     cliround.FrameOf(model, request),
		toolNames: toolNames,
		state:     cliround.StateStarting,
	}
	running.client.register(thread.id, thread)
	return thread, nil
}

func classifyCallError(err error) error {
	var rpc *rpcError
	if errors.As(err, &rpc) {
		return &cliround.HarnessError{Message: rpc.Message}
	}
	return err
}

func (t *Thread) Frame() cliround.Frame {
	return t.frame
}

func (t *Thread) Consumed() *cliround.Consumed {
	return &t.consumed
}

func (t *Thread) State() cliround.State {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	return t.state
}

func (t *Thread) TakeCarried() (cliround.Usage, []cliround.Event) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	usage, events := t.carriedUsage, t.carriedEvents
	t.carriedUsage, t.carriedEvents = cliround.Usage{}, nil
	return usage, events
}

// Extend continues the open turn with Platform's tool results (and any new user input), or opens the next turn.
func (t *Thread) Extend(ctx context.Context, plan cliround.Plan, request *cliround.Request) (*cliround.Result, error) {
	return t.runRound(ctx, func(current *round) error {
		return t.continueTurn(ctx, current, plan)
	})
}

func (t *Thread) continueTurn(ctx context.Context, current *round, plan cliround.Plan) error {
	t.mutex.Lock()
	hasWaiting := len(t.waiting) > 0
	turnID := t.turnID
	t.mutex.Unlock()

	if !hasWaiting {
		if len(plan.NewInput) == 0 {
			return &cliround.HarnessError{Message: "the round has nothing new for Codex to answer"}
		}
		return t.startTurn(ctx, plan.NewInput)
	}

	if len(plan.NewInput) > 0 {
		if turnID == "" {
			return &cliround.HarnessError{Message: "Codex has tool calls waiting but no open turn"}
		}
		callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		err := t.client.call(callCtx, "turn/steer", map[string]any{
			"threadId": t.id, "input": userInput(plan.NewInput), "expectedTurnId": turnID,
		}, nil)
		if err != nil {
			return classifyCallError(err)
		}
	}

	for _, result := range plan.Results {
		waiting := t.takeWaiting(result.CallID)
		if waiting == nil {
			return &cliround.HarnessError{Message: "a tool result has no waiting Codex call"}
		}
		t.client.respond(waiting.rpcID, map[string]any{
			"contentItems": []map[string]any{{"type": "inputText", "text": capToolOutput(result.Text)}},
			"success":      true,
		})
	}

	t.mutex.Lock()
	defer t.mutex.Unlock()
	if hasUnreportedLocked(t.waiting) {
		t.armTimerLocked(current)
	}
	return nil
}

func (t *Thread) startTurn(ctx context.Context, input []cliround.Item) error {
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	params := map[string]any{"threadId": t.id, "input": []any{}}
	if len(input) > 0 {
		params["input"] = userInput(input)
	}
	if err := t.client.call(callCtx, "turn/start", params, nil); err != nil {
		return classifyCallError(err)
	}
	return nil
}

func (t *Thread) takeWaiting(callID string) *waitingCall {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	for index, candidate := range t.waiting {
		if candidate.callID == callID {
			t.waiting = append(t.waiting[:index], t.waiting[index+1:]...)
			return candidate
		}
	}
	return nil
}

func hasUnreportedLocked(waiting []*waitingCall) bool {
	for _, candidate := range waiting {
		if !candidate.reported {
			return true
		}
	}
	return false
}

// runRound opens a round, lets kick put Codex to work, and waits for the round to end: a quiet batch of tool calls,
// or the turn finishing.
func (t *Thread) runRound(ctx context.Context, kick func(current *round) error) (*cliround.Result, error) {
	current := &round{done: make(chan roundEnd, 1)}
	t.mutex.Lock()
	if t.closed {
		t.mutex.Unlock()
		return nil, &cliround.HarnessError{Message: "the Codex session was closed"}
	}
	t.round = current
	t.state = cliround.StateRunning
	t.mutex.Unlock()

	if err := kick(current); err != nil {
		t.abortRound(current)
		return nil, err
	}

	select {
	case end := <-current.done:
		return t.completeRound(current, end)
	case <-ctx.Done():
		t.abortRound(current)
		return nil, ctx.Err()
	}
}

func (t *Thread) abortRound(current *round) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.endRoundWithErrorLocked(current)
}

func (t *Thread) endRoundWithErrorLocked(current *round) {
	if current.timer != nil {
		current.timer.Stop()
	}
	current.finished = true
	t.carriedUsage.Add(current.usage)
	t.carriedEvents = append(t.carriedEvents, current.events...)
	if t.round == current {
		t.round = nil
	}
	t.state = cliround.StateDirty
}

func (t *Thread) completeRound(current *round, end roundEnd) (*cliround.Result, error) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if end.err != nil {
		t.endRoundWithErrorLocked(current)
		return nil, end.err
	}

	result := &cliround.Result{
		Outcome: end.outcome,
		Text:    strings.Join(current.texts, "\n\n"),
		Calls:   []cliround.Call{},
		Usage:   current.usage,
		Events:  current.events,
	}
	if len(current.thinking) > 0 {
		thinking := strings.Join(current.thinking, "\n\n")
		result.Thinking = &thinking
	}
	if result.Events == nil {
		result.Events = []cliround.Event{}
	}
	t.round = nil
	switch end.outcome {
	case cliround.OutcomeToolCalls:
		for _, candidate := range t.waiting {
			if candidate.reported {
				continue
			}
			candidate.reported = true
			arguments := candidate.arguments
			if len(arguments) == 0 || string(arguments) == "null" {
				arguments = json.RawMessage("{}")
			}
			result.Calls = append(result.Calls, cliround.Call{ID: candidate.callID, Name: candidate.tool, Arguments: arguments})
		}
		t.state = cliround.StateWaitingOnPlatform
	case cliround.OutcomeAnswer:
		t.state = cliround.StateIdle
	default:
		t.state = cliround.StateDirty
	}
	return result, nil
}

// finishLocked ends the open round once.
func (t *Thread) finishLocked(current *round, outcome string, err error) {
	if current.finished {
		return
	}
	current.finished = true
	if current.timer != nil {
		current.timer.Stop()
	}
	current.done <- roundEnd{outcome: outcome, err: err}
}

func (t *Thread) armTimerLocked(current *round) {
	if current.timer != nil {
		current.timer.Stop()
	}
	current.timer = time.AfterFunc(batchQuietWindow, func() {
		t.mutex.Lock()
		defer t.mutex.Unlock()
		if t.round == current && hasUnreportedLocked(t.waiting) {
			t.finishLocked(current, cliround.OutcomeToolCalls, nil)
		}
	})
}

// Close interrupts the open turn, waits briefly for Codex to report the usage that interrupt flushes, and drops the
// thread.
func (t *Thread) Close() (cliround.Usage, error) {
	t.mutex.Lock()
	if t.closed {
		t.mutex.Unlock()
		return cliround.Usage{}, nil
	}
	t.closed = true
	turnID := t.turnID
	if t.round != nil {
		t.finishLocked(t.round, "", &cliround.HarnessError{Message: "the Codex session was closed"})
	}
	t.mutex.Unlock()

	var closeErr error
	if t.server.alive() {
		if turnID != "" {
			interruptCtx, cancel := context.WithTimeout(context.Background(), closeInterruptWait)
			err := t.client.call(interruptCtx, "turn/interrupt", map[string]any{"threadId": t.id, "turnId": turnID}, nil)
			cancel()
			if err != nil {
				closeErr = err
			} else {
				t.waitForTurnEnd(closeInterruptWait)
			}
		}
		unsubscribeCtx, cancel := context.WithTimeout(context.Background(), closeInterruptWait)
		if err := t.client.call(unsubscribeCtx, "thread/unsubscribe", map[string]any{"threadId": t.id}, nil); err != nil && closeErr == nil {
			closeErr = err
		}
		cancel()
	}
	t.client.unregister(t.id)

	t.mutex.Lock()
	defer t.mutex.Unlock()
	usage := t.carriedUsage
	t.carriedUsage = cliround.Usage{}
	return usage, closeErr
}

func (t *Thread) waitForTurnEnd(limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		t.mutex.Lock()
		ended := t.turnID == ""
		t.mutex.Unlock()
		if ended {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (t *Thread) onServerExit(err error) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.state = cliround.StateDirty
	t.turnID = ""
	if t.round != nil {
		t.finishLocked(t.round, "", &cliround.HarnessError{Message: "codex app-server exited"})
	}
}

// injectHistory appends history rows to the thread's model-visible history.
func (t *Thread) injectHistory(ctx context.Context, history []cliround.Item) error {
	items := historyItems(history)
	var batch []map[string]any
	batchBytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		err := t.client.call(callCtx, "thread/inject_items", map[string]any{"threadId": t.id, "items": batch}, nil)
		batch, batchBytes = nil, 0
		if err != nil {
			return classifyCallError(err)
		}
		return nil
	}
	for _, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if batchBytes > 0 && batchBytes+len(encoded) > injectBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, item)
		batchBytes += len(encoded)
	}
	return flush()
}

// Factory starts a fresh thread, feeds it the request's history and runs the round.
func (d *Driver) Factory(ctx context.Context, model string, request *cliround.Request, budgets cliround.Budgets) (cliround.Runner, *cliround.Result, error) {
	history, trailing := splitTrailingUser(request.Items)
	if len(trailing) == 0 && (len(history) == 0 || history[len(history)-1].Role != cliround.RoleTool) {
		return nil, nil, &cliround.InvalidRequestError{Message: "the round has nothing new for Codex to answer"}
	}
	running, err := d.serverFor(ctx)
	if err != nil {
		return nil, nil, err
	}
	thread, err := d.startThread(ctx, running, model, request, true)
	if err != nil {
		return nil, nil, err
	}

	rebuildStarted := time.Now()
	rebuildCtx, cancelRebuild := context.WithTimeout(ctx, budgets.Rebuild)
	err = thread.injectHistory(rebuildCtx, history)
	cancelRebuild()
	if err != nil {
		_, _ = thread.Close()
		return nil, nil, err
	}
	rebuildMs := max(1, time.Since(rebuildStarted).Milliseconds())

	stepCtx, cancelStep := context.WithTimeout(ctx, budgets.Step)
	defer cancelStep()
	result, err := thread.runRound(stepCtx, func(*round) error {
		return thread.startTurn(stepCtx, trailing)
	})
	if err != nil {
		_, _ = thread.Close()
		return nil, nil, err
	}
	result.RebuildMs = rebuildMs
	return thread, result, nil
}

// OneShot runs a request that belongs to no conversation (a title, a compaction, a routing decision) on a throwaway
// thread without tools.
func (d *Driver) OneShot(ctx context.Context, model string, request *cliround.Request) (*cliround.Result, error) {
	history, trailing := splitTrailingUser(request.Items)
	if len(trailing) == 0 {
		return nil, &cliround.InvalidRequestError{Message: "a one-shot request must end with a user message"}
	}
	if hasToolStructure(history) {
		history = []cliround.Item{{Role: cliround.RoleUser, Text: cliround.SeedHeader + cliround.RenderTranscript(history)}}
	}
	running, err := d.serverFor(ctx)
	if err != nil {
		return nil, err
	}
	thread, err := d.startThread(ctx, running, model, request, false)
	if err != nil {
		return nil, err
	}
	if err := thread.injectHistory(ctx, history); err != nil {
		_, _ = thread.Close()
		return nil, err
	}
	result, err := thread.runRound(ctx, func(*round) error {
		return thread.startTurn(ctx, trailing)
	})
	flushed, _ := thread.Close()
	if err != nil {
		return nil, err
	}
	result.Usage.Add(flushed)
	return result, nil
}
