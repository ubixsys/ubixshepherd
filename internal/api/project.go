package api

import (
	"fmt"
	"net/url"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

// PathProjects names a project's brief routes.
const PathProjects = "/v1/projects"

// PathProjectBrief reads a project's brief (GET): the approved text with its age, and
// the open draft.
func PathProjectBrief(project string) string {
	return fmt.Sprintf("%s/%s/brief", PathProjects, url.PathEscape(project))
}

// PathProjectBriefDraft records a draft brief (POST).
func PathProjectBriefDraft(project string) string { return PathProjectBrief(project) + "/draft" }

// PathProjectBriefApprove approves the open draft (POST). It is the person's alone.
func PathProjectBriefApprove(project string) string { return PathProjectBrief(project) + "/approve" }

// DraftBrief is the body of POST /v1/projects/{name}/brief/draft. The text is redacted
// before it is stored.
type DraftBrief struct {
	Text string `json:"text"`
}

// ApproveBrief is the body of POST /v1/projects/{name}/brief/approve. Words are the
// person's own: their approval, typed by them. The daemon refuses an approval from an
// agent's token, and from the front desk outside a turn the person started, as it does
// an answer to a decision.
type ApproveBrief struct {
	// ID is the draft being approved; it must still be the project's open draft.
	ID    int64  `json:"id"`
	Words string `json:"words"`
}

// BriefView is a project's brief as the person and the front desk see it. Only
// Approved is ever given to an agent; Pending never is.
type BriefView struct {
	Project string `json:"project"`
	// Approved is the brief agents are given; nil when none has been approved.
	Approved *store.ProjectBrief `json:"approved,omitempty"`
	// Age is the time since approval, in nanoseconds as a Go duration; AgeText is the
	// same as agents read it ("approved 41 days ago").
	Age     time.Duration `json:"age,omitempty"`
	AgeText string        `json:"age_text,omitempty"`
	// Stale is set when the approved brief is older than MaxAge (brief_max_age).
	Stale  bool          `json:"stale,omitempty"`
	MaxAge time.Duration `json:"max_age"`
	// Pending is the open draft waiting for the person; nil when none.
	Pending *store.ProjectBrief `json:"pending,omitempty"`
	// Caches are what agents in the project are told about its build caches.
	Caches []BriefCache `json:"caches,omitempty"`
}

// BriefCache is a declared build cache as agents are told of it.
type BriefCache struct {
	Path   string `json:"path"`
	Repo   string `json:"repo"`
	Recipe string `json:"recipe"`
	Note   string `json:"note,omitempty"`
}
