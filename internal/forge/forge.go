// Package forge reads what a forge knows about a lane's branch: its merge request, the
// request's pipeline, the jobs that failed and their logs. Proof comes from here: a lane
// is merged when the forge has a merge commit for it, not when an agent says so.
//
// Forges are reached through their own CLIs (glab, gh) and the person's login in them,
// so Shepherd keeps no forge credentials.
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
)

// MR is a merge (or pull) request.
type MR struct {
	IID   int    `json:"iid"`
	State string `json:"state"` // opened, merged, closed, locked
	// SHA is the request's head; MergeSHA the commit that landed it (merge or squash).
	SHA      string    `json:"sha"`
	MergeSHA string    `json:"merge_sha,omitempty"`
	URL      string    `json:"url"`
	Pipeline *Pipeline `json:"pipeline,omitempty"`
}

// Pipeline is a request's head pipeline.
type Pipeline struct {
	ID     int64  `json:"id"`
	Status string `json:"status"` // running, pending, success, failed, canceled, skipped, ...
	SHA    string `json:"sha"`
	URL    string `json:"url"`
}

// Job is one failed job of a pipeline.
type Job struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Forge answers for one repository.
type Forge interface {
	Name() string
	// MRForBranch is the most recently updated request from the branch, or nil.
	MRForBranch(ctx context.Context, branch string) (*MR, error)
	FailedJobs(ctx context.Context, pipeline int64) ([]Job, error)
	// JobLog is the end of a job's log, cleaned for reading.
	JobLog(ctx context.Context, job int64, lines int) (string, error)
	// CreateMR opens a merge request from source into target.
	CreateMR(ctx context.Context, source, target, title, description string) (*MR, error)
	// RefPipeline is the latest pipeline for a branch or tag, or nil.
	RefPipeline(ctx context.Context, ref string) (*Pipeline, error)
}

// Remote is a git remote URL split into host and repository path.
type Remote struct {
	Host, Path string
}

var scpLike = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// ParseRemote splits a remote URL: git@host:group/repo.git, ssh://git@host:22/group/repo,
// https://host/group/repo.git.
func ParseRemote(remote string) (Remote, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return Remote{}, fmt.Errorf("no remote")
	}
	var r Remote
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return r, err
		}
		r = Remote{Host: u.Hostname(), Path: strings.TrimPrefix(u.Path, "/")}
	} else if m := scpLike.FindStringSubmatch(remote); m != nil {
		r = Remote{Host: m[1], Path: m[2]}
	} else {
		return r, fmt.Errorf("remote %q: not a URL Shepherd can read", remote)
	}
	r.Path = strings.TrimSuffix(strings.TrimSuffix(r.Path, "/"), ".git")
	if r.Host == "" || r.Path == "" {
		return r, fmt.Errorf("remote %q: no host or path", remote)
	}
	return r, nil
}

// For returns the forge for a remote. GitHub is recognised but not read yet (it is the
// mirror's forge in v1); any other host is taken to be GitLab.
func For(remote string) (Forge, error) {
	r, err := ParseRemote(remote)
	if err != nil {
		return nil, err
	}
	if r.Host == "github.com" {
		return nil, fmt.Errorf("GitHub as a lane's forge is not supported yet (%s)", r.Path)
	}
	return &GitLab{Host: r.Host, Project: r.Path}, nil
}

// GitLab reads a GitLab project through glab.
type GitLab struct {
	Host, Project string
	// Run runs glab and returns its stdout; tests replace it.
	Run func(ctx context.Context, args ...string) ([]byte, error)
}

func (g *GitLab) Name() string { return "gitlab" }

func (g *GitLab) api(ctx context.Context, path string, v any) error {
	run := g.Run
	if run == nil {
		run = glab
	}
	out, err := run(ctx, "api", "--hostname", g.Host, path)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("glab api %s: %w", path, err)
	}
	return nil
}

func glab(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "glab", args...)
	var errb strings.Builder
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return nil, fmt.Errorf("glab %s: %s", strings.Join(args, " "), msg)
		}
		return nil, fmt.Errorf("glab %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func (g *GitLab) project() string { return "projects/" + url.PathEscape(g.Project) }

type glMR struct {
	IID          int    `json:"iid"`
	State        string `json:"state"`
	SHA          string `json:"sha"`
	MergeCommit  string `json:"merge_commit_sha"`
	SquashCommit string `json:"squash_commit_sha"`
	WebURL       string `json:"web_url"`
	HeadPipeline *struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		SHA    string `json:"sha"`
		WebURL string `json:"web_url"`
	} `json:"head_pipeline"`
}

