package claudepty

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/agentparley/tunnel/internal/cliround"
	"github.com/agentparley/tunnel/internal/managedcli"
)

const (
	oneShotMaxTokens = 8000

	// maxOneShotBytes is the largest prompt Platform can send; a larger one means the box was asked for more than the
	// wire allows.
	maxOneShotBytes = 12 << 20

	utilityIdleLimit = 30 * time.Minute
)

// utility is the one shared Claude Code process for calls that belong to no conversation (a title, a compaction
// summary, a routing decision). It has no tools and no system prompt of its own; every call names its model.
type utility struct {
	process *process
	driver  *Driver

	// holds counts the calls using the process and isRetired marks one that is no longer the shared process; both
	// are guarded by the driver's utilityMutex.
	holds     int
	isRetired bool

	partsMutex sync.Mutex
	timer      *time.Timer
	sequence   int
}

type utilitySink struct{}

func (utilitySink) onStepStart(string, int) stepDecision {
	return stepDecision{Mode: stepModeMadeUp, StopReason: stopReasonEndTurn}
}
func (utilitySink) onStepEnd(stepReport)      {}
func (utilitySink) onTurnComplete(turnReport) {}
func (utilitySink) onEvent(modEvent)          {}

func (u *utility) close() {
	if u.timer != nil {
		u.timer.Stop()
	}
	u.process.close()
}

// utilityFor returns the shared process with a hold on it; the caller gives the hold back with releaseUtility. A
// process replaced by a version change, or dropped for idleness, closes once its last hold is released.
func (d *Driver) utilityFor(ctx context.Context, model string) (*utility, error) {
	if _, _, err := managedcli.Ensure(ctx, d.manifest(), "claude"); err != nil {
		return nil, err
	}

	var stale *utility
	defer func() {
		if stale != nil {
			stale.close()
		}
	}()
	d.utilityMutex.Lock()
	defer d.utilityMutex.Unlock()
	if current := d.utility; current != nil {
		pinned := d.manifest().Version("claude")
		if current.process.alive() && current.process.version == pinned && current.timer.Reset(utilityIdleLimit) {
			current.holds++
			return current, nil
		}
		d.utility = nil
		current.isRetired = true
		if current.holds == 0 {
			stale = current
		}
	}
	running, err := d.startProcess(ctx, processSpec{model: model, hello: hello{Tools: []toolSpec{}}, sink: utilitySink{}})
	if err != nil {
		return nil, err
	}
	created := &utility{process: running, driver: d, holds: 1}
	created.timer = time.AfterFunc(utilityIdleLimit, func() { d.dropUtility(created) })
	d.utility = created
	return created, nil
}

func (d *Driver) releaseUtility(target *utility) {
	d.utilityMutex.Lock()
	target.holds--
	isDone := target.isRetired && target.holds == 0
	d.utilityMutex.Unlock()
	if isDone {
		target.close()
	}
}

func (d *Driver) dropUtility(target *utility) {
	d.utilityMutex.Lock()
	if d.utility == target {
		d.utility = nil
	}
	target.isRetired = true
	isDone := target.holds == 0
	d.utilityMutex.Unlock()
	if isDone {
		target.close()
	}
}

func splitTrailingUser(items []cliround.Item) (history, trailing []cliround.Item) {
	cut := len(items)
	for cut > 0 && items[cut-1].Role == cliround.RoleUser {
		cut--
	}
	return items[:cut], items[cut:]
}

type completionReply struct {
	IsAnswered bool   `json:"isAnswered"`
	Text       string `json:"text"`
	Reason     string `json:"reason"`
	Error      string `json:"error"`
	Detail     string `json:"detail"`
	Usage      *struct {
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		CacheReadTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// OneShot runs a request that belongs to no conversation through the shared process's model.complete.
func (d *Driver) OneShot(ctx context.Context, model string, request *cliround.Request) (*cliround.Result, error) {
	history, trailing := splitTrailingUser(request.Items)
	if len(trailing) == 0 {
		return nil, &cliround.InvalidRequestError{Message: "a one-shot request must end with a user message"}
	}
	prompt := joinTexts(trailing)
	if len(history) > 0 {
		prompt = cliround.SeedHeader + cliround.RenderTranscript(history) + "\n\n" + prompt
	}
	if len(prompt) > maxOneShotBytes {
		return nil, &cliround.HarnessError{Message: "the one-shot prompt is too large for Claude Code on this box"}
	}

	running, err := d.utilityFor(ctx, model)
	if err != nil {
		return nil, err
	}
	defer d.releaseUtility(running)
	bridge := running.process.bridge

	running.partsMutex.Lock()
	running.sequence++
	id := fmt.Sprintf("complete-%d", running.sequence)
	waiter := bridge.expectCompletion(id)
	defer bridge.forgetCompletion(id)
	parts := chunkForMod(prompt)
	for _, part := range parts[:len(parts)-1] {
		if err := bridge.sendAcknowledged(ctx, command{Kind: commandCompletePart, ID: id, Text: part}, running.process.exited); err != nil {
			running.partsMutex.Unlock()
			return nil, err
		}
	}
	complete := command{Kind: commandComplete, ID: id, Model: model, Prompt: parts[len(parts)-1], MaxTokens: oneShotMaxTokens, Effort: request.Effort}
	if request.System != nil {
		complete.System = *request.System
	}
	err = bridge.send(ctx, complete)
	running.partsMutex.Unlock()
	if err != nil {
		return nil, err
	}

	select {
	case raw := <-waiter:
		return completionResult(raw)
	case <-running.process.exited:
		return nil, classifyExit(running.process.screen.tail(12))
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

func completionResult(raw json.RawMessage) (*cliround.Result, error) {
	var reply completionReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, &cliround.HarnessError{Message: "Claude Code returned an unreadable completion"}
	}
	if !reply.IsAnswered {
		switch reply.Reason {
		case "api-error":
			return nil, mapModError(reply.Error, reply.Detail)
		case "empty-reply":
			return nil, &cliround.HarnessError{Message: "Claude Code returned an empty reply"}
		default:
			detail := reply.Reason
			if reply.Detail != "" {
				detail += ": " + reply.Detail
			}
			return nil, &cliround.HarnessError{Message: "Claude Code could not complete the request (" + strings.TrimSpace(detail) + ")"}
		}
	}
	result := &cliround.Result{Outcome: cliround.OutcomeAnswer, Text: reply.Text, Calls: []cliround.Call{}, Events: []cliround.Event{}}
	if reply.Usage != nil {
		result.Usage = cliround.Usage{
			Input:       reply.Usage.InputTokens,
			CachedInput: reply.Usage.CacheReadTokens,
			CacheWrite:  reply.Usage.CacheCreationTokens,
			Output:      reply.Usage.OutputTokens,
			Requests:    1,
		}
	}
	return result, nil
}
