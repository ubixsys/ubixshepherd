package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
)

// FollowOptions tune how a followed stream reconnects. The zero value is sensible.
type FollowOptions struct {
	// MinBackoff and MaxBackoff bound the wait before reopening a dropped stream; the
	// wait doubles from the first up to the second and starts over after a stream that
	// delivered something. Defaults: 500ms and 15s.
	MinBackoff, MaxBackoff time.Duration
	// Idle is how long the stream may stay silent before it is taken for dead and
	// reopened. The daemon pings every 15 seconds. Default: 60s.
	Idle time.Duration
	// OnReconnect, when set, is told why a stream dropped, before the wait.
	OnReconnect func(err error)
}

func (o FollowOptions) withDefaults() FollowOptions {
	if o.MinBackoff <= 0 {
		o.MinBackoff = 500 * time.Millisecond
	}
	if o.MaxBackoff < o.MinBackoff {
		o.MaxBackoff = max(15*time.Second, o.MinBackoff)
	}
	if o.Idle <= 0 {
		o.Idle = 60 * time.Second
	}
	return o
}

// fatal marks a stream failure that retrying will not fix.
type fatal struct{ err error }

func (f fatal) Error() string { return f.err.Error() }
func (f fatal) Unwrap() error { return f.err }

// follow keeps a server-sent event stream open until ctx ends or handle fails. path
// builds the URL path and query to resume after an id; handle returns the id to
// resume from (0 keeps the last one: a piece with no id of its own). A dropped
// connection, a clean end of stream, a daemon that restarted (the client is redialed)
// and a busy daemon are retried with a backoff; any other refusal is returned.
func (c *Client) follow(ctx context.Context, path func(after int64) string, after int64, opt FollowOptions,
	handle func(id int64, event string, data []byte) (int64, error)) error {
	opt = opt.withDefaults()
	wait := opt.MinBackoff
	for {
		progressed := false
		err := c.stream(ctx, path(after), after, opt.Idle, func(id int64, event string, data []byte) error {
			progressed = true
			next, err := handle(id, event, data)
			if err != nil {
				return fatal{err}
			}
			if next > after {
				after = next
			}
			return nil
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var f fatal
		if errors.As(err, &f) {
			return f.err
		}
		if progressed {
			wait = opt.MinBackoff
		}
		if opt.OnReconnect != nil {
			opt.OnReconnect(err)
		}
		if errors.Is(err, ErrNoDaemon) {
			// The daemon restarted with a new address and token, or is not up yet.
			_ = c.Redial()
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		wait = min(wait*2, opt.MaxBackoff)
	}
}

// stream opens one connection and passes each event to fn until it ends. A nil return
// is a clean end; a fatal error is one not to retry.
func (c *Client) stream(ctx context.Context, path string, after int64, idle time.Duration,
	fn func(id int64, event string, data []byte) error) error {
	c.mu.RLock()
	base, token := c.base, c.token
	c.mu.RUnlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return fatal{err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	if after > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	if c.Name != "" {
		req.Header.Set(api.ClientHeader, c.Name)
	}
	// The client's own timeout covers a whole request: a stream has none.
	hc := &http.Client{}
	if c.http != nil {
		hc.Transport = c.http.Transport
	}
	resp, err := hc.Do(req)
	if err != nil {
		if c.host != "" {
			return &unreachable{host: c.host, err: err}
		}
		return fmt.Errorf("%w (%v)", ErrNoDaemon, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return &refused{host: c.host}
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("daemon: %s", errorText(resp))
	case resp.StatusCode/100 != 2:
		return fatal{fmt.Errorf("daemon: %s", errorText(resp))}
	}
	// The daemon pings every 15 seconds: a silent stream is a dead connection, and a
	// blocked read is only broken by ending the request.
	dog := time.AfterFunc(idle, cancel)
	defer dog.Stop()
	rd := bufio.NewReaderSize(resp.Body, 64*1024)
	var id int64
	var event string
	var data []string
	for {
		line, err := readLine(rd)
		if err != nil {
			if ctx.Err() != nil && err != io.EOF {
				return fmt.Errorf("stream silent for %s", idle)
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
		dog.Reset(idle)
		switch {
		case line == "":
			if len(data) > 0 {
				if err := fn(id, event, []byte(strings.Join(data, "\n"))); err != nil {
					return err
				}
			}
			id, event, data = 0, "", nil
		case strings.HasPrefix(line, ":"):
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "id":
				if n, err := strconv.ParseInt(value, 10, 64); err == nil {
					id = n
				}
			case "event":
				event = value
			case "data":
				data = append(data, value)
			}
		}
	}
}

// maxEventLine bounds one line of a stream, so a broken peer cannot fill memory.
const maxEventLine = 8 << 20

// readLine reads one line without its newline (and carriage return).
func readLine(r *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		part, isPrefix, err := r.ReadLine()
		if err != nil {
			return "", err
		}
		sb.Write(part)
		if sb.Len() > maxEventLine {
			return "", errors.New("stream line too long")
		}
		if !isPrefix {
			return sb.String(), nil
		}
	}
}

// errorText is the daemon's error message for a failed response, or its status.
func errorText(resp *http.Response) string {
	var e api.Error
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e) == nil && e.Error != "" {
		return e.Error
	}
	return resp.Status
}

func decodeEvent(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
