package harness

import (
	"fmt"
	"regexp"
	"strconv"
)

var semverPattern = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// parseSemver extracts the first "major.minor.patch" run from versionOutput (CLI --version output commonly wraps it
// in extra text, e.g. "2.1.224 (Claude Code)") and returns it as a comparable triple.
func parseSemver(versionOutput string) (major, minor, patch int, err error) {
	match := semverPattern.FindStringSubmatch(versionOutput)
	if match == nil {
		return 0, 0, 0, fmt.Errorf("no semantic version found in %q", versionOutput)
	}
	major, _ = strconv.Atoi(match[1])
	minor, _ = strconv.Atoi(match[2])
	patch, _ = strconv.Atoi(match[3])
	return major, minor, patch, nil
}

// meetsFloor reports whether versionOutput's version is >= the floor major.minor.patch.
func meetsFloor(versionOutput string, floorMajor, floorMinor, floorPatch int) (bool, error) {
	major, minor, patch, err := parseSemver(versionOutput)
	if err != nil {
		return false, err
	}
	if major != floorMajor {
		return major > floorMajor, nil
	}
	if minor != floorMinor {
		return minor > floorMinor, nil
	}
	return patch >= floorPatch, nil
}