func (g *GitLab) MRForBranch(ctx context.Context, branch string) (*MR, error) {
	var list []glMR
	q := fmt.Sprintf("%s/merge_requests?source_branch=%s&state=all&order_by=updated_at&per_page=1", g.project(), url.QueryEscape(branch))
	if err := g.api(ctx, q, &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	// The list leaves the head pipeline out; the request itself has it.
	var one glMR
	if err := g.api(ctx, fmt.Sprintf("%s/merge_requests/%d", g.project(), list[0].IID), &one); err != nil {
		return nil, err
	}
	mr := &MR{IID: one.IID, State: one.State, SHA: one.SHA, URL: one.WebURL, MergeSHA: one.SquashCommit}
	if one.MergeCommit != "" {
		mr.MergeSHA = one.MergeCommit
	}
	if p := one.HeadPipeline; p != nil {
		mr.Pipeline = &Pipeline{ID: p.ID, Status: p.Status, SHA: p.SHA, URL: p.WebURL}
	}
	return mr, nil
}

func (g *GitLab) CreateMR(ctx context.Context, source, target, title, description string) (*MR, error) {
	run := g.Run
	if run == nil {
		run = glab
	}
	out, err := run(ctx, "api", "--hostname", g.Host, "--method", "POST", g.project()+"/merge_requests",
		"--raw-field", "source_branch="+source, "--raw-field", "target_branch="+target,
		"--raw-field", "title="+title, "--raw-field", "description="+description,
		"--raw-field", "remove_source_branch=true")
	if err != nil {
		return nil, err
	}
	var m glMR
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("create merge request: %w", err)
	}
	return &MR{IID: m.IID, State: m.State, SHA: m.SHA, URL: m.WebURL}, nil
}

func (g *GitLab) RefPipeline(ctx context.Context, ref string) (*Pipeline, error) {
	var list []struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		SHA    string `json:"sha"`
		WebURL string `json:"web_url"`
	}
	if err := g.api(ctx, fmt.Sprintf("%s/pipelines?ref=%s&order_by=id&sort=desc&per_page=1", g.project(), url.QueryEscape(ref)), &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	p := list[0]
	return &Pipeline{ID: p.ID, Status: p.Status, SHA: p.SHA, URL: p.WebURL}, nil
}

func (g *GitLab) FailedJobs(ctx context.Context, pipeline int64) ([]Job, error) {
	var jobs []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		WebURL string `json:"web_url"`
	}
	if err := g.api(ctx, fmt.Sprintf("%s/pipelines/%d/jobs?scope[]=failed", g.project(), pipeline), &jobs); err != nil {
		return nil, err
	}
	var out []Job
	for _, j := range jobs {
		out = append(out, Job{ID: j.ID, Name: j.Name, URL: j.WebURL})
	}
	return out, nil
}

func (g *GitLab) JobLog(ctx context.Context, job int64, lines int) (string, error) {
	run := g.Run
	if run == nil {
		run = glab
	}
	out, err := run(ctx, "api", "--hostname", g.Host, fmt.Sprintf("%s/jobs/%d/trace", g.project(), job))
	if err != nil {
		return "", err
	}
	return CleanLog(string(out), lines), nil
}

var (
	ansi      = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]|\x1b\][^\x07]*\x07`)
	runnerTS  = regexp.MustCompile(`^\d{4}-\d\d-\d\dT[\d:.]+Z \d+[OE]\+? ?`)
	sectionMk = regexp.MustCompile(`section_(start|end):\d+:\S+`)
)

// CleanLog strips terminal colours, the runner's timestamps and section markers from a
// job log and keeps its last lines: where the failure usually is.
func CleanLog(raw string, lines int) string {
	var keep []string
	for _, l := range strings.Split(raw, "\n") {
		l = ansi.ReplaceAllString(l, "")
		l = strings.ReplaceAll(l, "\r", "")
		l = runnerTS.ReplaceAllString(l, "")
		l = sectionMk.ReplaceAllString(l, "")
		if strings.TrimSpace(l) == "" {
			continue
		}
		keep = append(keep, l)
	}
	if len(keep) > lines {
		keep = keep[len(keep)-lines:]
	}
	return strings.Join(keep, "\n")
}

// unreachable are the words of errors that mean the host did not answer: DNS, TCP and
// TLS failures and timeouts, as Go's net package and glab (which prints them) word them.
var unreachable = regexp.MustCompile(`(?i)no such host|server misbehaving|temporary failure in name resolution|` +
	`connection refused|connection reset|network is unreachable|no route to host|host is down|` +
	`i/o timeout|tls handshake timeout|client\.timeout exceeded|deadline exceeded|` +
	`unexpected eof|broken pipe|dial tcp`)

// serverError and clientError are an HTTP 5xx or 4xx as glab reports it:
// "502 Bad Gateway (HTTP 502)".
var (
	serverError = regexp.MustCompile(`\bHTTP 5\d\d\b`)
	clientError = regexp.MustCompile(`\bHTTP 4\d\d\b`)
)

// Unreachable says whether err means the forge's host could not be reached or could not
// answer (a network failure, or a 5xx), as opposed to an answer that refused the request
// (a 4xx: no access, no such project), which waiting would not change.
func Unreachable(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	if clientError.MatchString(msg) {
		return false // an answer, whatever else the message says
	}
	return unreachable.MatchString(msg) || serverError.MatchString(msg)
}
