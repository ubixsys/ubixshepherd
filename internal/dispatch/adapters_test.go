package dispatch

import (
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

func TestAgeText(t *testing.T) {
	for age, want := range map[time.Duration]string{
		0: "today", 23 * time.Hour: "today", 24 * time.Hour: "1 day ago", 41*day + time.Hour: "41 days ago",
	} {
		if got := AgeText(age); got != want {
			t.Errorf("AgeText(%v) = %q, want %q", age, got, want)
		}
	}
}

func TestStandingLayersWorkspaceProjectRepo(t *testing.T) {
	p := &ProjectContext{Name: "shop", Brief: "Ship checkout.", Age: 41 * day, MaxAge: 30 * day,
		Caches: []Cache{{Path: "/build/llvm", Repo: "core", Recipe: "tools/build.sh", Note: "Rebuild it."}}}
	got := Standing("adapter note", "WS rules", p, "REPO rules")
	order := []string{"adapter note", "The workspace's rules: WS rules", "Project shop's brief (approved 41 days ago",
		"past the project's 30-day limit", "Ship checkout.", "build caches", "/build/llvm: made by tools/build.sh in repo core. Rebuild it.",
		"This repo's rules", "REPO rules"}
	at := -1
	for _, want := range order {
		i := strings.Index(got, want)
		if i < 0 || i < at {
			t.Fatalf("%q missing or out of order (after offset %d) in:\n%s", want, at, got)
		}
		at = i
	}
	if strings.Contains(got, "do not hand-patch") == false {
		t.Errorf("caches do not say to rebuild:\n%s", got)
	}
}

func TestStandingStaleBoundary(t *testing.T) {
	at := func(age time.Duration) string {
		return Standing("", "", &ProjectContext{Name: "p", Brief: "b", Age: age, MaxAge: 30 * day}, "")
	}
	if got := at(30 * day); strings.Contains(got, "limit") {
		t.Errorf("a brief exactly at its limit is stale:\n%s", got)
	}
	if got := at(30*day + time.Second); !strings.Contains(got, "limit") {
		t.Errorf("a brief past its limit is not marked:\n%s", got)
	}
}

func TestStandingWithoutProjectIsTheRepoRules(t *testing.T) {
	for _, p := range []*ProjectContext{nil, {Name: "p"}} {
		if got := Standing("note", "ignored workspace", p, "be kind"); got != "note\nThis repo's rules: be kind" {
			t.Errorf("standing = %q", got)
		}
	}
	if got := Standing("", "", nil, ""); got != "" {
		t.Errorf("standing = %q", got)
	}
}

func TestStandingRedactsTheBrief(t *testing.T) {
	got := Standing("", "", &ProjectContext{Name: "p", Brief: "use glpat-AbCdEfGhIjKlMnOpQrStUv here"}, "")
	if strings.Contains(got, "glpat-") {
		t.Errorf("brief not redacted:\n%s", got)
	}
}
