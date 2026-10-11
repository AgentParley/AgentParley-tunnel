// Package codexrpc drives Codex through its app-server: one long-lived `codex app-server` process per box, speaking
// JSON-RPC over stdin and stdout, with one app-server thread per warm AgentParley conversation. A thread implements
// cliround.Runner. AgentParley's tools are dynamic tools in the `agentparley` namespace; the daemon holds each
// tool-call request open until Platform has run the tool and sends the result in its next round.
package codexrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"
	"time"
)

const maxLineBytes = 64 << 20

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("codex app-server error %d: %s", e.Code, e.Message)
}

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// threadSink receives everything the app-server sends about one thread, from the client's reader goroutine, so it
// must not block.
type threadSink interface {
	onNotification(method string, params json.RawMessage)
	onServerRequest(id json.RawMessage, method string, params json.RawMessage)
	onServerExit(err error)
}

type response struct {
	result json.RawMessage
	err    error
}

type client struct {
	logger *log.Logger
	writer io.Writer

	writeMutex sync.Mutex

	mutex   sync.Mutex
	nextID  int64
	pending map[string]chan response
	threads map[string]threadSink
	exited  error
}

func newClient(writer io.Writer, logger *log.Logger) *client {
	return &client{logger: logger, writer: writer, pending: map[string]chan response{}, threads: map[string]threadSink{}}
}

func (c *client) write(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.writeMutex.Lock()
	defer c.writeMutex.Unlock()
	_, err = c.writer.Write(append(encoded, '\n'))
	return err
}

// call sends one request and waits for its response.
func (c *client) call(ctx context.Context, method string, params any, result any) error {
	c.mutex.Lock()
	if c.exited != nil {
		c.mutex.Unlock()
		return c.exited
	}
	c.nextID++
	id := c.nextID
	responses := make(chan response, 1)
	c.pending[strconv.FormatInt(id, 10)] = responses
	c.mutex.Unlock()

	forget := func() {
		c.mutex.Lock()
		delete(c.pending, strconv.FormatInt(id, 10))
		c.mutex.Unlock()
	}
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		forget()
		return fmt.Errorf("writing %s to codex app-server: %w", method, err)
	}
	select {
	case reply := <-responses:
		if reply.err != nil {
			return reply.err
		}
		if result != nil && len(reply.result) > 0 {
			if err := json.Unmarshal(reply.result, result); err != nil {
				return fmt.Errorf("reading the %s response: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		forget()
		return ctx.Err()
	}
}

func (c *client) notify(method string) error {
	return c.write(map[string]any{"method": method})
}

func (c *client) respond(id json.RawMessage, result any) {
	if err := c.write(map[string]any{"id": id, "result": result}); err != nil {
		c.logger.Printf("codex: answering a server request: %v", err)
	}
}

func (c *client) respondError(id json.RawMessage, code int, text string) {
	if err := c.write(map[string]any{"id": id, "error": map[string]any{"code": code, "message": text}}); err != nil {
		c.logger.Printf("codex: answering a server request: %v", err)
	}
}

func (c *client) register(threadID string, sink threadSink) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.threads[threadID] = sink
}

func (c *client) unregister(threadID string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	delete(c.threads, threadID)
}

func (c *client) sinkFor(params json.RawMessage) threadSink {
	var addressed struct {
		ThreadID string `json:"threadId"`
	}
	if json.Unmarshal(params, &addressed) != nil || addressed.ThreadID == "" {
		return nil
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.threads[addressed.ThreadID]
}

// readLoop dispatches every line the app-server prints until its stdout closes.
func (c *client) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 1<<20), maxLineBytes)
	for scanner.Scan() {
		var incoming message
		if err := json.Unmarshal(scanner.Bytes(), &incoming); err != nil {
			c.logger.Printf("codex: unreadable line from the app-server: %v", err)
			continue
		}
		switch {
		case len(incoming.ID) > 0 && incoming.Method != "":
			c.handleServerRequest(incoming)
		case len(incoming.ID) > 0:
			c.handleResponse(incoming)
		case incoming.Method != "":
			if sink := c.sinkFor(incoming.Params); sink != nil {
				sink.onNotification(incoming.Method, incoming.Params)
			}
		}
	}
	err := scanner.Err()
	if err == nil {
		err = errors.New("codex app-server exited")
	}
	c.fail(err)
}

func (c *client) handleResponse(incoming message) {
	c.mutex.Lock()
	responses := c.pending[string(incoming.ID)]
	delete(c.pending, string(incoming.ID))
	c.mutex.Unlock()
	if responses == nil {
		return
	}
	if incoming.Error != nil {
		responses <- response{err: incoming.Error}
		return
	}
	responses <- response{result: incoming.Result}
}

func (c *client) handleServerRequest(incoming message) {
	switch incoming.Method {
	case "currentTime/read":
		c.respond(incoming.ID, map[string]any{"unixTimeMs": time.Now().UnixMilli()})
		return
	case "account/chatgptAuthTokens/refresh", "attestation/generate":
		c.respondError(incoming.ID, -32000, "not available to AgentParley")
		return
	}
	sink := c.sinkFor(incoming.Params)
	if sink == nil {
		c.respondError(incoming.ID, -32000, "no such thread")
		return
	}
	sink.onServerRequest(incoming.ID, incoming.Method, incoming.Params)
}

// fail ends every in-flight call and tells every thread the app-server is gone.
func (c *client) fail(err error) {
	c.mutex.Lock()
	if c.exited != nil {
		c.mutex.Unlock()
		return
	}
	c.exited = err
	pending := c.pending
	c.pending = map[string]chan response{}
	sinks := make([]threadSink, 0, len(c.threads))
	for _, sink := range c.threads {
		sinks = append(sinks, sink)
	}
	c.threads = map[string]threadSink{}
	c.mutex.Unlock()

	for _, responses := range pending {
		responses <- response{err: err}
	}
	for _, sink := range sinks {
		sink.onServerExit(err)
	}
}
