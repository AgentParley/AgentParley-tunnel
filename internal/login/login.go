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
	"net/http"
	"net/url"
	"os"
	"runtime"
	"time"

	"github.com/agentparley/tunnel/internal/credstore"
	"github.com/agentparley/tunnel/internal/machineid"
	"github.com/agentparley/tunnel/internal/version"
)

// ErrExpired and ErrDenied are terminal outcomes — the daemon stops polling and tells the user to run `login`
// again (expired) or that the request was rejected (denied), rather than retrying forever.
var (
	ErrExpired = errors.New("device code expired — run 'agentparley-tunnel login' again")
	ErrDenied  = errors.New("device authorization was denied")
)

// Prompt is what login.Run reports back to the caller so it can show the user code before polling begins.
type Prompt struct {
	UserCode        string
	VerificationURI string
	IntervalSeconds int
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

func (client *apiClient) enrol(request enrolmentTokenRequest) (*enrolResponse, error) {
	var response enrolResponse
	if err := client.postJSON("/tunnel/enrol", request, &response); err != nil {
		return nil, err
	}
	return &response, nil
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
