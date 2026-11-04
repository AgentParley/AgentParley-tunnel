package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentparley/tunnel/internal/config"
	"github.com/agentparley/tunnel/internal/shellrun"
)

func TestAddressAllowed(t *testing.T) {
	allow := []string{"192.168.1.20:22", "NAS.local:22", "[::1]:3000"}
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"192.168.1.20", 22, true},
		{"192.168.1.20", 2222, false},
		{"192.168.1.21", 22, false},
		{"nas.LOCAL", 22, true},
		{"::1", 3000, true},
		{"[::1]", 3000, true},
		{"0:0:0:0:0:0:0:1", 3000, true},
		{"localhost", 3000, false},
		{"192.168.1.20 ", 22, true},
		{"192.168.1.0/24", 22, false},
		{"*", 22, false},
		{"", 22, false},
		{"192.168.1.20", 0, false},
	}
	for _, c := range cases {
		if got := AddressAllowed(allow, c.host, c.port); got != c.want {
			t.Errorf("AddressAllowed(%q, %d) = %v, want %v", c.host, c.port, got, c.want)
		}
	}
	if AddressAllowed(nil, "192.168.1.20", 22) {
		t.Error("an empty allow list must deny everything")
	}
}

func TestProgramNameValidation(t *testing.T) {
	for _, name := range []string{"notes", "a", "my-tool_2", "0abc"} {
		if err := config.ValidateProgramName(name); err != nil {
			t.Errorf("%q should be valid: %v", name, err)
		}
	}
	for _, name := range []string{"", "Notes", "-a", "_a", "a b", "a/b", "a;b", "../x", strings.Repeat("a", 65)} {
		if err := config.ValidateProgramName(name); err == nil {
			t.Errorf("%q should be rejected", name)
		}
	}
}

func TestRelayConfigValidation(t *testing.T) {
	bad := []config.RelayConfig{
		{Allow: []string{"nohostport"}},
		{Allow: []string{"host:0"}},
		{Allow: []string{"host:70000"}},
		{Programs: map[string]config.RelayProgram{"p": {Command: []string{"npx"}}}},
		{Programs: map[string]config.RelayProgram{"p": {}}},
		{Programs: map[string]config.RelayProgram{"p": {Command: []string{"/bin/true"}, Env: map[string]string{"A=B": "x"}}}},
		{MaxConcurrent: -1},
	}
	for index, relay := range bad {
		if err := relay.Validate(); err == nil {
			t.Errorf("case %d should be rejected", index)
		}
	}
}

func TestParseStartTime(t *testing.T) {
	stat := "123 (my (odd) name) S 1 123 123 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 100 18446744073709551615"
	got, err := parseStartTime(stat)
	if err != nil || got != "987654" {
		t.Fatalf("parseStartTime = %q, %v", got, err)
	}
}

func TestEditKeepsCommentsAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "# keep me\nrun_as: root\nenabled: true  # trailing\nread_only: false\n"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if changed, err := Allow(path, "192.168.1.20:22"); err != nil || !changed {
		t.Fatalf("Allow = %v, %v", changed, err)
	}
	if changed, _ := Allow(path, "192.168.1.20:22"); changed {
		t.Error("second Allow must be a no-op")
	}
	program := config.RelayProgram{Command: []string{"/usr/bin/true"}, Env: map[string]string{"A": "b"}}
	if _, err := AddProgram(path, "notes", program); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{"# keep me", "# trailing", "read_only: false"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("edited config lost %q:\n%s", want, raw)
		}
	}
	loaded, err := config.Load(path)
	if err != nil || len(loaded.Relay.Allow) != 1 || loaded.Relay.Programs["notes"].Command[0] != "/usr/bin/true" {
		t.Fatalf("round trip failed: %+v %v", loaded, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
	if _, err := Disallow(path, "192.168.1.20:22"); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveProgram(path, "notes"); err != nil {
		t.Fatal(err)
	}
	loaded, err = config.Load(path)
	if err != nil || len(loaded.Relay.Allow) != 0 || len(loaded.Relay.Programs) != 0 {
		t.Fatalf("removal failed: %+v %v", loaded, err)
	}
	if _, err := Allow(path, "bad"); err == nil {
		t.Error("Allow must validate")
	}
}

func TestProgramRunsWithoutShellAndIsStopped(t *testing.T) {
	user := &shellrun.User{Username: "tester", Home: t.TempDir(), Shell: "/bin/sh"}
	registry := NewPidFile(t.TempDir())
	started, refusal := StartProgram(user, config.RelayProgram{Command: []string{"/bin/cat"}}, registry)
	if refusal != nil {
		t.Fatal(refusal)
	}
	if _, err := started.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 16)
	count, _ := started.Read(buffer)
	if string(buffer[:count]) != "hello\n" {
		t.Fatalf("echo = %q", buffer[:count])
	}
	done := make(chan struct{})
	go func() { started.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not stop the program")
	}
	if entries, _ := os.ReadDir(filepath.Join(registry.dir)); len(entries) != 0 {
		t.Error("pid file entry was not removed")
	}

	if _, refusal := StartProgram(user, config.RelayProgram{Command: []string{"/nonexistent/prog"}}, registry); refusal == nil || refusal.Code != CodeProgramFailed {
		t.Errorf("missing binary should give program_failed, got %v", refusal)
	}
}
