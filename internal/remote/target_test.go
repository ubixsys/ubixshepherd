package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDest(t *testing.T) {
	for _, tc := range []struct {
		in      string
		dest    string
		port    int
		wantErr string
	}{
		{in: "box", dest: "box"},
		{in: "me@box", dest: "me@box"},
		{in: "ssh://box", dest: "box"},
		{in: "ssh://me@box:2222", dest: "me@box", port: 2222},
		{in: "ssh://me@box:2222/", dest: "me@box", port: 2222},
		{in: "ssh://[::1]:22", dest: "[::1]", port: 22},
		{in: "ssh://me@[fe80::1]", dest: "me@[fe80::1]"},
		{in: "", wantErr: "empty"},
		{in: "-oProxyCommand=evil", wantErr: "cannot start with '-'"},
		{in: "ssh://-oProxyCommand=x", wantErr: "cannot start with '-'"},
		{in: "me@box x", wantErr: "whitespace"},
		{in: "https://box", wantErr: "only ssh://"},
		{in: "ssh://box:0", wantErr: "port"},
		{in: "ssh://box:99999", wantErr: "port"},
		{in: "ssh://box/path", wantErr: "want ssh://"},
		{in: "ssh://me:secret@box", wantErr: "password"},
		{in: "ssh://", wantErr: "want ssh://"},
	} {
		got, err := ParseDest(tc.in)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%q: err = %v, want %q", tc.in, err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Errorf("%q: error echoes the password: %v", tc.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got.Dest != tc.dest || got.Port != tc.port || got.Home != DefaultHome {
			t.Errorf("%q: got %+v, want dest %q port %d", tc.in, got, tc.dest, tc.port)
		}
	}
}

func writeHosts(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadHosts(t *testing.T) {
	p := writeHosts(t, `
default: build
hosts:
  build:
    ssh: me@build.example
    home: /srv/shepherd
    workspace: main
    control: true
  odd:
    ssh: ssh://me@odd.example:2200
`)
	h, err := LoadHosts(p)
	if err != nil {
		t.Fatal(err)
	}
	b, ok, err := h.Lookup("build")
	if err != nil || !ok {
		t.Fatalf("lookup build: %v %v", ok, err)
	}
	if b.Dest != "me@build.example" || b.Home != "/srv/shepherd" || b.Workspace != "main" || !b.Control || b.Name != "build" {
		t.Errorf("build = %+v", b)
	}
	o, _, _ := h.Lookup("odd")
	if o.Dest != "me@odd.example" || o.Port != 2200 || o.Home != DefaultHome || o.Control {
		t.Errorf("odd = %+v", o)
	}
	if _, ok, _ := h.Lookup("nope"); ok {
		t.Error("found a host that is not there")
	}
	if got := strings.Join(h.Names(), ","); got != "build,odd" {
		t.Errorf("names = %s", got)
	}
}

func TestLoadHostsProblems(t *testing.T) {
	if h, err := LoadHosts(filepath.Join(t.TempDir(), "absent.yaml")); err != nil || len(h.Hosts) != 0 {
		t.Errorf("missing file: %+v, %v", h, err)
	}
	if h, err := LoadHosts(writeHosts(t, "")); err != nil || h.Default != "" {
		t.Errorf("empty file: %+v, %v", h, err)
	}
	for name, body := range map[string]string{
		"unknown key":     "hosts:\n  a:\n    ssh: x\n    hme: y\n",
		"no ssh":          "hosts:\n  a:\n    home: y\n",
		"undefined deflt": "default: zzz\nhosts:\n  a:\n    ssh: x\n",
		"not yaml":        "hosts: [",
	} {
		if _, err := LoadHosts(writeHosts(t, body)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	h, _ := LoadHosts(writeHosts(t, "hosts:\n  bad:\n    ssh: -oFoo\n"))
	if _, _, err := h.Lookup("bad"); err == nil || !strings.Contains(err.Error(), `host "bad"`) {
		t.Errorf("bad dest: %v", err)
	}
}

func TestSelect(t *testing.T) {
	hosts := func() (Hosts, error) {
		return LoadHosts(writeHosts(t, "default: dflt\nhosts:\n  dflt:\n    ssh: d@dflt\n  other:\n    ssh: o@other\n"))
	}
	none := func() (Hosts, error) { return Hosts{}, nil }
	failing := func() (Hosts, error) { t.Error("hosts.yaml read when it need not be"); return Hosts{}, nil }
	for _, tc := range []struct {
		name            string
		flag, env       string
		local           bool
		hosts           func() (Hosts, error)
		wantOK          bool
		wantDest        string
		wantErrContains string
	}{
		{name: "nothing selected is local", hosts: none},
		{name: "flag by name", flag: "other", hosts: hosts, wantOK: true, wantDest: "o@other"},
		{name: "flag beats env", flag: "other", env: "dflt", hosts: hosts, wantOK: true, wantDest: "o@other"},
		{name: "env", env: "other", hosts: hosts, wantOK: true, wantDest: "o@other"},
		{name: "default from file", hosts: hosts, wantOK: true, wantDest: "d@dflt"},
		{name: "--local beats default", local: true, hosts: failing},
		{name: "literal url needs no file", flag: "ssh://me@x:22", hosts: failing, wantOK: true, wantDest: "me@x"},
		{name: "user@host literal", flag: "me@x", hosts: none, wantOK: true, wantDest: "me@x"},
		{name: "unknown name", flag: "zzz", hosts: hosts, wantErrContains: "have: dflt, other"},
		{name: "unknown name no file", flag: "zzz", hosts: none, wantErrContains: "ssh://"},
		{name: "bad literal", flag: "ssh://box:0", hosts: none, wantErrContains: "port"},
	} {
		got, ok, err := Select(tc.flag, tc.env, tc.local, tc.hosts)
		if tc.wantErrContains != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErrContains)
			}
			continue
		}
		if err != nil || ok != tc.wantOK || got.Dest != tc.wantDest {
			t.Errorf("%s: got %+v ok=%v err=%v", tc.name, got, ok, err)
		}
	}
}
