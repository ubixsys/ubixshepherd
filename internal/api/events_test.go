package api

import "testing"

func TestEventKindDeskRotated(t *testing.T) {
	if got := EventKind("desk_rotated"); got != EventDeskRotated {
		t.Fatalf("EventKind(desk_rotated) = %q, want %q", got, EventDeskRotated)
	}
	if got := EventKind("no_such_kind"); got != EventInfo {
		t.Fatalf("an unknown kind = %q, want info", got)
	}
}
