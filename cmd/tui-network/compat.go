package main

import (
	"context"

	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/manifest"
	tuinetwork "github.com/tui-tools/tui-network"
)

// probeCompat reads the version of the systemd this tool is about to drive.
//
// The facts it is judged against — the minimum version, the versions the lab
// has actually run against, the caveats that apply to a range, and which read
// paths exist on which systemd — come from the repository's own tool.json,
// embedded in the binary, so there is no second copy of them in the code.
//
// It never fails: a manifest that cannot be parsed and a missing binary both
// produce the zero Result, whose capability set answers yes to everything —
// which is the right default, because a backend that cannot do what was asked
// refuses in its own words, and that is a better message than a view hidden
// over an unreadable version string.
func probeCompat(ctx context.Context, demo bool) compat.Result {
	// --demo drives an in-memory machine; probing the real systemd on the
	// host would report a version that has nothing to do with what is on
	// screen.
	if demo {
		return compat.Result{}
	}
	m, err := manifest.Load(tuinetwork.ManifestJSON)
	if err != nil {
		return compat.Result{}
	}
	backend, ok := m.Backend(backendName)
	if !ok {
		return compat.Result{}
	}
	return compat.Probe(ctx, backend)
}
