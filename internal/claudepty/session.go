package claudepty

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agentparley/tunnel/internal/cliround"
)

const (
	maxToolNameLength = 64

	// maxToolResultUnits is the most a tool result may hold, in UTF-16 code units (a JavaScript string length), when it
	// reaches Claude Code. Past 50,000 the engine swaps the result for a 2 KB preview and the path of a file the model
	// has no tool to read (measured on 2.1.296: 30,000 passes whole, 60,000 is replaced), so AgentParley cuts it first,
	// with a visible marker, and the model keeps as much as fits.
	maxToolResultUnits = 49000

	// maxContextUnits is the same limit for a piece of context riding on a result (Claude Code keeps the head of a
	// piece past 100,000 characters and drops the rest).
	maxContextUnits = 90000

	missingResultText = "No result was recorded for this call."

	toolNote = "Your tools are named mcp__agentparley__<name>. Where these instructions name a tool, call it by that prefixed name."
)

var (
	toolNameCharacters = regexp.MustCompile(`[^A-Za-z0-9_-]`)
	callIDPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// toolNames maps AgentParley's tool names to the names registered in Claude Code and back. A name Claude Code cannot
// take as it is (odd characters, too long once prefixed) is registered under a short alias.
type toolNames struct {
	registered map[string]string
	original   map[string]string
}

func registeredName(name string) string {
	clean := toolNameCharacters.ReplaceAllString(name, "_")
	if clean == name && len(toolPrefix)+len(clean) <= maxToolNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "_" + hex.EncodeToString(sum[:])[:6]
	room := maxToolNameLength - len(toolPrefix) - len(suffix)
	if len(clean) > room {
		clean = clean[:room]
	}
	return clean + suffix
}

// newToolNames covers the current tools and every tool the history names; a historical tool that is no longer in the
// set is registered as unavailable so the replayed calls to it still resolve.
func newToolNames(tools []cliround.Tool, items []cliround.Item) (*toolNames, []toolSpec) {
	names := &toolNames{registered: map[string]string{}, original: map[string]string{}}
	var specs []toolSpec
	add := func(name, description string, schema json.RawMessage) {
		if _, exists := names.registered[name]; exists {
			return
		}
		short := registeredName(name)
		if strings.TrimSpace(description) == "" {
			description = name
		}
		names.registered[name] = toolPrefix + short
		names.original[toolPrefix+short] = name
		specs = append(specs, toolSpec{Name: short, Description: description, InputSchema: schema})
	}
	for _, tool := range tools {
		schema := tool.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		add(tool.Name, tool.Description, schema)
	}
	for _, item := range items {
		for _, call := range item.Calls {
			add(call.Name, "No longer available.", json.RawMessage(`{"type":"object"}`))
		}
	}
	return names, specs
}

func (n *toolNames) full(name string) string {
	return n.registered[name]
}

func (n *toolNames) platform(fullName string) (string, bool) {
	name, ok := n.original[fullName]
	return name, ok
}

// replayID is the id a historical call carries inside Claude Code: Platform's own when the API accepts it, else a
// stable stand-in.
func replayID(callID string) string {
	if callIDPattern.MatchString(callID) {
		return callID
	}
	sum := sha256.Sum256([]byte(callID))
	return "toolu_" + hex.EncodeToString(sum[:])[:24]
}

func encodedLength(text string) int {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(text)
	return buffer.Len()
}

func utf16Length(text string) int {
	units := 0
	for _, character := range text {
		units += utf16Width(character)
	}
	return units
}

func utf16Width(character rune) int {
	if character >= 0x10000 {
		return 2
	}
	return 1
}

func capToolResult(text string) string {
	return capText(text, maxToolResultUnits)
}

// capText cuts text that is larger than Claude Code passes to the model, and says so at the end of it.
func capText(text string, limit int) string {
	total := utf16Length(text)
	if total <= limit {
		return text
	}
	marker := fmt.Sprintf("\n[... cut by AgentParley: the text has %d characters and Claude Code passes at most %d to the model; the first part is shown]", total, limit)
	room := limit - len(marker)
	kept, units := 0, 0
	for kept < len(text) {
		character, size := utf8.DecodeRuneInString(text[kept:])
		width := utf16Width(character)
		if units+width > room {
			break
		}
		units += width
		kept += size
	}
	return text[:kept] + marker
}

type roundEnd struct {
	outcome string
	err     error
}

type round struct {
	texts    []string
	thinking []string
	events   []cliround.Event
	calls    []cliround.Call
	usage    cliround.Usage
	lastStop string
	done     chan roundEnd
	finished bool
}

// rebuildTimings are the phases of bringing a fresh session up to date, for the replay benchmark.
type rebuildTimings struct {
	Start, Seed, Replay, Total time.Duration
}

// Session is one warm Claude Code conversation. It implements cliround.Runner.
type Session struct {
	driver   *Driver
	process  *process
	frame    cliround.Frame
	consumed cliround.Consumed
	names    *toolNames

	mutex          sync.Mutex
	state          cliround.State
	current        *round
	replay         []stepDecision
	rebuildDone    chan struct{}
	carriedUsage   cliround.Usage
	carriedEvents  []cliround.Event
	offRoundLogged bool
	closed         bool
	timings        rebuildTimings
}

func (s *Session) Frame() cliround.Frame {
	return s.frame
}

func (s *Session) Consumed() *cliround.Consumed {
	return &s.consumed
}

func (s *Session) State() cliround.State {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.state
}

// Timings reports how long the rebuild that created this session took, phase by phase.
func (s *Session) Timings() rebuildTimings {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.timings
}

func (s *Session) TakeCarried() (cliround.Usage, []cliround.Event) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	usage, events := s.carriedUsage, s.carriedEvents
	s.carriedUsage, s.carriedEvents = cliround.Usage{}, nil
	return usage, events
}

