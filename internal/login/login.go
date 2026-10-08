// Package login implements the RFC 8628 device grant the daemon runs exactly once per install: obtain a user code,
// wait for portal approval, redeem it for a one-time enrolment token, generate this box's Ed25519 identity key, and
// exchange the enrolment token for a durable refresh token.
package login

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/agentparley/tunnel/internal/credstore"
	"github.com/agentparley/tunnel/internal/machineid"
	"github.com/agentparley/tunnel/internal/version"
)

// ErrExpired and ErrDenied are terminal outcomes — the daemon stops polling and tells the user to run `login`
// again (expired) or that the request was rejected (denied), rather than retrying forever.
var (
	ErrExpired = errors.New("device code expired — run 'agentparley login' again")
	ErrDenied  = errors.New("device authorization was denied")
)

// Prompt is what login.Run reports back to the caller so it can show the user code before polling begins.
type Prompt struct {
	UserCode        string
	VerificationURI string
	IntervalSeconds int
}

// RetryableError is a failed enrolment worth repeating later. RetryAfter is the platform's own Retry-After, zero when
// it sent none.
type RetryableError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// TokenRejectedError means the platform answered 400 with code "enrolment_token_rejected" — unknown, expired, revoked
// or already used. The token is not presented again this boot.
type TokenRejectedError struct {
	Status int
}

func (e *TokenRejectedError) Error() string {
	return fmt.Sprintf("the platform rejected the enrolment token (status %d)", e.Status)
}

// Result is the durable identity produced by a successful login, ready to hand to credstore.Store.
type Result struct {
	Credentials *credstore.Credentials
}

type apiClient struct {
	baseURL string
	http    *http.Client
}

// Run drives the full device-grant flow against apiBaseURL. onPrompt is called once, as soon as the user code is
// issued, so the caller can print it before this function blocks on the poll loop.
func Run(apiBaseURL, runAsUser string, onPrompt func(Prompt)) (*Result, error) {
	client := &apiClient{baseURL: apiBaseURL, http: &http.Client{Timeout: 15 * time.Second}}

	deviceCodeResponse, err := client.issueDeviceCode()
	if err != nil {
		return nil, fmt.Errorf("requesting a device code: %w", err)
	}

	onPrompt(Prompt{
		UserCode:        deviceCodeResponse.UserCode,
		VerificationURI: deviceCodeResponse.VerificationURI,
		IntervalSeconds: deviceCodeResponse.IntervalSeconds,
	})

	enrolmentToken, err := client.pollForEnrolmentToken(deviceCodeResponse)
	if err != nil {
		return nil, err
	}

	return client.enrolWithToken(apiBaseURL, runAsUser, enrolmentToken)
}

// RunWithToken is the non-interactive login: it exchanges an enrolment token that was handed to this box out of band
// (a token file) for a durable identity, with exactly the request Run sends after a redeemed device code. Failures
// are typed: *RetryableError (network, 429, 5xx — try again later) or *TokenRejectedError (the platform refused
// this token; presenting it again is pointless).
func RunWithToken(apiBaseURL, runAsUser, enrolmentToken string) (*Result, error) {
	client := &apiClient{baseURL: apiBaseURL, http: &http.Client{Timeout: 15 * time.Second}}
	return client.enrolWithToken(apiBaseURL, runAsUser, enrolmentToken)
}

func (client *apiClient) enrolWithToken(apiBaseURL, runAsUser, enrolmentToken string) (*Result, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating the install key: %w", err)
	}

	serverHost := hostOf(apiBaseURL)
	machineID, err := machineid.Get(serverHost)
	if err != nil {
		return nil, fmt.Errorf("determining this box's machine id: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	enrolResponse, err := client.enrol(enrolmentTokenRequest{
		EnrolmentToken:  enrolmentToken,
		MachineID:       machineID,
		PublicKey:       base64.StdEncoding.EncodeToString(publicKey),
		Hostname:        hostname,
		RunAsUser:       runAsUser,
		OperatingSystem: runtime.GOOS,
		Architecture:    runtime.GOARCH,
		AgentVersion:    version.Version,
	})
	if err != nil {
		return nil, fmt.Errorf("exchanging the enrolment token: %w", err)
	}

	return &Result{Credentials: &credstore.Credentials{
		RefreshToken: enrolResponse.RefreshToken,
		PrivateKey:   base64.StdEncoding.EncodeToString(privateKey.Seed()),
		PublicKey:    base64.StdEncoding.EncodeToString(publicKey),
		RoutingKey:   enrolResponse.RoutingKey,
		SSHHostID:    enrolResponse.SSHHostID,
		MachineID:    machineID,
	}}, nil
}

func (client *apiClient) issueDeviceCode() (*deviceCodeResponse, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown-host"
	}
	machineHint := fmt.Sprintf("%s (%s/%s)", hostname, runtime.GOOS, runtime.GOARCH)

	var response deviceCodeResponse
	if err := client.postJSON("/tunnel/device/code", deviceCodeRequest{MachineHint: machineHint}, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (client *apiClient) pollForEnrolmentToken(deviceCode *deviceCodeResponse) (string, error) {
	intervalSeconds := deviceCode.IntervalSeconds
	deadline := time.Now().Add(time.Until(deviceCode.ExpiresAt))

	for {
		if time.Now().After(deadline) {
			return "", ErrExpired
		}
		time.Sleep(time.Duration(intervalSeconds) * time.Second)

		var tokenResponse deviceTokenResponse
		var tokenError deviceTokenError
		status, err := client.postJSONWithStatus("/tunnel/device/token", deviceTokenRequest{DeviceCode: deviceCode.DeviceCode}, &tokenResponse, &tokenError)
		if err != nil {
			return "", err
		}

		if status == http.StatusOK {
			return tokenResponse.EnrolmentToken, nil
		}

		switch tokenError.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			intervalSeconds += 5
			continue
		case "expired_token":
			return "", ErrExpired
		default:
			return "", ErrDenied
		}
	}
}

