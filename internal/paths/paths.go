// Package paths decides where Shepherd keeps its files on this machine, and compares
// filesystem paths the same way on every OS.
package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// HomeEnv overrides the Shepherd home directory, mainly for tests and for running more
// than one daemon on a machine.
const HomeEnv = "SHEPHERD_HOME"

// Home is the directory holding the config, the store and the daemon's runtime file:
// $SHEPHERD_HOME if set, otherwise ~/.shepherd, the same path on every OS.
func Home() (string, error) {
	if h := os.Getenv(HomeEnv); h != "" {
		return filepath.Abs(h)
	}
	base, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, ".shepherd"), nil
}

// Layout names the files under a home directory.
type Layout struct {
	Home string
}

func (l Layout) Config() string { return filepath.Join(l.Home, "config.yaml") }
func (l Layout) Store() string  { return filepath.Join(l.Home, "shepherd.db") }

// Log is where a background daemon writes its log.
func (l Layout) Log() string { return filepath.Join(l.Home, "daemon.log") }

// Console is where a detached daemon's stdout and stderr go, and where a service manager
// is told to put them: a panic, or an error before the daemon has opened its log. The
// daemon's own log (Log) is written, and rotated, by the daemon alone, because a file a
// manager or parent holds open cannot be renamed out from under it.
func (l Layout) Console() string { return filepath.Join(l.Home, "daemon.out") }

// Runtime is written by a running daemon: its address, pid and access token.
func (l Layout) Runtime() string { return filepath.Join(l.Home, "daemon.json") }

// Canonical returns p as an absolute, cleaned path with symlinks resolved, so that
// /tmp and /private/tmp on macOS, or a symlinked ~/git, compare equal. A path that does
// not exist yet is returned absolute and cleaned.
func Canonical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Clean(abs), nil
	}
	if err != nil {
		return "", err
	}
	return real, nil
}

// Within reports whether p is base or below it. Both must be canonical. Comparison is
// case-insensitive on Windows and macOS, whose default filesystems are.
func Within(base, p string) bool {
	if foldCase() {
		base, p = strings.ToLower(base), strings.ToLower(p)
	}
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func foldCase() bool { return runtime.GOOS == "windows" || runtime.GOOS == "darwin" }
