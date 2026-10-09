package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// Roles a token can have. The operator token is the one in daemon.json, for the person
// and their own tools; worker and desk tokens are minted by the daemon and live only in
// its memory, so a restart revokes them all.
//
// Scoped tokens stop accidents and tool misuse: an agent's tools cannot answer its own
// decision or start another agent. They are not isolation: an agent running as the
// same OS user can read daemon.json and use the operator token. See design.md §3.17.
const (
	RoleOperator = "operator"
	// RoleWorker is an agent Shepherd started: one run, and only what the worker tools
	// need.
	RoleWorker = "worker"
	// RoleDesk is the front desk the daemon runs: the operator tools, but answering a
	// decision only in a turn the person started.
	RoleDesk = "desk"
)

// principal is who a request comes from.
type principal struct {
	Role string
	// Run is the run a worker token belongs to.
	Run int64
}

type principalKey struct{}

func principalOf(ctx context.Context) principal {
	p, _ := ctx.Value(principalKey{}).(principal)
	return p
}

// tokens are the scoped tokens minted since the daemon started, by the SHA-256 of the
// token: the raw value is handed out once and never kept.
type tokens struct {
	mu sync.Mutex
	by map[[32]byte]principal
}

func (t *tokens) mint(p principal) (string, error) {
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.by == nil {
		t.by = map[[32]byte]principal{}
	}
	t.by[sha256.Sum256([]byte(tok))] = p
	return tok, nil
}

func (t *tokens) lookup(tok string) (principal, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.by[sha256.Sum256([]byte(tok))]
	return p, ok
}

// revoke ends every token whose principal match says so.
func (t *tokens) revoke(match func(principal) bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, p := range t.by {
		if match(p) {
			delete(t.by, k)
		}
	}
}

// MintWorker returns a token for one run's worker tools (dispatch.Credentials).
func (s *Server) MintWorker(runID int64) (string, error) {
	if runID == 0 {
		return "", errors.New("a worker token needs a run")
	}
	return s.tokens.mint(principal{Role: RoleWorker, Run: runID})
}

// Revoke ends the run's worker tokens (dispatch.Credentials).
func (s *Server) Revoke(runID int64) {
	s.tokens.revoke(func(p principal) bool { return p.Role == RoleWorker && p.Run == runID })
}

// MintDesk returns a token for the front desk's operator tools.
func (s *Server) MintDesk() (string, error) {
	return s.tokens.mint(principal{Role: RoleDesk})
}

// RevokeDesk ends every desk token.
func (s *Server) RevokeDesk() {
	s.tokens.revoke(func(p principal) bool { return p.Role == RoleDesk })
}

// URL is the daemon's API base, once it listens (dispatch.Credentials).
func (s *Server) URL() string {
	if a := s.addr.Load(); a != nil {
		return "http://" + *a
	}
	return ""
}

// auth finds who a request is from: the operator token, or a scoped token minted since
// the daemon started.
func (s *Server) auth(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		var p principal
		if subtle.ConstantTimeCompare([]byte(got), want) == 1 {
			p = principal{Role: RoleOperator}
		} else if tok, ok := strings.CutPrefix(got, "Bearer "); ok && tok != "" {
			if p, ok = s.tokens.lookup(tok); !ok {
				p = principal{}
			}
		}
		if p.Role == "" {
			writeError(w, http.StatusUnauthorized, errors.New("missing, wrong or revoked token"))
			return
		}
		if sw, ok := w.(*statusWriter); ok {
			sw.role = p.Role
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// access is which scoped roles may call a route; the operator may call every one.
type access uint8

const (
	operatorOnly access = 0
	// forDesk: the front desk may call it.
	forDesk access = 1 << iota
	// forWorker: a worker may call it; the handler, or ownRun, keeps it to its own run.
	forWorker
	// ownRun: a worker only for the run named by the path's {id}.
	ownRun
)

// guard refuses a request from a role the route is not for, before its handler runs.
func guard(a access, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r.Context())
		switch p.Role {
		case RoleOperator:
		case RoleDesk:
			if a&forDesk == 0 {
				writeError(w, http.StatusForbidden, errors.New("the front desk's token cannot do this; it is the person's"))
				return
			}
		case RoleWorker:
			if a&(forWorker|ownRun) == 0 {
				writeError(w, http.StatusForbidden, errors.New("an agent's token cannot do this; ask the person (ask_human) or another lane (ask_shepherd)"))
				return
			}
			if a&ownRun != 0 {
				if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err != nil || id != p.Run {
					writeError(w, http.StatusForbidden, fmt.Errorf("an agent's token is for run %d only", p.Run))
					return
				}
			}
		default:
			writeError(w, http.StatusUnauthorized, errors.New("missing, wrong or revoked token"))
			return
		}
		h(w, r)
	}
}

// workerLane is the lane of the run a worker's token is for, or nil when the request is
// not from a worker.
func (s *Server) workerLane(r *http.Request) (*store.Lane, error) {
	p := principalOf(r.Context())
	if p.Role != RoleWorker {
		return nil, nil
	}
	run, err := s.Store.Run(r.Context(), p.Run)
	if err != nil {
		return nil, err
	}
	l, err := s.Store.Lane(r.Context(), run.LaneID)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// clientName is what the request's log line names the caller: its role, and the name it
// gave itself.
func clientName(r *http.Request) string {
	c := r.Header.Get(api.ClientHeader)
	if c == "" {
		c = "unknown"
	}
	return c
}

// deskTurnIsHuman says whether the front desk's turn in progress was started by the
// person. With no desk running, no turn is.
func (s *Server) deskTurnIsHuman() bool {
	if f := s.deskHuman.Load(); f != nil {
		return (*f)()
	}
	return false
}

// personsAnswer guards answering a decision. An answer is the person's: the front desk
// records one only in a turn they started, never in one Shepherd started to tell it
// about the swarm.
func (s *Server) personsAnswer(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if principalOf(r.Context()).Role == RoleDesk && !s.deskTurnIsHuman() {
			writeError(w, http.StatusForbidden, errors.New("the front desk may record an answer only in a turn the person started; bring the decision to them"))
			return
		}
		h(w, r)
	}
}
