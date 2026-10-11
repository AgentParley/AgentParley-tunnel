package claudepty

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentparley/tunnel/internal/cliround"
)

const (
	// ReplayStepWindow is how many of the open turn's steps a rebuilt session replays with real tool-call structure;
	// everything before them is handed over as a record. See README "Rebuild timings".
	ReplayStepWindow = 64

	SeedChunkBytes = 3 << 20

	continuePrompt = "Continue the task from where the record above leaves off."
)

// planRebuild splits the history of a conversation that must be rebuilt in a fresh Claude Code.
//
// The open turn starts after the last assistant row that made no calls (its final answer). Its tool-calling steps are
// replayed as made-up steps (window) so the next real step sees them exactly as it would have; at most
// ReplayStepWindow of them, the latest. Everything before the window is the record (seed). The prompt is the user
// rows that opened the turn, or a continue line when they fell into the record.
func planRebuild(items []cliround.Item) (seed []cliround.Item, prompt string, window []cliround.Item) {
	openStart := 0
	for index := len(items) - 1; index >= 0; index-- {
		if items[index].Role == cliround.RoleAssistant && len(items[index].Calls) == 0 {
			openStart = index + 1
			break
		}
	}
	open := items[openStart:]
	leading := 0
	for leading < len(open) && open[leading].Role == cliround.RoleUser {
		leading++
	}
	rest := open[leading:]

	assistants := 0
	for _, item := range rest {
		if item.Role == cliround.RoleAssistant {
			assistants++
		}
	}
	if assistants == 0 {
		return items[:openStart], joinTexts(open[:leading]), nil
	}
	if assistants <= ReplayStepWindow {
		return items[:openStart], joinTexts(open[:leading]), rest
	}

	cut, counted := len(rest), 0
	for index := len(rest) - 1; index >= 0 && counted < ReplayStepWindow; index-- {
		if rest[index].Role == cliround.RoleAssistant {
			counted++
			cut = index
		}
	}
	return items[:openStart+leading+cut], continuePrompt, rest[cut:]
}

// seedChunks renders the record and splits it into pieces the mod can fetch.
func seedChunks(seed []cliround.Item) []string {
	if len(seed) == 0 {
		return nil
	}
	return chunkForMod(cliround.SeedHeader + cliround.RenderTranscript(seed))
}

// chunkForMod splits text so every piece's JSON form fits what the mod can fetch.
func chunkForMod(text string) []string {
	var pieces []string
	for _, chunk := range cliround.Chunk(text, SeedChunkBytes) {
		pieces = append(pieces, splitToFit(chunk)...)
	}
	return pieces
}

func splitToFit(text string) []string {
	if encodedLength(text) <= maxEncodedBytes || len(text) < 2 {
		return []string{text}
	}
	cut := len(text) / 2
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	if cut == 0 {
		return []string{text}
	}
	return append(splitToFit(text[:cut]), splitToFit(text[cut:])...)
}

// argumentsObject returns a historical call's arguments as a JSON object; anything else is wrapped as {"value": raw}.
func argumentsObject(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage("{}")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		return json.RawMessage(trimmed)
	}
	encoded, _ := json.Marshal(map[string]string{"value": trimmed})
	return encoded
}

// armReplay queues the made-up steps of the window and hands every historical result to the tool.call hooks that will
// ask for it. A user row between a batch's results and the next step rides on the batch's last result.
func (s *Session) armReplay(window []cliround.Item) {
	var steps []stepDecision
	answers := map[string]toolAnswer{}
	lastResult := ""
	for _, item := range window {
		switch item.Role {
		case cliround.RoleAssistant:
			step := stepDecision{Mode: "made-up", Text: item.Text, StopReason: "end_turn"}
			for _, call := range item.Calls {
				step.StopReason = "tool_use"
				step.ToolUses = append(step.ToolUses, madeUpUse{Name: s.names.full(call.Name), Input: argumentsObject(call.Arguments)})
				step.ToolIDs = append(step.ToolIDs, replayID(call.ID))
			}
			steps = append(steps, step)
		case cliround.RoleTool:
			lastResult = replayID(item.CallID)
			answers[lastResult] = toolAnswer{Text: capToolResult(item.Text)}
		case cliround.RoleUser:
			if lastResult != "" {
				answer := answers[lastResult]
				answer.Context = append(answer.Context, capText(item.Text, maxContextUnits))
				answers[lastResult] = answer
			}
		}
	}
	for _, step := range steps {
		for _, id := range step.ToolIDs {
			if _, ok := answers[id]; !ok {
				answers[id] = toolAnswer{Text: missingResultText}
			}
		}
	}

	s.mutex.Lock()
	s.replay = steps
	s.mutex.Unlock()
	for id, answer := range answers {
		s.process.bridge.deliver(id, answer)
	}
}

// Factory starts Claude Code, brings it up to date with the request's history (the record as hidden context rows, the
// open turn's latest steps as made-up steps) and runs the round on the first real model step.
func (d *Driver) Factory(ctx context.Context, model string, request *cliround.Request, budgets cliround.Budgets) (cliround.Runner, *cliround.Result, error) {
	seed, prompt, window := planRebuild(request.Items)
	if strings.TrimSpace(prompt) == "" {
		return nil, nil, &cliround.InvalidRequestError{Message: "the round has nothing new for Claude Code to answer"}
	}

	names, specs := newToolNames(request.Tools, request.Items)
	greeting := hello{Tools: specs}
	if request.System != nil {
		greeting.System = *request.System
	}
	if len(specs) > 0 {
		greeting.ToolNote = toolNote
	}
	session := &Session{
		driver:      d,
		frame:       cliround.FrameOf(model, request),
		names:       names,
		state:       cliround.StateStarting,
		rebuildDone: make(chan struct{}),
	}
	rebuilt := session.rebuildDone

	started := time.Now()
	rebuildCtx, cancelRebuild := context.WithTimeout(ctx, budgets.Rebuild)
	defer cancelRebuild()
	running, err := d.startProcess(rebuildCtx, processSpec{model: model, effort: request.Effort, hello: greeting, sink: session})
	if err != nil {
		return nil, nil, err
	}
	session.process = running
	fail := func(err error) (cliround.Runner, *cliround.Result, error) {
		_, _ = session.Close()
		return nil, nil, err
	}
	session.timings.Start = time.Since(started)

	seedStarted := time.Now()
	for _, chunk := range seedChunks(seed) {
		if err := running.bridge.sendAcknowledged(rebuildCtx, command{Kind: "seed", Text: chunk}, running.exited); err != nil {
			return fail(err)
		}
	}
	session.timings.Seed = time.Since(seedStarted)

	current, err := session.openRound()
	if err != nil {
		return fail(err)
	}
	replayStarted := time.Now()
	session.armReplay(window)
	if err := running.bridge.send(rebuildCtx, command{Kind: "submit", Text: prompt}); err != nil {
		return fail(err)
	}
	select {
	case <-rebuilt:
	case <-running.exited:
		return fail(session.exitError())
	case <-rebuildCtx.Done():
		return fail(context.Cause(rebuildCtx))
	}
	session.timings.Replay = time.Since(replayStarted)
	session.timings.Total = time.Since(started)

	stepCtx, cancelStep := context.WithTimeout(ctx, budgets.Step)
	defer cancelStep()
	result, err := session.awaitRound(stepCtx, current)
	if err != nil {
		return fail(err)
	}
	result.RebuildMs = max(1, session.timings.Total.Milliseconds())
	return session, result, nil
}
