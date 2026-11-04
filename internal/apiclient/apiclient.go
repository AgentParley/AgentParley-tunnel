// Package apiclient is a small client for the platform's /api/v1 (API-key) surface.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"time"

	"github.com/agentparley/tunnel/internal/version"
)

type Client struct {
	server, apiKey, userAgent string
	http                      *http.Client
}

func New(server, apiKey string) *Client {
	return &Client{
		server:    server,
		apiKey:    apiKey,
		userAgent: "agentparley/" + version.Version + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")",
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

type Agent struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Slug        string  `json:"slug"`
	Description string  `json:"description"`
	ModelID     *string `json:"modelId"`
	IsEnabled   bool    `json:"isEnabled"`
}

type Chat struct {
	ID             string    `json:"id"`
	AgentID        string    `json:"agentId"`
	Title          *string   `json:"title"`
	State          string    `json:"state"`
	CreatedAt      time.Time `json:"createdAt"`
	LastActivityAt time.Time `json:"lastActivityAt"`
}

type ChatSummary struct {
	ID             string    `json:"id"`
	AgentID        string    `json:"agentId"`
	Title          *string   `json:"title"`
	CreatedAt      time.Time `json:"createdAt"`
	LastActivityAt time.Time `json:"lastActivityAt"`
}

type Message struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Text       string    `json:"text"`
	CreatedAt  time.Time `json:"createdAt"`
	SenderName *string   `json:"senderName"`
}

type MessagesPage struct {
	Messages           []Message `json:"messages"`
	NextAfter          *string   `json:"nextAfter"`
	HasMore            bool      `json:"hasMore"`
	State              string    `json:"state"`
	NextPollIntervalMs int       `json:"nextPollIntervalMs"`
}

// Error is any non-2xx answer; Message is the body's "message" when it has one.
type Error struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	var agents []Agent
	err := c.do(ctx, http.MethodGet, "/api/v1/agents", nil, &agents, true, "")
	return agents, err
}

// ListChats lists the chats this key started, newest first; agentID "" means every agent.
func (c *Client) ListChats(ctx context.Context, agentID string, limit int) ([]ChatSummary, error) {
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if agentID != "" {
		query.Set("agentId", agentID)
	}
	var chats []ChatSummary
	err := c.do(ctx, http.MethodGet, "/api/v1/chats?"+query.Encode(), nil, &chats, true, "")
	return chats, err
}

func (c *Client) StartChat(ctx context.Context, agentID, text, idempotencyKey string) (*Chat, error) {
	var chat Chat
	body := map[string]string{"agentId": agentID, "text": text}
	if err := c.do(ctx, http.MethodPost, "/api/v1/chats", body, &chat, true, idempotencyKey); err != nil {
		return nil, err
	}
	return &chat, nil
}

// SendMessage is never retried: a repeat could post the message twice. after is nil when the chat had no readable line.
func (c *Client) SendMessage(ctx context.Context, chatID, text string) (after *string, err error) {
	var response struct {
		After *string `json:"after"`
	}
	err = c.do(ctx, http.MethodPost, "/api/v1/chats/"+url.PathEscape(chatID)+"/messages", map[string]string{"text": text}, &response, false, "")
	return response.After, err
}

func (c *Client) Messages(ctx context.Context, chatID string, after *string, limit int) (*MessagesPage, error) {
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if after != nil {
		query.Set("after", *after)
	}
	var page MessagesPage
	if err := c.do(ctx, http.MethodGet, "/api/v1/chats/"+url.PathEscape(chatID)+"/messages?"+query.Encode(), nil, &page, true, ""); err != nil {
		return nil, err
	}
	return &page, nil
}

// DeleteSelf deletes the key this client is using.
func (c *Client) DeleteSelf(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/keys/self", nil, nil, true, "")
}

func (c *Client) do(ctx context.Context, method, path string, body, out any, retry bool, idempotencyKey string) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	attempts := 1
	if retry {
		attempts = 4
	}
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		err := c.once(ctx, method, path, payload, out, idempotencyKey)
		if err == nil || attempt >= attempts || ctx.Err() != nil || !retryable(err) {
			return err
		}
		wait := backoff
		backoff *= 2
		var apiErr *Error
		if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
			wait = apiErr.RetryAfter
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

func (c *Client) once(ctx context.Context, method, path string, payload []byte, out any, idempotencyKey string) error {
	httpRequest, err := http.NewRequestWithContext(ctx, method, c.server+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpRequest.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		httpRequest.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		httpRequest.Header.Set("Idempotency-Key", idempotencyKey)
	}

	httpResponse, err := c.http.Do(httpRequest)
	if err != nil {
		return err
	}
	defer httpResponse.Body.Close()
	data, err := io.ReadAll(io.LimitReader(httpResponse.Body, 16<<20))
	if err != nil {
		return err
	}

	if httpResponse.StatusCode >= 200 && httpResponse.StatusCode < 300 {
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("decoding the response from %s: %w", path, err)
			}
		}
		return nil
	}

	apiErr := &Error{Status: httpResponse.StatusCode}
	var parsed struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(data, &parsed) == nil {
		apiErr.Message, apiErr.Code = parsed.Message, parsed.Code
	}
	if seconds, err := strconv.Atoi(httpResponse.Header.Get("Retry-After")); err == nil && seconds > 0 {
		apiErr.RetryAfter = time.Duration(seconds) * time.Second
	}
	return apiErr
}

func retryable(err error) bool {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return true
	}
	switch apiErr.Status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}
