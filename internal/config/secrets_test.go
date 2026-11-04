package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckGroupPrivate(t *testing.T) {
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	write := func(path, text string) {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(passwd, "root:x:0:0::/root:/bin/sh\nagent:x:1001:1001::/home/agent:/bin/sh\n")
	cases := []struct {
		name, group, passwd string
		wantErr             bool
	}{
		{"private", "agent:x:1001:\n", "", false},
		{"private with self member", "agent:x:1001:agent\n", "", false},
		{"other member", "agent:x:1001:agent,bob\n", "", true},
		{"users group", "users:x:100:\n", "", true},
		{"primary group of another user", "agent:x:1001:\n", "bob:x:1002:1001::/home/bob:/bin/sh\n", true},
	}
	for _, c := range cases {
		gid := 1001
		if c.name == "users group" {
			gid = 100
		}
		groupPath := filepath.Join(dir, "group")
		write(groupPath, c.group)
		passwdPath := passwd
		if c.passwd != "" {
			passwdPath = filepath.Join(dir, "passwd2")
			write(passwdPath, "agent:x:1001:1001::/home/agent:/bin/sh\n"+c.passwd)
		}
		err := checkGroupPrivate(groupPath, passwdPath, "agent", gid)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
	}
}
