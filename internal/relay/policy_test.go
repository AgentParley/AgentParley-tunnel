package relay

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentparley/tunnel/internal/config"
)

const goodConfig = "enabled: true\nrun_as: agent\nrelay:\n  allow: [\"10.0.0.5:22\"]\n"

func TestPolicyFailsClosedOnBrokenConfigAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(text string, mtime time.Time) {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	write(goodConfig, base)
	initial, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	policy := NewPolicy(path, initial)
	if err := policy.CheckAddress("10.0.0.5", 22); err != nil {
		t.Fatalf("good config should allow: %v", err)
	}

	write("enabled: [unclosed", base.Add(time.Minute))
	if err := policy.CheckAddress("10.0.0.5", 22); err == nil {
		t.Fatal("a config that no longer parses must deny")
	}
	if err := policy.CheckAddress("10.0.0.5", 22); err == nil {
		t.Fatal("still broken on the second check (same mtime) must still deny")
	}

	write(goodConfig, base.Add(2*time.Minute))
	if err := policy.CheckAddress("10.0.0.5", 22); err != nil {
		t.Fatalf("a good read must restore the grants: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := policy.CheckAddress("10.0.0.5", 22); err == nil {
		t.Fatal("a missing config must deny")
	}
}
