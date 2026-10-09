package daemon

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRotatingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	r, err := OpenRotating(path, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Each line is 6 bytes: one fits under the limit, two do not.
	for _, line := range []string{"aaaaa\n", "bbbbb\n", "ccccc\n", "ddddd\n", "eeeee\n", "fffff\n"} {
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	for file, want := range map[string]string{
		"daemon.log": "fffff\n", "daemon.log.1": "eeeee\n", "daemon.log.2": "ddddd\n", "daemon.log.3": "ccccc\n",
	} {
		if got := readFile(t, filepath.Join(filepath.Dir(path), file)); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Error("more than 3 old files kept")
	}
}

// A restart picks up the size the file already has, and a record is not split.
func TestRotatingFileReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	os.WriteFile(path, []byte("12345678"), 0o600)
	r, err := OpenRotating(path, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	r.Write([]byte("abcdef\n"))
	r.Close()
	if got := readFile(t, path+".1"); got != "12345678" {
		t.Errorf("old file = %q", got)
	}
	if got := readFile(t, path); got != "abcdef\n" {
		t.Errorf("new file = %q", got)
	}
	if _, err := r.Write([]byte("x")); err == nil {
		t.Error("write after close succeeded")
	}
}

// A record longer than the limit is still written whole.
func TestRotatingFileBigRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	r, _ := OpenRotating(path, 10, 2)
	defer r.Close()
	big := strings.Repeat("x", 50) + "\n"
	r.Write([]byte("hi\n"))
	r.Write([]byte(big))
	if got := readFile(t, path); got != big {
		t.Errorf("log = %q", got)
	}
}

func TestOpenLoggerRedactsAndTees(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	var tee bytes.Buffer
	log, closer, err := OpenLogger(path, &tee, slog.LevelInfo)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello", "token", "ghp_abcdefghijklmnopqrstuvwxyz0123456789")
	log.Debug("quiet")
	closer.Close()
	got := readFile(t, path)
	if !strings.Contains(got, "msg=hello") || strings.Contains(got, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("file = %q", got)
	}
	if strings.Contains(got, "quiet") || tee.String() != got {
		t.Errorf("tee = %q, file = %q", tee.String(), got)
	}
}
