package git

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// MinVersion is the oldest git Shepherd supports. The push block hands git its
// configuration through GIT_CONFIG_COUNT, GIT_CONFIG_KEY_n and GIT_CONFIG_VALUE_n, which
// git ignores without a word before 2.31, and so does `rev-parse --path-format`. On an
// older git the block would silently not apply.
var MinVersion = Version{2, 31, 0}

// Version is a git version: major, minor and patch. A fourth number or a vendor suffix
// ("2.34.1.windows.1", "2.39.2 (Apple Git-143)") does not take part in the comparison.
type Version [3]int

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool {
	for i := range v {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

var versionRE = regexp.MustCompile(`^git version (\d+)\.(\d+)(?:\.(\d+))?`)

// ParseVersion reads the output of `git --version`.
func ParseVersion(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, fmt.Errorf("cannot read a git version from %q", strings.TrimSpace(s))
	}
	var v Version
	for i := range v {
		if m[i+1] != "" {
			v[i], _ = strconv.Atoi(m[i+1])
		}
	}
	return v, nil
}

// CheckVersion runs `git --version` and returns an error naming the version found and
// the one required when git is missing, unreadable or older than min.
func CheckVersion(ctx context.Context, min Version) error {
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		return fmt.Errorf("git is required (version %s or newer) but could not be run: %v", min, err)
	}
	v, err := ParseVersion(string(out))
	if err != nil {
		return fmt.Errorf("git %s or newer is required: %v", min, err)
	}
	if v.Less(min) {
		return fmt.Errorf("git %s is too old: Shepherd needs git %s or newer (older git silently ignores the environment config that blocks an agent's pushes)", v, min)
	}
	return nil
}
