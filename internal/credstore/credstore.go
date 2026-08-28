// Package credstore holds the daemon's durable identity: the refresh token and the Ed25519 possession-proof key.
// The OS-specific storage location and file modes live in the platform-suffixed file (store_linux.go today) so a
// future macOS/Windows port only has to add a sibling file, per the design doc's "macOS later" note.
package credstore

// Credentials is the daemon's entire durable identity, persisted after a successful enrolment and read back on
// every start.
type Credentials struct {
	RefreshToken string `json:"refreshToken"`
	PrivateKey   string `json:"privateKey"` // base64, Ed25519 seed (32 bytes)
	PublicKey    string `json:"publicKey"`  // base64, Ed25519 public key — matches SshHost.HostKeyFingerprint's pin
	RoutingKey   string `json:"routingKey"`
	SSHHostID    string `json:"sshHostId"`
	MachineID    string `json:"machineId"`
}

// Store persists Credentials to disk. Load returns (nil, nil) — not an error — when no credentials have been
// saved yet, since "never logged in" is an expected state for `agentparley-tunnel login` to detect.
type Store interface {
	Load() (*Credentials, error)
	Save(credentials *Credentials) error
	Delete() error
}
