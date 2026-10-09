package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWithin(t *testing.T) {
	base := filepath.FromSlash("/work/git")
	cases := []struct {
		p    string
		want bool
	}{
		{"/work/git", true},
		{"/work/git/repo", true},
		{"/work/git/repo/sub/dir", true},
		{"/work/gitlab", false},
		{"/work", false},
		{"/other", false},
		{"/work/git/../elsewhere", false},
		{"/work/git/..repo", true},
	}
	for _, c := range cases {
		if got := Within(base, filepath.Clean(filepath.FromSlash(c.p))); got != c.want {
			t.Errorf("Within(%q, %q) = %v, want %v", base, c.p, got, c.want)
		}
	}
}

func TestCanonicalResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	a, err := Canonical(link)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonical(real)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("Canonical(link) = %q, Canonical(real) = %q", a, b)
	}
}

func TestCanonicalMissingPath(t *testing.T) {
	p, err := Canonical(filepath.Join(t.TempDir(), "not", "yet"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p) {
		t.Errorf("Canonical returned relative %q", p)
	}
}

func TestHomeOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(HomeEnv, dir)
	h, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	if h != dir {
		t.Errorf("Home() = %q, want %q", h, dir)
	}
}

func TestHomeDefault(t *testing.T) {
	t.Setenv(HomeEnv, "")
	h, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	user, _ := os.UserHomeDir()
	if h != filepath.Join(user, ".shepherd") {
		t.Errorf("Home() = %q, want ~/.shepherd", h)
	}
}

func TestLayoutFiles(t *testing.T) {
	l := Layout{Home: filepath.Join("h", "s")}
	for got, want := range map[string]string{
		l.Config(): "config.yaml", l.Store(): "shepherd.db", l.Log(): "daemon.log",
		l.Console(): "daemon.out", l.Runtime(): "daemon.json",
	} {
		if got != filepath.Join("h", "s", want) {
			t.Errorf("%s, want %s under the home", got, want)
		}
	}
}
