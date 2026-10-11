package claudepty

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentparley/tunnel/internal/cliround"
)

const (
	// pollHold is how long a long-poll waits before answering 204 so the mod asks again.
	pollHold = 25 * time.Second

	// maxEncodedBytes keeps one answer to the mod under the 4 MiB the mod's fetch accepts (verified: 3.99 MB goes
	// through, 4.3 MB is dropped).
	maxEncodedBytes = 3584 << 10

	toolPrefix = "mcp__agentparley__"
)

// toleratedTools are the engine tools allowed to exist next to AgentParley's; the mod's tool.check denies every call to
// them. EndConversation is switched on by a server flag for subscription logins and no flag or deny rule removes it.
var toleratedTools = map[string]bool{"ToolSearch": true, "EndConversation": true}

// command is one instruction the mod picks up from GET /next.
type command struct {
	Sequence  int64  `json:"sequence"`
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Model     string `json:"model,omitempty"`
	System    string `json:"system,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
	MaxTokens int    `json:"maxTokens,omitempty"`
	Effort    string `json:"effort,omitempty"`
}

// toolAnswer is what a waiting tool.call hook receives.
type toolAnswer struct {
	Text    string   `json:"text"`
	Context []string `json:"context,omitempty"`
	Deny    *string  `json:"deny,omitempty"`
}

type toolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// hello is what the mod gets at session start: Platform's system prompt, the note naming the prefixed tools, and the
// tools to register.
type hello struct {
	System   string     `json:"system"`
	ToolNote string     `json:"toolNote"`
	Tools    []toolSpec `json:"tools"`
}

type stepToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type modUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

type stepReport struct {
	TurnID     string        `json:"turnId"`
	Index      int           `json:"index"`
	Text       string        `json:"text"`
	Thinking   string        `json:"thinking"`
	ToolUses   []stepToolUse `json:"toolUses"`
	StopReason string        `json:"stopReason"`
	Usage      *modUsage     `json:"usage"`
}

type refusalReport struct {
	Explanation *string `json:"explanation"`
}

type turnReport struct {
	TurnID  string         `json:"turnId"`
	Answer  string         `json:"answer"`
	Reason  string         `json:"reason"`
	Refusal *refusalReport `json:"refusal"`
}

type modEvent struct {
	Kind   string `json:"kind"`
	Tool   string `json:"tool"`
	Detail string `json:"detail"`
	Error  string `json:"error"`
}

const (
	commandSeed         = "seed"
	commandSubmit       = "submit"
	commandCompletePart = "complete-part"
	commandComplete     = "complete"

	stepModeReal   = "real"
	stepModeMadeUp = "made-up"

	stopReasonEndTurn         = "end_turn"
	stopReasonToolUse         = "tool_use"
	stopReasonMaxTokens       = "max_tokens"
	stopReasonContextExceeded = "model_context_window_exceeded"

	eventBlockedTool = "blocked_tool"
	eventError       = "error"

	turnReasonAnswer  = "answer"
	turnReasonRefusal = "refusal"
	turnReasonAborted = "aborted"
)

// stepDecision answers POST /step-start: a real model request, or a response the mod fabricates.
type stepDecision struct {
	Mode       string      `json:"mode"`
	Text       string      `json:"text,omitempty"`
	ToolUses   []madeUpUse `json:"toolUses,omitempty"`
	ToolIDs    []string    `json:"toolIds,omitempty"`
	StopReason string      `json:"stopReason,omitempty"`
}

type madeUpUse struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// modSink receives what the mod reports. Every method runs on an HTTP handler goroutine and must not block.
type modSink interface {
	onStepStart(turnID string, index int) stepDecision
	onStepEnd(report stepReport)
	onTurnComplete(report turnReport)
	onEvent(event modEvent)
}

// bridge is the daemon's end of the private unix socket the mod talks to.
type bridge struct {
	path     string
	listener net.Listener
	server   *http.Server

	hello    hello
	sink     modSink
	commands chan command

	mutex       sync.Mutex
	sequence    int64
	unacked     *command
	ackWaiters  map[int64]chan struct{}
	results     map[string]*resultSlot
	completions map[string]chan json.RawMessage
	ready       chan struct{}
	failure     error
	readyOnce   sync.Once
	closed      bool
}

func newBridge(path string, hello hello, sink modSink) (*bridge, error) {
	if len(path) >= 100 {
		return nil, fmt.Errorf("the bridge socket path %q is too long for a unix socket", path)
	}
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, err
	}
	b := &bridge{
		path:        path,
		listener:    listener,
		hello:       hello,
		sink:        sink,
		commands:    make(chan command, 64),
		ackWaiters:  map[int64]chan struct{}{},
		results:     map[string]*resultSlot{},
		completions: map[string]chan json.RawMessage{},
		ready:       make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hello", b.handleHello)
	mux.HandleFunc("POST /tools-registered", b.handleToolsRegistered)
	mux.HandleFunc("GET /next", b.handleNext)
	mux.HandleFunc("POST /ack", b.handleAck)
	mux.HandleFunc("POST /complete-result", b.handleCompleteResult)
	mux.HandleFunc("POST /step-start", b.handleStepStart)
	mux.HandleFunc("POST /step-end", b.handleStepEnd)
	mux.HandleFunc("GET /result", b.handleResult)
	mux.HandleFunc("POST /turn-complete", b.handleTurnComplete)
	mux.HandleFunc("POST /event", b.handleEvent)
	b.server = &http.Server{Handler: mux}
	go func() { _ = b.server.Serve(listener) }()
	return b, nil
}

func (b *bridge) close() {
	b.mutex.Lock()
	b.closed = true
	b.mutex.Unlock()
	_ = b.server.Close()
	_ = os.Remove(b.path)
}

func (b *bridge) fail(err error) {
	b.mutex.Lock()
	if b.failure == nil {
		b.failure = err
	}
	b.mutex.Unlock()
	b.readyOnce.Do(func() { close(b.ready) })
}

func (b *bridge) failed() error {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.failure
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(body.Bytes())
}

func decode(request *http.Request, into any) error {
	return json.NewDecoder(request.Body).Decode(into)
}

func waitSeconds(request *http.Request) time.Duration {
	hold := pollHold
	if value, err := strconv.Atoi(request.URL.Query().Get("wait")); err == nil && value >= 0 && time.Duration(value)*time.Second < hold {
		hold = time.Duration(value) * time.Second
	}
	return hold
}

func foreignTools(names []string) []string {
	var foreign []string
	for _, name := range names {
		if !strings.HasPrefix(name, toolPrefix) && !toleratedTools[name] {
			foreign = append(foreign, name)
		}
	}
	return foreign
}

func (b *bridge) rejectForeign(writer http.ResponseWriter, names []string) bool {
	foreign := foreignTools(names)
	if len(foreign) == 0 {
		return false
	}
	err := &cliround.HarnessError{Message: "Claude Code exposes tools AgentParley did not grant: " + strings.Join(foreign, ", ")}
	b.fail(err)
	writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Message})
	return true
}

func (b *bridge) handleHello(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		EngineVersion string   `json:"engineVersion"`
		ToolsBefore   []string `json:"toolsBefore"`
	}
	if err := decode(request, &body); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if b.rejectForeign(writer, body.ToolsBefore) {
		return
	}
	writeJSON(writer, http.StatusOK, b.hello)
}

func (b *bridge) handleToolsRegistered(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Tools []string `json:"tools"`
	}
	if err := decode(request, &body); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if b.rejectForeign(writer, body.Tools) {
		return
	}
	b.readyOnce.Do(func() { close(b.ready) })
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (b *bridge) handleNext(writer http.ResponseWriter, request *http.Request) {
	if pending := b.unackedCommand(); pending != nil {
		writeJSON(writer, http.StatusOK, pending)
		return
	}
	timer := time.NewTimer(waitSeconds(request))
	defer timer.Stop()
	select {
	case next := <-b.commands:
		b.mutex.Lock()
		b.unacked = &next
		b.mutex.Unlock()
		writeJSON(writer, http.StatusOK, next)
	case <-timer.C:
		writer.WriteHeader(http.StatusNoContent)
	case <-request.Context().Done():
		writer.WriteHeader(http.StatusNoContent)
	}
}

// unackedCommand is the command the mod was given and has not acknowledged yet; it is served again until it is, so a
// response lost on the socket costs nothing and commands keep their order.
func (b *bridge) unackedCommand() *command {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.unacked
}

func (b *bridge) handleAck(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Sequence int64 `json:"sequence"`
	}
	if err := decode(request, &body); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	b.mutex.Lock()
	if b.unacked != nil && b.unacked.Sequence == body.Sequence {
		b.unacked = nil
	}
	if waiter := b.ackWaiters[body.Sequence]; waiter != nil {
		close(waiter)
		delete(b.ackWaiters, body.Sequence)
	}
	b.mutex.Unlock()
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (b *bridge) handleCompleteResult(writer http.ResponseWriter, request *http.Request) {
	var raw json.RawMessage
	if err := decode(request, &raw); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	var identity struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &identity)
	b.mutex.Lock()
	waiter := b.completions[identity.ID]
	b.mutex.Unlock()
	if waiter != nil {
		select {
		case waiter <- raw:
		default:
		}
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (b *bridge) handleStepStart(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		TurnID string `json:"turnId"`
		Index  int    `json:"index"`
	}
	if err := decode(request, &body); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(writer, http.StatusOK, b.sink.onStepStart(body.TurnID, body.Index))
}

func (b *bridge) handleStepEnd(writer http.ResponseWriter, request *http.Request) {
	var report stepReport
	if err := decode(request, &report); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	b.sink.onStepEnd(report)
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (b *bridge) handleTurnComplete(writer http.ResponseWriter, request *http.Request) {
	var report turnReport
	if err := decode(request, &report); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	b.sink.onTurnComplete(report)
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (b *bridge) handleEvent(writer http.ResponseWriter, request *http.Request) {
	var event modEvent
	if err := decode(request, &event); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	b.sink.onEvent(event)
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

// resultSlot holds one tool result. A poll that is answered keeps the result readable for resultKeep, so a response
// the mod never received can be fetched again by its next poll.
type resultSlot struct {
	ready  chan struct{}
	answer toolAnswer
	isSet  bool
	reaped bool
}

const resultKeep = time.Minute

func (b *bridge) handleResult(writer http.ResponseWriter, request *http.Request) {
	callID := request.URL.Query().Get("callId")
	slot := b.resultSlot(callID)
	timer := time.NewTimer(waitSeconds(request))
	defer timer.Stop()
	select {
	case <-slot.ready:
		b.mutex.Lock()
		answer := slot.answer
		firstRead := !slot.reaped
		slot.reaped = true
		b.mutex.Unlock()
		if firstRead {
			time.AfterFunc(resultKeep, func() {
				b.mutex.Lock()
				delete(b.results, callID)
				b.mutex.Unlock()
			})
		}
		writeJSON(writer, http.StatusOK, answer)
	case <-timer.C:
		writer.WriteHeader(http.StatusNoContent)
	case <-request.Context().Done():
		writer.WriteHeader(http.StatusNoContent)
	}
}

func (b *bridge) resultSlot(callID string) *resultSlot {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	slot := b.results[callID]
	if slot == nil {
		slot = &resultSlot{ready: make(chan struct{})}
		b.results[callID] = slot
	}
	return slot
}

// deliver hands the answer to the tool.call hook waiting on callID, now or when it next asks.
func (b *bridge) deliver(callID string, answer toolAnswer) {
	slot := b.resultSlot(callID)
	b.mutex.Lock()
	defer b.mutex.Unlock()
	if slot.isSet {
		return
	}
	slot.answer = answer
	slot.isSet = true
	close(slot.ready)
}

// send queues a command for the mod.
func (b *bridge) send(ctx context.Context, next command) error {
	_, _, err := b.enqueue(ctx, next, false)
	return err
}

// enqueue numbers the command and queues it. With wantAck it returns the channel closed when the mod acknowledges it.
func (b *bridge) enqueue(ctx context.Context, next command, wantAck bool) (int64, <-chan struct{}, error) {
	b.mutex.Lock()
	b.sequence++
	next.Sequence = b.sequence
	var acknowledged chan struct{}
	if wantAck {
		acknowledged = make(chan struct{})
		b.ackWaiters[next.Sequence] = acknowledged
	}
	b.mutex.Unlock()
	select {
	case b.commands <- next:
		return next.Sequence, acknowledged, nil
	case <-ctx.Done():
		b.forgetAck(next.Sequence)
		return 0, nil, context.Cause(ctx)
	}
}

func (b *bridge) forgetAck(sequence int64) {
	b.mutex.Lock()
	delete(b.ackWaiters, sequence)
	b.mutex.Unlock()
}

// sendAcknowledged queues a command and waits for the mod's /ack.
func (b *bridge) sendAcknowledged(ctx context.Context, next command, exited <-chan struct{}) error {
	sequence, acknowledged, err := b.enqueue(ctx, next, true)
	if err != nil {
		return err
	}
	defer b.forgetAck(sequence)
	select {
	case <-acknowledged:
		return nil
	case <-exited:
		return errors.New("Claude Code exited")
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// expectCompletion reserves the channel a /complete-result for id will arrive on.
func (b *bridge) expectCompletion(id string) chan json.RawMessage {
	waiter := make(chan json.RawMessage, 1)
	b.mutex.Lock()
	b.completions[id] = waiter
	b.mutex.Unlock()
	return waiter
}

func (b *bridge) forgetCompletion(id string) {
	b.mutex.Lock()
	delete(b.completions, id)
	b.mutex.Unlock()
}
