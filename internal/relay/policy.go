package relay

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/agentparley/tunnel/internal/config"
)

const defaultMaxConcurrent = 128

// Policy answers "may this relay go there" from config.yaml, re-reading the file only when its mtime changes so
// `sudo agentparley allow` takes effect without a restart.
type Policy struct {
	path string

	mu       sync.Mutex
	modTime  time.Time
	current  *config.Config
	loggedAt time.Time
}

func NewPolicy(path string, initial *config.Config) *Policy {
	policy := &Policy{path: path, current: initial}
	if info, err := os.Stat(path); err == nil {
		policy.modTime = info.ModTime()
	}
	return policy
}

func (p *Policy) config() *config.Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	info, err := os.Stat(p.path)
	if err != nil {
		p.logOnce(err)
		return p.current
	}
	if info.ModTime().Equal(p.modTime) {
		return p.current
	}
	loaded, err := config.Load(p.path)
	if err != nil {
		p.modTime = info.ModTime()
		p.logOnce(err)
		return p.current
	}
	p.modTime = info.ModTime()
	p.current = loaded
	return p.current
}

// logOnce keeps a broken edit from flooding the journal: the last good copy stays in force and the error shows once.
func (p *Policy) logOnce(err error) {
	if time.Since(p.loggedAt) < time.Hour {
		return
	}
	p.loggedAt = time.Now()
	log.Printf("relay: keeping the previous config, could not re-read it: %v", err)
}

// CheckAddress returns nil when host:port may be relayed to.
func (p *Policy) CheckAddress(host string, port int) error {
	current := p.config()
	if !current.Enabled {
		return &Error{CodePolicyDenied, "this computer has enabled: false in its config"}
	}
	if !AddressAllowed(current.Relay.Allow, host, port) {
		return &Error{CodeRelayNotAllowed, fmt.Sprintf("%s is not in relay.allow on this computer", net.JoinHostPort(host, fmt.Sprint(port)))}
	}
	return nil
}

// Program returns the named program, or an error carrying the wire code.
func (p *Policy) Program(name string) (config.RelayProgram, error) {
	current := p.config()
	if !current.Enabled {
		return config.RelayProgram{}, &Error{CodePolicyDenied, "this computer has enabled: false in its config"}
	}
	program, ok := current.Relay.Programs[name]
	if !ok {
		return config.RelayProgram{}, &Error{CodeRelayProgramUnknown, fmt.Sprintf("no program named %q in relay.programs on this computer", name)}
	}
	return program, nil
}

// MaxConcurrent is the configured relay limit, or 128.
func (p *Policy) MaxConcurrent() int {
	if limit := p.config().Relay.MaxConcurrent; limit > 0 {
		return limit
	}
	return defaultMaxConcurrent
}

// AddressAllowed is the default-deny matcher: an exact host (case-insensitive, same IP in any spelling) and an
// exact port against one allow entry. There are no wildcards, ranges or subnets.
func AddressAllowed(allow []string, host string, port int) bool {
	host = normaliseHost(host)
	if host == "" || port < 1 || port > 65535 {
		return false
	}
	for _, entry := range allow {
		entryHost, entryPort, err := config.NormaliseAllowEntry(entry)
		if err != nil || entryPort != port {
			continue
		}
		if hostsEqual(normaliseHost(entryHost), host) {
			return true
		}
	}
	return false
}

func normaliseHost(host string) string {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return host
}

func hostsEqual(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	ipA, ipB := net.ParseIP(a), net.ParseIP(b)
	return ipA != nil && ipB != nil && ipA.Equal(ipB)
}
