package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentparley/tunnel/internal/credstore"
	"github.com/agentparley/tunnel/internal/login"
)

const (
	tokenLoginInitialBackoff = 5 * time.Second
	tokenLoginMaxBackoff     = 5 * time.Minute
	maxRetryAfter            = time.Hour

	// tokenFileRecheckInterval is how often an idle daemon looks at the token file again.
	tokenFileRecheckInterval = time.Minute

	rejectedTokenFileName = "rejected-enrolment-token"
)

// readEnrolmentToken returns the trimmed content of the token file, or "" when it is missing or empty.
func (d *Daemon) readEnrolmentToken() string {
	data, err := os.ReadFile(d.tunnelConfig.EnrolmentTokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// enrolFromTokenFile logs in with the token file, with no person. It returns credentials once enrolment succeeds, or
// ctx's error when the daemon is asked to stop first. It never returns for any other reason and never calls the
// platform in a tight loop: every failure except the platform's explicit refusal (400 enrolment_token_rejected) backs
// off from 5 s to 5 min (honouring Retry-After), and a refused token is remembered until the next boot and not
// presented again — the daemon then idles, looking at the file once a minute and trying only when its content has
// changed.
func (d *Daemon) enrolFromTokenFile(ctx context.Context) (*credstore.Credentials, error) {
	retryDelay := newBackoffBetween(tokenLoginInitialBackoff, tokenLoginMaxBackoff)
	rejectedHash := loadRejectedTokenHash()
	idleLogged := false

	for {
		enrolmentToken := d.readEnrolmentToken()
		if enrolmentToken == "" || hashToken(enrolmentToken) == rejectedHash {
			if !idleLogged {
				d.logger.Printf("no usable enrolment token in %s; waiting for it to change", d.tunnelConfig.EnrolmentTokenFile)
				idleLogged = true
			}
			if err := sleepContext(ctx, tokenFileRecheckInterval); err != nil {
				return nil, err
			}
			continue
		}
		idleLogged = false

		result, err := login.RunWithToken(d.apiBaseURL, d.runAsUser.Username, enrolmentToken)
		if err == nil {
			if err := d.store.Save(result.Credentials); err != nil {
				return nil, fmt.Errorf("saving credentials after token-file login: %w", err)
			}
			d.logger.Printf("enrolled from %s as ssh host %s", d.tunnelConfig.EnrolmentTokenFile, result.Credentials.SSHHostID)
			return result.Credentials, nil
		}

		var rejectedErr *login.TokenRejectedError
		if errors.As(err, &rejectedErr) {
			rejectedHash = hashToken(enrolmentToken)
			saveRejectedTokenHash(rejectedHash)
			d.logger.Printf("enrolment token in %s refused (%v); not presenting it again", d.tunnelConfig.EnrolmentTokenFile, err)
			continue
		}

		wait := retryDelay.next()
		var retryableErr *login.RetryableError
		if errors.As(err, &retryableErr) && retryableErr.RetryAfter > wait {
			wait = min(retryableErr.RetryAfter, maxRetryAfter)
		}
		d.logger.Printf("token-file login failed (%v); retrying in %s", err, wait.Round(time.Second))
		if err := sleepContext(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func hashToken(enrolmentToken string) string {
	sum := sha256.Sum256([]byte(enrolmentToken))
	return hex.EncodeToString(sum[:])
}

func rejectedTokenPath() string {
	return filepath.Join(credstore.StateDir(), rejectedTokenFileName)
}

const bootIDPath = "/proc/sys/kernel/random/boot_id"

// currentBootID is "" where there is no boot id (not Linux); the refusal is then kept in memory only.
func currentBootID() string {
	data, err := os.ReadFile(bootIDPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// The record is "<boot_id> <hash>" so a reboot always gets one fresh try: a record from another boot is ignored and
// removed. It lives in the state directory because the daemon cannot write the root-owned /run/agentparley.
func loadRejectedTokenHash() string {
	bootID := currentBootID()
	if bootID == "" {
		return ""
	}
	data, err := os.ReadFile(rejectedTokenPath())
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || fields[0] != bootID {
		_ = os.Remove(rejectedTokenPath())
		return ""
	}
	return fields[1]
}

func saveRejectedTokenHash(hash string) {
	bootID := currentBootID()
	if bootID == "" {
		return
	}
	if err := os.MkdirAll(credstore.StateDir(), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(rejectedTokenPath(), []byte(bootID+" "+hash+"\n"), 0o600)
}
