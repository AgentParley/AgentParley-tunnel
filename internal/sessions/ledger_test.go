package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsUsableRootRejectsUnsafeFolders(t *testing.T) {
	base := t.TempDir()

	good := filepath.Join(base, "good")
	if !isUsableRoot(good) {
		t.Fatal("a fresh 0700 folder owned by this user should be usable")
	}

	loose := filepath.Join(base, "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if isUsableRoot(loose) {
		t.Fatal("a folder others can read must be rejected")
	}

	link := filepath.Join(base, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if isUsableRoot(link) {
		t.Fatal("a symlink must be rejected")
	}

	if ownedPrivateDir(good, os.Getuid()+1) {
		t.Fatal("a folder owned by another uid must be rejected")
	}
}
