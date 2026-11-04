package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// HasProgramEnv reports whether any relay program carries an env value, i.e. whether config.yaml holds secrets.
func (relay RelayConfig) HasProgramEnv() bool {
	for _, program := range relay.Programs {
		if len(program.Env) > 0 {
			return true
		}
	}
	return false
}

// SecureFile makes path root:<runAs group> 0640. Only root can chown, so it is a no-op error for anyone else.
func SecureFile(path string, gid int) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("securing %s needs root", path)
	}
	if err := os.Chown(path, 0, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o640)
}

// CheckGroupPrivate returns an error when other users can read a root:<gid> 0640 file: the group is the distro's
// shared "users" group, lists members besides runAs, or is some other account's primary group.
func CheckGroupPrivate(runAs string, gid int) error {
	return checkGroupPrivate("/etc/group", "/etc/passwd", runAs, gid)
}

func checkGroupPrivate(groupPath, passwdPath, runAs string, gid int) error {
	groups, err := os.Open(groupPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", groupPath, err)
	}
	defer groups.Close()
	scanner := bufio.NewScanner(groups)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < 4 {
			continue
		}
		if id, err := strconv.Atoi(fields[2]); err != nil || id != gid {
			continue
		}
		if fields[0] == "users" {
			return fmt.Errorf("group %q is shared with other users", fields[0])
		}
		for _, member := range strings.Split(fields[3], ",") {
			if member = strings.TrimSpace(member); member != "" && member != runAs {
				return fmt.Errorf("group %q also has member %q", fields[0], member)
			}
		}
	}
	passwd, err := os.Open(passwdPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", passwdPath, err)
	}
	defer passwd.Close()
	scanner = bufio.NewScanner(passwd)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < 4 || fields[0] == runAs {
			continue
		}
		if id, err := strconv.Atoi(fields[3]); err == nil && id == gid {
			return fmt.Errorf("group %d is also the primary group of user %q", gid, fields[0])
		}
	}
	return nil
}