// Close ends the process. Claude Code reports no usage on the way out, so nothing is flushed.
func (s *Session) Close() (cliround.Usage, error) {
	s.mutex.Lock()
	if s.closed {
		s.mutex.Unlock()
		return cliround.Usage{}, nil
	}
	s.closed = true
	if s.current != nil {
		s.finishLocked(s.current, "", &cliround.HarnessError{Message: "the Claude Code session was closed"})
	}
	s.mutex.Unlock()
	s.process.close()
	s.mutex.Lock()
	defer s.mutex.Unlock()
	usage := s.carriedUsage
	s.carriedUsage = cliround.Usage{}
	return usage, nil
}

// Extend continues the conversation with Platform's tool results (and any new user input), or opens the next turn.
func (s *Session) Extend(ctx context.Context, plan cliround.Plan, request *cliround.Request) (*cliround.Result, error) {
	current, err := s.openRound()
	if err != nil {
		return nil, err
	}
	switch {
	case len(plan.Results) > 0:
		s.deliverResults(plan.Results, plan.NewInput)
	case len(plan.NewInput) > 0:
		if err := s.process.bridge.send(ctx, command{Kind: commandSubmit, Text: joinTexts(plan.NewInput)}); err != nil {
			return nil, s.abort(current, err)
		}
	default:
		return nil, s.abort(current, &cliround.HarnessError{Message: "the round has nothing new for Claude Code to answer"})
	}
	return s.awaitRound(ctx, current)
}

func joinTexts(items []cliround.Item) string {
	texts := make([]string, 0, len(items))
	for _, item := range items {
		texts = append(texts, item.Text)
	}
	return strings.Join(texts, "\n\n")
}

// deliverResults hands each waiting tool.call hook its result; new user input rides on the last one as context, which
// the model reads right after the results.
func (s *Session) deliverResults(results, newInput []cliround.Item) {
	for index, result := range results {
		answer := toolAnswer{Text: capToolResult(result.Text)}
		if index == len(results)-1 {
			for _, item := range newInput {
				answer.Context = append(answer.Context, capText(item.Text, maxContextUnits))
			}
		}
		s.process.bridge.deliver(result.CallID, answer)
	}
}

func (s *Session) openRound() (*round, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.closed || !s.process.alive() {
		return nil, s.exitError()
	}
	if s.state == cliround.StateDirty {
		return nil, cliround.ErrSessionDirty
	}
	current := &round{done: make(chan roundEnd, 1)}
	s.current = current
	s.state = cliround.StateRunning
	return current, nil
}

func (s *Session) exitError() error {
	if s.process.alive() {
		return &cliround.HarnessError{Message: "the Claude Code session was closed"}
	}
	return classifyExit(s.process.screen.tail(12))
}

