package cli

import (
	"os"
	"strconv"
	"strings"
)

// The daemon outlives the process that starts it, so it must not carry that process's
// per-run state. When an agent runs `make install`, `shepherd daemon restart` starts the
// daemon as the agent's child; unscrubbed, it inherits the run's push block (every push
// the daemon makes then fails, because the block rewrites the remote to a URL git cannot
// run) and the run's identity (SHEPHERD_TOKEN would become the daemon's own).
//
// This is the one list of what a daemon must not inherit.
var (
	// runEnvNames are per-run variables dropped by name.
	runEnvNames = []string{
		"SHEPHERD_RUN", "SHEPHERD_LANE", "SHEPHERD_CLIENT", "SHEPHERD_URL", "SHEPHERD_TOKEN",
		"CLAUDECODE",
		// The push block (internal/dispatch/pushblock.go) and git's own serialised form of
		// the same settings; GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n> go with the count.
		"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
	}
	// runEnvPrefixes are per-run variable families dropped by prefix (Claude Code's
	// session variables).
	runEnvPrefixes = []string{"CLAUDE_CODE_"}
)

// runEnvDrops lists the names in environ (KEY=value entries) that a daemon must not
// inherit. GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n> are dropped for every n below
// GIT_CONFIG_COUNT. A user's own GIT_CONFIG_GLOBAL and the like are kept.
func runEnvDrops(environ []string) map[string]bool {
	drop := map[string]bool{}
	count := 0
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if k == "GIT_CONFIG_COUNT" {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				count = n
			}
		}
		for _, p := range runEnvPrefixes {
			if strings.HasPrefix(k, p) {
				drop[k] = true
			}
		}
	}
	for _, k := range runEnvNames {
		drop[k] = true
	}
	for i := 0; i < count; i++ {
		n := strconv.Itoa(i)
		drop["GIT_CONFIG_KEY_"+n] = true
		drop["GIT_CONFIG_VALUE_"+n] = true
	}
	return drop
}

// scrubRunEnv returns environ without the per-run and push-block variables.
func scrubRunEnv(environ []string) []string {
	drop := runEnvDrops(environ)
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			out = append(out, kv)
		}
	}
	return out
}

// unsetRunEnv removes the same variables from this process's own environment.
func unsetRunEnv() {
	for k := range runEnvDrops(os.Environ()) {
		os.Unsetenv(k)
	}
}