// enrol classifies its failures so a non-interactive caller can tell "try again later" (*RetryableError) from "this
// token will never work" (*TokenRejectedError) — see RunWithToken.
func (client *apiClient) enrol(request enrolmentTokenRequest) (*enrolResponse, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	httpResponse, err := client.http.Post(client.baseURL+"/tunnel/enrol", "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, &RetryableError{Err: err}
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode >= 200 && httpResponse.StatusCode < 300 {
		var response enrolResponse
		if err := json.NewDecoder(httpResponse.Body).Decode(&response); err != nil {
			return nil, &RetryableError{Err: fmt.Errorf("decoding response from /tunnel/enrol: %w", err)}
		}
		return &response, nil
	}

	if httpResponse.StatusCode == http.StatusBadRequest && isExplicitRefusal(httpResponse.Body) {
		return nil, &TokenRejectedError{Status: httpResponse.StatusCode}
	}
	return nil, &RetryableError{
		Err:        fmt.Errorf("/tunnel/enrol returned status %d", httpResponse.StatusCode),
		RetryAfter: parseRetryAfter(httpResponse.Header.Get("Retry-After")),
	}
}

// enrolmentTokenRejectedCode is the only answer that means "this token will never work"; every other non-success
// answer (an ingress 404 or 400 mid-deploy, 403, HTML, 503 enrolment_not_ready) is retried.
const enrolmentTokenRejectedCode = "enrolment_token_rejected"

// isExplicitRefusal reads a 400 body defensively: anything that is not JSON with the refusal code is not a refusal.
func isExplicitRefusal(body io.Reader) bool {
	data, err := io.ReadAll(io.LimitReader(body, 64*1024))
	if err != nil {
		return false
	}
	var errorBody struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &errorBody); err != nil {
		return false
	}
	return errorBody.Code == enrolmentTokenRejectedCode
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if retryTime, err := http.ParseTime(value); err == nil {
		return max(time.Until(retryTime), 0)
	}
	return 0
}

func (client *apiClient) postJSON(path string, requestBody, responseBody any) error {
	status, err := client.postJSONWithStatus(path, requestBody, responseBody, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%s returned unexpected status %d", path, status)
	}
	return nil
}

// postJSONWithStatus decodes into successBody on 2xx, or into errorBody (if non-nil) on any other status —
// callers that need to distinguish "authorization_pending" from a genuine failure pass both.
func (client *apiClient) postJSONWithStatus(path string, requestBody, successBody, errorBody any) (int, error) {
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return 0, err
	}

	httpResponse, err := client.http.Post(client.baseURL+path, "application/json", bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode >= 200 && httpResponse.StatusCode < 300 {
		if successBody != nil {
			if err := json.NewDecoder(httpResponse.Body).Decode(successBody); err != nil {
				return httpResponse.StatusCode, fmt.Errorf("decoding response from %s: %w", path, err)
			}
		}
		return httpResponse.StatusCode, nil
	}

	if errorBody != nil {
		_ = json.NewDecoder(httpResponse.Body).Decode(errorBody)
		return httpResponse.StatusCode, nil
	}
	return httpResponse.StatusCode, fmt.Errorf("%s returned status %d", path, httpResponse.StatusCode)
}

type deviceCodeRequest struct {
	MachineHint string `json:"machineHint"`
}

type deviceCodeResponse struct {
	UserCode        string    `json:"userCode"`
	DeviceCode      string    `json:"deviceCode"`
	VerificationURI string    `json:"verificationUri"`
	IntervalSeconds int       `json:"intervalSeconds"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

type deviceTokenRequest struct {
	DeviceCode string `json:"deviceCode"`
}

type deviceTokenResponse struct {
	EnrolmentToken string `json:"enrolmentToken"`
}

type deviceTokenError struct {
	Error string `json:"error"`
}

type enrolmentTokenRequest struct {
	EnrolmentToken  string `json:"enrolmentToken"`
	MachineID       string `json:"machineId"`
	PublicKey       string `json:"publicKey"`
	Hostname        string `json:"hostname"`
	RunAsUser       string `json:"runAsUser"`
	OperatingSystem string `json:"operatingSystem"`
	Architecture    string `json:"architecture"`
	AgentVersion    string `json:"agentVersion"`
}

type enrolResponse struct {
	RefreshToken string    `json:"refreshToken"`
	AccessToken  string    `json:"accessToken"`
	RoutingKey   string    `json:"routingKey"`
	SSHHostID    string    `json:"sshHostId"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

// hostOf strips the scheme and any path from a base URL, leaving the bare host[:port] the machine-id hash mixes
// in. Falls back to the raw input if it doesn't parse as a URL — machine-id hashing only needs SOME stable string
// tied to the server, not a strictly valid host.
func hostOf(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return baseURL
	}
	return parsed.Host
}