// classifyExit words a process that died mid-conversation.
func classifyExit(screen string) error {
	err := classifyScreen(screen)
	if harnessErr, ok := err.(*cliround.HarnessError); ok {
		return &cliround.HarnessError{Message: strings.Replace(harnessErr.Message, "did not start", "stopped", 1)}
	}
	return err
}

func (s *Session) abort(current *round, err error) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.endRoundLocked(current)
	return err
}

func (s *Session) endRoundLocked(current *round) {
	current.finished = true
	s.carriedUsage.Add(current.usage)
	s.carriedEvents = append(s.carriedEvents, current.events...)
	if s.current == current {
		s.current = nil
	}
	s.state = cliround.StateDirty
}

func (s *Session) finishLocked(current *round, outcome string, err error) {
	if current.finished {
		return
	}
	current.finished = true
	current.done <- roundEnd{outcome: outcome, err: err}
	s.markRebuiltLocked()
}

// awaitRound waits for the round to end and builds its result.
func (s *Session) awaitRound(ctx context.Context, current *round) (*cliround.Result, error) {
	select {
	case end := <-current.done:
		return s.completeRound(current, end)
	case <-s.process.exited:
		return nil, s.abort(current, s.exitError())
	case <-ctx.Done():
		return nil, s.abort(current, context.Cause(ctx))
	}
}

func (s *Session) completeRound(current *round, end roundEnd) (*cliround.Result, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if end.err != nil {
		s.endRoundLocked(current)
		return nil, end.err
	}
	result := &cliround.Result{
		Outcome: end.outcome,
		Text:    strings.Join(current.texts, "\n\n"),
		Calls:   current.calls,
		Usage:   current.usage,
		Events:  current.events,
	}
	if result.Calls == nil {
		result.Calls = []cliround.Call{}
	}
	if result.Events == nil {
		result.Events = []cliround.Event{}
	}
	if len(current.thinking) > 0 {
		thinking := strings.Join(current.thinking, "\n\n")
		result.Thinking = &thinking
	}
	s.current = nil
	switch end.outcome {
	case cliround.OutcomeToolCalls:
		s.state = cliround.StateWaitingOnPlatform
	case cliround.OutcomeAnswer:
		s.state = cliround.StateIdle
	default:
		s.state = cliround.StateDirty
	}
	return result, nil
}

func usageOf(reported *modUsage) cliround.Usage {
	if reported == nil {
		return cliround.Usage{}
	}
	return cliround.Usage{
		Input:       reported.InputTokens,
		CachedInput: reported.CacheReadTokens,
		CacheWrite:  reported.CacheCreationTokens,
		Output:      reported.OutputTokens,
		Requests:    1,
	}
}

func (s *Session) onStepStart(turnID string, index int) stepDecision {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if len(s.replay) > 0 {
		next := s.replay[0]
		s.replay = s.replay[1:]
		return next
	}
	if s.current == nil {
		s.offRoundLocked("a model step")
		return stepDecision{Mode: stepModeMadeUp, StopReason: stopReasonEndTurn}
	}
	s.markRebuiltLocked()
	return s.driver.decideStep()
}

func (s *Session) markRebuiltLocked() {
	if s.rebuildDone != nil {
		close(s.rebuildDone)
		s.rebuildDone = nil
	}
}

func (s *Session) onStepEnd(report stepReport) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	usage := usageOf(report.Usage)
	current := s.current
	if current == nil || current.finished {
		s.carriedUsage.Add(usage)
		return
	}
	current.usage.Add(usage)
	current.lastStop = report.StopReason
	if report.Text != "" {
		current.texts = append(current.texts, report.Text)
	}
	if report.Thinking != "" {
		current.thinking = append(current.thinking, report.Thinking)
	}

	var calls []cliround.Call
	for _, use := range report.ToolUses {
		name, ours := s.names.platform(use.Name)
		if !ours {
			current.events = append(current.events, cliround.Event{Kind: cliround.EventBlockedTool, Tool: use.Name, Detail: use.Name + " is not an AgentParley tool"})
			continue
		}
		arguments := use.Input
		if len(arguments) == 0 {
			arguments = json.RawMessage("{}")
		}
		calls = append(calls, cliround.Call{ID: use.ID, Name: name, Arguments: arguments})
	}
	switch {
	case len(calls) > 0:
		current.calls = calls
		s.finishLocked(current, cliround.OutcomeToolCalls, nil)
	case report.StopReason == stopReasonContextExceeded:
		s.finishLocked(current, "", cliround.ErrContextFull)
	}
}

