package git

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]Version{
		"git version 2.34.1":                 {2, 34, 1},
		"git version 2.34.1\n":               {2, 34, 1},
		"git version 2.39.2 (Apple Git-143)": {2, 39, 2},
		"git version 2.34.1.windows.1":       {2, 34, 1},
		"git version 2.43.0.rc1":             {2, 43, 0},
		"git version 2.30":                   {2, 30, 0},
		"git version 2.10.0-rc0":             {2, 10, 0},
	} {
		got, err := ParseVersion(in)
		if err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "garbage", "git", "git version", "git version x.y", "version 2.34.1"} {
		if v, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) = %v, want an error", in, v)
		}
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b Version
		less bool
	}{
		{Version{2, 30, 9}, Version{2, 31, 0}, true},
		{Version{2, 31, 0}, Version{2, 31, 0}, false},
		{Version{2, 31, 1}, Version{2, 31, 0}, false},
		{Version{1, 99, 0}, Version{2, 0, 0}, true},
		{Version{3, 0, 0}, Version{2, 99, 9}, false},
		{Version{2, 9, 0}, Version{2, 31, 0}, true},
	} {
		if got := c.a.Less(c.b); got != c.less {
			t.Errorf("%v.Less(%v) = %v", c.a, c.b, got)
		}
	}
}

// fakeGit puts a git on PATH that prints out for --version.
func fakeGit(t *testing.T, out string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for git")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho '" + out + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestCheckVersionRejectsOldGit(t *testing.T) {
	fakeGit(t, "git version 2.30.2")
	err := CheckVersion(context.Background(), MinVersion)
	if err == nil {
		t.Fatal("git 2.30.2 passed")
	}
	if !strings.Contains(err.Error(), "2.30.2") || !strings.Contains(err.Error(), MinVersion.String()) {
		t.Errorf("error does not name both versions: %v", err)
	}
}

func TestCheckVersionAcceptsNewEnoughGit(t *testing.T) {
	fakeGit(t, "git version 2.39.2 (Apple Git-143)")
	if err := CheckVersion(context.Background(), MinVersion); err != nil {
		t.Errorf("git 2.39.2: %v", err)
	}
}

func TestCheckVersionNoGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := CheckVersion(context.Background(), MinVersion); err == nil {
		t.Error("no git on PATH passed")
	}
}

func TestCheckVersionGarbage(t *testing.T) {
	fakeGit(t, "hello")
	if err := CheckVersion(context.Background(), MinVersion); err == nil {
		t.Error("unreadable version passed")
	}
}
