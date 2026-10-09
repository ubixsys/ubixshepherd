package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/ubixsys/ubixshepherd/internal/redact"
)

// ParseRuntime reads a daemon.json. Its errors never quote the content, because the
// content holds the access token.
func ParseRuntime(b []byte) (RuntimeInfo, error) {
	var rt RuntimeInfo
	if len(strings.TrimSpace(string(b))) == 0 {
		return rt, errors.New("is empty")
	}
	if err := json.Unmarshal(b, &rt); err != nil {
		var syn *json.SyntaxError
		var typ *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syn):
			return RuntimeInfo{}, fmt.Errorf("is not valid JSON (error at byte %d)", syn.Offset)
		case errors.As(err, &typ):
			return RuntimeInfo{}, fmt.Errorf("has a %q field of the wrong type", typ.Field)
		}
		return RuntimeInfo{}, errors.New("could not be read")
	}
	if rt.Addr == "" {
		return RuntimeInfo{}, errors.New("has no address")
	}
	if rt.Token == "" {
		return RuntimeInfo{}, errors.New("has no token")
	}
	return rt, nil
}

// scrub removes the token, and anything else that looks like a credential, from text
// that came from a remote machine before it is shown or wrapped in an error.
func scrub(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, "[token]")
	}
	return redact.String(s)
}

// tokenGuess pulls the token out of runtime-file text that may not parse, so a failure
// message can still be scrubbed of it.
func tokenGuess(out []byte) string {
	if m := tokenField.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	// Not JSON at all: treat the longest word as the secret, which costs a little
	// readability in an error and never leaks.
	best := ""
	for _, w := range strings.FieldsFunc(string(out), func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) {
		if len(w) >= 12 && len(w) > len(best) {
			best = w
		}
	}
	return best
}

var tokenField = regexp.MustCompile(`"token"\s*:\s*"([^"]+)"`)

// detail formats ssh's stderr for the end of an error message.
func detail(stderr, token string) string {
	s := strings.TrimSpace(scrub(stderr, token))
	if s == "" {
		return ""
	}
	const max = 600
	if len(s) > max {
		s = s[:max] + "..."
	}
	return ": " + strings.ReplaceAll(s, "\n", "; ")
}

// classify turns a failed `ssh dest cat daemon.json` into a message that says what to do.
// ssh exits 255 for its own failures and passes through the remote command's status
// otherwise.
func (s *SSH) classify(err error, stderr, token string) error {
	low := strings.ToLower(stderr)
	host := s.Target.Label()
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return errors.New("ssh is not installed or not on PATH; remote hosts are reached with ssh")
	case hostKeyChanged(low):
		return fmt.Errorf("the ssh host key for %s has changed: someone may be intercepting the connection, or the host was reinstalled. "+
			"Check with its owner; if the change is expected, remove the old key with `ssh-keygen -R <host>` and connect once with ssh to accept the new one", host)
	case strings.Contains(low, "host key verification failed"):
		return fmt.Errorf("ssh does not trust %s yet (its host key is not in known_hosts): connect once with `ssh %s` and check the fingerprint it shows", host, s.Target.Dest)
	case strings.Contains(low, "permission denied") || strings.Contains(low, "too many authentication failures"):
		return fmt.Errorf("ssh could not authenticate to %s (no password prompts here: use a key, or ssh-agent): check that `ssh %s` works from this shell%s", host, s.Target.Dest, detail(stderr, token))
	case code == 255 || code == -1:
		return fmt.Errorf("cannot reach %s over ssh%s", host, detail(stderr, token))
	case strings.Contains(low, "no such file"):
		return s.noDaemon()
	}
	return fmt.Errorf("reading daemon.json on %s failed (ssh exit %d)%s", host, code, detail(stderr, token))
}

func (s *SSH) noDaemon() error {
	return fmt.Errorf("no daemon.json on %s: is the daemon running there? try `ssh %s shepherd daemon status` (or set \"home\" for this host if its Shepherd home is not %s)",
		s.Target.Label(), s.Target.Dest, s.Target.Home)
}

// classifyTunnel does the same for an ssh that was to hold the tunnel and ended.
func (s *SSH) classifyTunnel(err error, stderr, token string) error {
	low := strings.ToLower(stderr)
	host := s.Target.Label()
	switch {
	case strings.Contains(low, "address already in use") || strings.Contains(low, "cannot listen") ||
		strings.Contains(low, "could not request local forwarding") || strings.Contains(low, "bind "):
		return &bindError{fmt.Sprintf("the ssh tunnel to %s could not bind its local port%s", host, detail(stderr, token))}
	case strings.Contains(low, "administratively prohibited") || strings.Contains(low, "forwarding is disabled"):
		return fmt.Errorf("the ssh server on %s does not allow port forwarding (AllowTcpForwarding); Shepherd needs it to reach the daemon%s", host, detail(stderr, token))
	}
	return s.classify(err, stderr, token)
}

// hostKeyChanged matches the warning ssh prints when a known host presents another key.
func hostKeyChanged(low string) bool {
	return strings.Contains(low, "remote host identification has changed") ||
		strings.Contains(low, "host key for") && strings.Contains(low, "has changed")
}
