package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

func TestWebSessions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, h := range []string{"aa", "bb", "cc"} {
		err := db.SaveWebSession(ctx, store.WebSession{Hash: h, CSRF: "c" + h, Port: "9",
			Created: now.Add(time.Duration(i) * time.Second), LastSeen: now,
			Expires: now.Add(time.Hour * time.Duration(i))})
		if err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	db, err = Open(ctx, path) // a restart
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// "aa" expires at now: pruned.
	rows, err := db.LiveWebSessions(ctx, now)
	if err != nil || len(rows) != 2 || rows[0].Hash != "bb" || rows[0].CSRF != "cbb" || rows[0].Port != "9" {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if err := db.TouchWebSession(ctx, "bb", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteWebSession(ctx, "cc"); err != nil {
		t.Fatal(err)
	}
	rows, _ = db.LiveWebSessions(ctx, now)
	if len(rows) != 1 || !rows[0].LastSeen.Equal(now.Add(time.Minute)) {
		t.Fatalf("rows = %+v", rows)
	}
	if n, err := db.DeleteWebSessions(ctx); err != nil || n != 1 {
		t.Errorf("delete all = %d, %v", n, err)
	}
}
