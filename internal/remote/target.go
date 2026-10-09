// Package remote reaches a daemon on another machine. The daemon listens on loopback
// only, so the one transport today is an ssh tunnel to it; Transport is the seam where a
// second one (direct TLS, say) would plug in.
package remote

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// HostEnv selects a host for commands that were not given --host.
const HostEnv = "SHEPHERD_HOST"

// DefaultHome is where a daemon keeps its files when its environment does not say.
const DefaultHome = "~/.shepherd"

// Target is one machine reached over ssh.
type Target struct {
	// Name is how the person refers to it: a hosts.yaml key, or the destination itself.
	Name string
	// Dest is what ssh is given: user@host, or a host alias from ssh's own config.
	Dest string
	// Port overrides the port ssh would use; zero leaves it to ssh's configuration.
	Port int
	// Home is the daemon's Shepherd home on that machine, as its shell reads it.
	Home string
	// Workspace is the workspace commands act on by default, when none is named.
	Workspace string
	// Control shares one ssh connection between the commands run in a short while.
	Control bool
}

// Label names the target in messages, without anything secret.
func (t Target) Label() string {
	if t.Name != "" {
		return t.Name
	}
	return t.Dest
}

// ParseDest reads "[user@]host" or "ssh://[user@]host[:port]" into a Target.
func ParseDest(s string) (Target, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Target{}, errors.New("empty ssh destination")
	}
	t := Target{Name: s, Home: DefaultHome}
	if !strings.Contains(s, "://") {
		t.Dest = s
		return t, validate(t)
	}
	u, err := url.Parse(s)
	if err != nil {
		return Target{}, errors.New("bad host: not a valid ssh://[user@]host[:port]") // not echoed: it may hold a password
	}
	if u.Scheme != "ssh" {
		return Target{}, fmt.Errorf("bad host %q: only ssh:// is supported", s)
	}
	if u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return Target{}, fmt.Errorf("bad host %q: want ssh://[user@]host[:port]", s)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Target{}, fmt.Errorf("bad host %q: port %q is not 1-65535", s, p)
		}
		t.Port = n
	}
	host := u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // ssh takes a bracketed IPv6 literal after user@
	}
	t.Dest = host
	if u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			return Target{}, errors.New("bad host: a password has no place in a host; use ssh keys")
		}
		t.Dest = u.User.Username() + "@" + host
	}
	return t, validate(t)
}

// validate refuses a destination ssh would read as an option or that holds whitespace.
func validate(t Target) error {
	if strings.HasPrefix(t.Dest, "-") {
		return fmt.Errorf("bad host %q: a destination cannot start with '-'", t.Dest)
	}
	if strings.ContainsAny(t.Dest, " \t\r\n") {
		return fmt.Errorf("bad host %q: a destination cannot hold whitespace", t.Dest)
	}
	return nil
}

// Hosts is the content of hosts.yaml.
type Hosts struct {
	// Default names the host used when none is selected. --local overrides it.
	Default string               `yaml:"default"`
	Hosts   map[string]HostEntry `yaml:"hosts"`
}

// HostEntry is one named machine.
type HostEntry struct {
	// SSH is "user@host" or an ssh:// URL (for a port).
	SSH string `yaml:"ssh"`
	// Home is the daemon's Shepherd home there (default ~/.shepherd).
	Home string `yaml:"home"`
	// Workspace is the workspace to act on when none is named.
	Workspace string `yaml:"workspace"`
	// Control turns on ssh connection sharing (ControlMaster) for this host.
	Control bool `yaml:"control"`
}

// LoadHosts reads path. A missing file is an empty Hosts, not an error: most people have
// none. Unknown keys are rejected, so a typo does not silently lose a setting.
func LoadHosts(path string) (Hosts, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Hosts{}, nil
	}
	if err != nil {
		return Hosts{}, err
	}
	var h Hosts
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&h); err != nil && !errors.Is(err, io.EOF) {
		return Hosts{}, fmt.Errorf("%s: %v", path, err)
	}
	for name, e := range h.Hosts {
		if e.SSH == "" {
			return Hosts{}, fmt.Errorf("%s: host %q has no ssh destination", path, name)
		}
	}
	if h.Default != "" {
		if _, ok := h.Hosts[h.Default]; !ok {
			return Hosts{}, fmt.Errorf("%s: default host %q is not defined", path, h.Default)
		}
	}
	return h, nil
}

// Lookup returns the named host.
func (h Hosts) Lookup(name string) (Target, bool, error) {
	e, ok := h.Hosts[name]
	if !ok {
		return Target{}, false, nil
	}
	t, err := ParseDest(e.SSH)
	if err != nil {
		return Target{}, true, fmt.Errorf("host %q: %w", name, err)
	}
	t.Name, t.Workspace, t.Control = name, e.Workspace, e.Control
	if e.Home != "" {
		t.Home = e.Home
	}
	return t, true, nil
}

// Names lists the defined hosts, for messages.
func (h Hosts) Names() []string {
	var out []string
	for n := range h.Hosts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Select decides which target a command runs against. flag is --host, env is
// $SHEPHERD_HOST, local is --local. It returns ok=false when the command is local: no
// host was selected, which is exactly the behaviour without this package. A literal
// ssh://... or user@host is taken as it is; anything else must be a name in hosts.yaml.
func Select(flag, env string, local bool, hosts func() (Hosts, error)) (Target, bool, error) {
	if local {
		return Target{}, false, nil
	}
	sel := flag
	if sel == "" {
		sel = env
	}
	if sel == "" {
		h, err := hosts()
		if err != nil {
			return Target{}, false, err
		}
		if h.Default == "" {
			return Target{}, false, nil
		}
		t, _, err := h.Lookup(h.Default)
		return t, err == nil, err
	}
	if strings.Contains(sel, "://") {
		t, err := ParseDest(sel)
		return t, err == nil, err
	}
	h, err := hosts()
	if err != nil {
		return Target{}, false, err
	}
	t, ok, err := h.Lookup(sel)
	if err != nil {
		return Target{}, false, err
	}
	if ok {
		return t, true, nil
	}
	if strings.Contains(sel, "@") {
		t, err := ParseDest(sel)
		return t, err == nil, err
	}
	if len(h.Hosts) == 0 {
		return Target{}, false, fmt.Errorf("no host %q: %s does not exist or defines none (use ssh://[user@]host[:port] for a host without a name)", sel, "hosts.yaml")
	}
	return Target{}, false, fmt.Errorf("no host %q in hosts.yaml (have: %s)", sel, strings.Join(h.Names(), ", "))
}