func (s *Session) onTurnComplete(report turnReport) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	current := s.current
	if current == nil || current.finished {
		return
	}
	switch report.Reason {
	case turnReasonAnswer:
		if current.lastStop == stopReasonMaxTokens && len(current.texts) == 0 {
			s.finishLocked(current, cliround.OutcomeLength, nil)
			return
		}
		s.finishLocked(current, cliround.OutcomeAnswer, nil)
	case turnReasonRefusal:
		explanation := ""
		if report.Refusal != nil && report.Refusal.Explanation != nil {
			explanation = strings.TrimSpace(*report.Refusal.Explanation)
		}
		if explanation == "" {
			explanation = "Claude declined to answer this."
		}
		current.texts = append(current.texts, explanation)
		s.finishLocked(current, cliround.OutcomeRefused, nil)
	case turnReasonAborted:
		s.finishLocked(current, "", &cliround.HarnessError{Message: "Claude Code stopped the turn"})
	default:
		go s.failTurn(current)
	}
}

// failTurn words a turn that ended in error without Claude Code reporting why. The one case seen is its own
// context-limit check, which stops the turn before any API request and is only shown on the screen, a moment after
// the turn ends.
func (s *Session) failTurn(current *round) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		screen := s.process.screen.tail(6)
		if strings.Contains(strings.ToLower(screen), "context limit reached") {
			s.failRound(current, cliround.ErrContextFull)
			return
		}
		if time.Now().After(deadline) {
			message := "Claude Code failed the turn"
			if screen != "" {
				message += ". Its screen showed:\n" + screen
			}
			s.failRound(current, &cliround.HarnessError{Message: message})
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Session) failRound(current *round, err error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.finishLocked(current, "", err)
}

func (s *Session) onEvent(event modEvent) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	switch event.Kind {
	case eventBlockedTool:
		recorded := cliround.Event{Kind: cliround.EventBlockedTool, Tool: event.Tool, Detail: event.Detail}
		if s.current != nil && !s.current.finished {
			s.current.events = append(s.current.events, recorded)
		} else {
			s.carriedEvents = append(s.carriedEvents, recorded)
		}
	case eventError:
		mapped := mapModError(event.Error, event.Detail)
		if s.current != nil && !s.current.finished {
			s.finishLocked(s.current, "", mapped)
			return
		}
		s.driver.logger.Printf("claude: an error outside a round: %s %s", event.Error, event.Detail)
	}
}

// offRoundLocked records Claude Code working while no round is open: the session is no longer in step with
// AgentParley's history, so it is marked dirty and the next round rebuilds it.
func (s *Session) offRoundLocked(what string) {
	s.state = cliround.StateDirty
	if s.offRoundLogged {
		return
	}
	s.offRoundLogged = true
	s.carriedEvents = append(s.carriedEvents, cliround.Event{Kind: cliround.EventOffRoundActivity, Detail: "Claude Code worked between steps"})
	s.driver.logger.Printf("claude: activity between rounds (%s); the session will be rebuilt", what)
}

// mapModError turns the error class Claude Code reports into the daemon's typed errors.
func mapModError(kind, detail string) error {
	lowered := strings.ToLower(detail)
	switch kind {
	case "rate_limit", "billing_error":
		return cliround.ErrUsageLimit
	case "authentication_failed":
		return cliround.ErrSignedOut
	case "invalid_request":
		switch {
		case strings.Contains(lowered, "prompt is too long"), strings.Contains(lowered, "context window"), strings.Contains(lowered, "too many tokens"):
			return cliround.ErrContextFull
		case strings.Contains(lowered, "code_version_too_old"), strings.Contains(lowered, "too old"), strings.Contains(lowered, "minimum version"), strings.Contains(lowered, "upgrade"):
			return cliround.ErrUpdateRequired
		}
	}
	message := "Claude Code reported an error: " + kind
	if detail != "" {
		message += " (" + detail + ")"
	}
	return &cliround.HarnessError{Message: message}
}
