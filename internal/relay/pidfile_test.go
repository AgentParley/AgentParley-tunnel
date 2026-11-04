package relay

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnedPrivateRejectsLooseModes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "123")
	if err := os.WriteFile(file, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ownedPrivate(file, false) {
		t.Fatal("0600 own file should pass")
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if ownedPrivate(file, false) {
		t.Fatal("group/other readable file must fail")
	}
	if ownedPrivate(file, true) {
		t.Fatal("a file is not a directory")
	}
}
