package version

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	info := func(v string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) { return &debug.BuildInfo{Main: debug.Module{Version: v}}, true }
	}
	none := func() (*debug.BuildInfo, bool) { return nil, false }
	for _, c := range []struct {
		stamped string
		read    func() (*debug.BuildInfo, bool)
		want    string
	}{
		{"v0.4.0", info("v0.3.0"), "v0.4.0"}, // the stamp wins
		{"dev", info("v0.3.0"), "v0.3.0"},    // go install ...@v0.3.0
		{"", info("v0.3.0"), "v0.3.0"},       // an empty stamp
		{"dev", info("(devel)"), "dev"},      // built from a checkout Go cannot version
		{"dev", info(""), "dev"},             // no module version
		{"dev", none, "dev"},                 // no build info at all
		{"", none, "dev"},
	} {
		if got := resolve(c.stamped, c.read); got != c.want {
			t.Errorf("resolve(%q) = %q, want %q", c.stamped, got, c.want)
		}
	}
}
