package claudepty

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/agentparley/tunnel/internal/cliround"
	"github.com/agentparley/tunnel/internal/managedcli"
)

// benchmarkStepBytes is the size of one synthetic tool result.
const benchmarkStepBytes = 1024

// RunReplayBenchmark times the rebuild path of a fresh Claude Code for histories of each size in steps, in two shapes:
// all closed turns (the record only) and one long open turn (record plus the replayed window). No model is called: the
// processes talk to a local stand-in for the API and the step after the window is answered by the mod itself.
func RunReplayBenchmark(ctx context.Context, manifest func() managedcli.Manifest, steps []int, output io.Writer) error {
	stub, baseURL, err := startStubAPI()
	if err != nil {
		return err
	}
	defer stub.Close()

	driver := NewDriver(manifest, log.New(io.Discard, "", 0))
	driver.baseURL = baseURL
	driver.stubToken = "sk-ant-oat01-benchmark-not-a-login"
	driver.benchmark = true
	defer driver.Shutdown()

	fmt.Fprintf(output, "replay window %d steps, one step is a tool call plus a %d-byte result\n", ReplayStepWindow, benchmarkStepBytes)
	fmt.Fprintf(output, "%-16s %6s %10s %10s %10s %10s\n", "shape", "steps", "start", "seed", "window", "total")
	for _, count := range steps {
		for _, shape := range []string{"closed turns", "one open turn"} {
			request := syntheticHistory(shape, count)
			timings, err := timeRebuild(ctx, driver, request)
			if err != nil {
				return fmt.Errorf("%s with %d steps: %w", shape, count, err)
			}
			fmt.Fprintf(output, "%-16s %6d %10s %10s %10s %10s\n", shape, count,
				rounded(timings.Start), rounded(timings.Seed), rounded(timings.Replay), rounded(timings.Total))
		}
	}
	return nil
}

func rounded(duration time.Duration) string {
	return duration.Round(10 * time.Millisecond).String()
}

func timeRebuild(ctx context.Context, driver *Driver, request *cliround.Request) (rebuildTimings, error) {
	rebuildCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	runner, _, err := driver.Factory(rebuildCtx, "sonnet", request, cliround.Budgets{Rebuild: 4 * time.Minute, Step: time.Minute})
	if err != nil {
		return rebuildTimings{}, err
	}
	defer runner.Close()
	return runner.(*Session).Timings(), nil
}

func syntheticHistory(shape string, count int) *cliround.Request {
	request := &cliround.Request{
		Tools: []cliround.Tool{{Name: "run_command", Description: "Run a command.", Parameters: []byte(`{"type":"object","properties":{"command":{"type":"string"}}}`)}},
	}
	result := strings.Repeat("x", benchmarkStepBytes)
	key := 0
	add := func(item cliround.Item) {
		key++
		item.Key = fmt.Sprintf("k%d", key)
		request.Items = append(request.Items, item)
	}
	step := func(index int) {
		callID := fmt.Sprintf("call_%d", index)
		add(cliround.Item{Role: cliround.RoleAssistant, Text: fmt.Sprintf("Step %d: checking.", index), Calls: []cliround.Call{{ID: callID, Name: "run_command", Arguments: []byte(fmt.Sprintf(`{"command":"echo %d"}`, index))}}})
		add(cliround.Item{Role: cliround.RoleTool, CallID: callID, Text: result})
	}

	switch shape {
	case "closed turns":
		const stepsPerTurn = 5
		for index := 0; index < count; index++ {
			if index%stepsPerTurn == 0 {
				add(cliround.Item{Role: cliround.RoleUser, Text: fmt.Sprintf("Question %d", index/stepsPerTurn)})
			}
			step(index)
			if index%stepsPerTurn == stepsPerTurn-1 || index == count-1 {
				add(cliround.Item{Role: cliround.RoleAssistant, Text: "Done."})
			}
		}
		add(cliround.Item{Role: cliround.RoleUser, Text: "One more question"})
	default:
		add(cliround.Item{Role: cliround.RoleUser, Text: "Please work through the whole list."})
		for index := 0; index < count; index++ {
			step(index)
		}
	}
	return request
}

// startStubAPI serves just enough of the Messages API for Claude Code's own start-up calls to succeed.
func startStubAPI() (*http.Server, string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		if request.Method != http.MethodPost || !strings.HasPrefix(request.URL.Path, "/v1/messages") {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Connection", "close")
		for _, event := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_stub","type":"message","role":"assistant","content":[],"model":"stub","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		} {
			_, _ = io.WriteString(writer, event+"\n\n")
		}
	})}
	go func() { _ = server.Serve(listener) }()
	return server, "http://" + listener.Addr().String(), nil
}
