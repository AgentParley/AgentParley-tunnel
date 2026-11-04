package relay

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/agentparley/tunnel/internal/shellrun"
)

// ResolveCommand finds a bare command name (npx) once, when the owner runs `program add`, so the saved config holds
// a full path. Search order: the PATH given with --env, the invoking process's PATH, then the usual per-user and
// system install folders (sudo's secure_path often hides the first ones).
func ResolveCommand(name, envPath, processPath, home string) (string, error) {
	var dirs []string
	dirs = append(dirs, filepath.SplitList(envPath)...)
	dirs = append(dirs, filepath.SplitList(processPath)...)
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".local/bin"), filepath.Join(home, ".npm-global/bin"))
		dirs = append(dirs, newestNodeBin(home)...)
	}
	dirs = append(dirs, "/usr/local/bin")
	for _, dir := range dirs {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%q was not found; give its full path (run `command -v %s` as yourself)", name, name)
}

// DefaultProgramPath is the PATH saved for a program that was not given one: its own folder first, so a
// `#!/usr/bin/env node` script finds node next to it.
func DefaultProgramPath(resolvedCommand string) string {
	return filepath.Dir(resolvedCommand) + ":" + shellrun.DefaultPath
}

func newestNodeBin(home string) []string {
	versions, _ := filepath.Glob(filepath.Join(home, ".nvm/versions/node/*/bin"))
	sort.Slice(versions, func(i, j int) bool { return versionLess(versions[j], versions[i]) })
	return versions
}

// versionLess compares the vMAJOR.MINOR.PATCH segment of two nvm bin paths numerically.
func versionLess(a, b string) bool {
	left, right := versionParts(a), versionParts(b)
	for index := 0; index < len(left) && index < len(right); index++ {
		if left[index] != right[index] {
			return left[index] < right[index]
		}
	}
	return len(left) < len(right)
}

func versionParts(path string) []int {
	segment := filepath.Base(filepath.Dir(path))
	var parts []int
	for _, piece := range strings.Split(strings.TrimPrefix(segment, "v"), ".") {
		number, _ := strconv.Atoi(piece)
		parts = append(parts, number)
	}
	return parts
}
