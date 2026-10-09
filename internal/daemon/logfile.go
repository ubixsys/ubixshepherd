package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"

	"github.com/ubixsys/ubixshepherd/internal/redact"
)

// Log rotation: daemon.log is the daemon's own file, so it can be renamed under itself.
// A service manager that opened the file for the daemon's stderr could not be told to
// reopen it, which is why the manager's output goes to a separate file (see
// internal/service) and the daemon writes its log itself.
const (
	// LogMaxBytes is the size daemon.log reaches before it is rotated.
	LogMaxBytes = 10 << 20
	// LogKeep is how many rotated files are kept: daemon.log.1 (newest) to daemon.log.3.
	LogKeep = 3
)

// RotatingFile is a file that is renamed to path.1 when a write would take it past max
// bytes, path.1 to path.2, and so on, dropping the oldest of keep. A record is never
// split across two files.
type RotatingFile struct {
	path string
	max  int64
	keep int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// OpenRotating opens path for appending, picking up the size it already has.
func OpenRotating(path string, max int64, keep int) (*RotatingFile, error) {
	r := &RotatingFile{path: path, max: max, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, fi.Size()
	return nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		// A failed rotation keeps logging to the file we have rather than losing the line.
		if err := r.rotate(); err != nil {
			fmt.Fprintf(os.Stderr, "shepherd: rotate %s: %v\n", r.path, err)
		}
		if r.f == nil {
			return 0, os.ErrClosed
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate moves the files up one place and opens a fresh one. On Windows a file cannot be
// renamed over another, so each destination is removed first.
func (r *RotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	r.f = nil
	name := func(i int) string { return r.path + "." + strconv.Itoa(i) }
	os.Remove(name(r.keep))
	for i := r.keep - 1; i >= 1; i-- {
		os.Rename(name(i), name(i+1))
	}
	os.Remove(name(1))
	renameErr := os.Rename(r.path, name(1))
	if err := r.open(); err != nil {
		return err
	}
	return renameErr
}

// Close closes the file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// newLogger writes text records to w, redacted, at level, with an identical warning or
// error held back for RepeatWindow after it was logged.
func newLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(newDedupHandler(slog.NewTextHandler(redact.Writer(w), &slog.HandlerOptions{Level: level})))
}

// OpenLogger returns a logger that writes to a rotating file at path, and to also (a
// terminal, for a daemon run by hand) when it is not nil, at level (a *slog.LevelVar
// lets a reload change it). See newLogger. Close the returned closer when the daemon
// stops.
func OpenLogger(path string, also io.Writer, level slog.Leveler) (*slog.Logger, io.Closer, error) {
	f, err := OpenRotating(path, LogMaxBytes, LogKeep)
	if err != nil {
		return nil, nil, err
	}
	var w io.Writer = f
	if also != nil {
		w = io.MultiWriter(f, also)
	}
	return newLogger(w, level), f, nil
}
