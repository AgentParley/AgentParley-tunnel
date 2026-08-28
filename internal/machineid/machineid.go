// Package machineid derives the stable, per-account-unique identifier the enrolment call sends as MachineId. The
// OS-specific raw-id source lives in the platform-suffixed file (machineid_linux.go today), per the design doc's
// "macOS later" note.
package machineid

import (
	"crypto/sha256"
	"encoding/hex"
)

// Get returns a stable id for this box, hashed together with serverHost so the SAME physical machine registered
// against two different servers (dev and prod, or two accounts' egress endpoints) produces two distinct ids —
// SshHost's unique (account_id, machine_id) index must never collide across genuinely different registrations.
func Get(serverHost string) (string, error) {
	rawID, err := readRawID()
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256([]byte(rawID + "|" + serverHost))
	return hex.EncodeToString(sum[:]), nil
}
