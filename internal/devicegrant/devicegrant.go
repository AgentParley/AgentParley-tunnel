// Package devicegrant is the OS-neutral RFC 8628 device grant the platform serves under /tunnel/device: the daemon's
// `login` redeems it for an enrolment token, `auth login` for an API key.
package devicegrant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"
)

type Purpose string

const (
	PurposeTunnel Purpose = "tunnel"
	PurposeCLI    Purpose = "cli"
)

type Request struct {
	Purpose         Purpose `json:"purpose"`
	MachineHint     string  `json:"machineHint"`
	HostName        string  `json:"hostName,omitempty"`
	OperatingSystem string  `json:"operatingSystem,omitempty"`
	Architecture    string  `json:"architecture,omitempty"`
	RunAsUser       string  `json:"runAsUser,omitempty"`
}

type Code struct {
	UserCode                string    `json:"userCode"`
	DeviceCode              string    `json:"deviceCode"`
	VerificationURI         string    `json:"verificationUri"`
	VerificationURIComplete string    `json:"verificationUriComplete"`
	IntervalSeconds         int       `json:"intervalSeconds"`
	ExpiresAt               time.Time `json:"expiresAt"`
}

var (
	ErrExpired     = errors.New("the code expired before it was approved")
	ErrDenied      = errors.New("the request was refused in the browser")
	ErrAlreadyUsed = errors.New("this code was already used")
)

// LocalRequest describes this computer: host name, GOOS/GOARCH, and machineHint "<host> (<os>/<arch>)".
func LocalRequest(purpose Purpose, runAsUser string) Request {
	hostName, err := os.Hostname()
	if err != nil || hostName == "" {
		hostName = "unknown-host"
	}
	return Request{
		Purpose:         purpose,
		MachineHint:     fmt.Sprintf("%s (%s/%s)", hostName, runtime.GOOS, runtime.GOARCH),
		HostName:        hostName,
		OperatingSystem: runtime.GOOS,
		Architecture:    runtime.GOARCH,
		RunAsUser:       runAsUser,
	}
}

func Issue(ctx context.Context, httpClient *http.Client, baseURL string, request Request) (*Code, error) {
	status, body, err := post(ctx, httpClient, baseURL+"/tunnel/device/code", request)
	if err != nil {
		return nil, fmt.Errorf("requesting a device code: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("requesting a device code: %s", failure(status, body))
	}
	var code Code
	if err := json.Unmarshal(body, &code); err != nil {
		return nil, fmt.Errorf("requesting a device code: decoding the response: %w", err)
	}
	return &code, nil
}

// Poll waits for approval and decodes the 200 body into success.
func Poll(ctx context.Context, httpClient *http.Client, baseURL string, code *Code, success any) error {
	interval := time.Duration(code.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}

	for {
		if time.Now().After(code.ExpiresAt) {
			return ErrExpired
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}

		status, body, err := post(ctx, httpClient, baseURL+"/tunnel/device/token", map[string]string{"deviceCode": code.DeviceCode})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}

		switch {
		case status == http.StatusOK:
			if err := json.Unmarshal(body, success); err != nil {
				return fmt.Errorf("decoding the token response: %w", err)
			}
			return nil
		case status >= 500:
			continue
		case status != http.StatusBadRequest:
			return fmt.Errorf("the token request failed: %s", failure(status, body))
		}

		var refusal struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &refusal)
		switch refusal.Error {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "expired_token":
			return ErrExpired
		case "invalid_grant":
			return ErrAlreadyUsed
		default:
			return ErrDenied
		}
	}
}

func post(ctx context.Context, httpClient *http.Client, url string, payload any) (int, []byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpResponse, err := httpClient.Do(httpRequest)
	if err != nil {
		return 0, nil, err
	}
	defer httpResponse.Body.Close()
	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, 1<<20))
	return httpResponse.StatusCode, body, err
}

func failure(status int, body []byte) string {
	var parsed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Message != "" {
		return fmt.Sprintf("%s (HTTP %d)", parsed.Message, status)
	}
	return fmt.Sprintf("HTTP %d", status)
}
