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

// probeDHCPCompat reads the version of the DHCP server this tool detected, the
// same way probeCompat reads systemd's — against the manifest backend block for
// the detected server, so the minimum and the notes come from tool.json rather
// than from a version number written here.
//
// It returns the zero Result under --demo and on a machine with no DHCP server:
// there is no real server to name a version for, and the zero Result answers
// "unknown", which the header renders as a plain backend name.
func probeDHCPCompat(ctx context.Context, demo bool, dhcpBackendName string) compat.Result {
	if demo || dhcpBackendName == "" || dhcpBackendName == "dhcp" {
		return compat.Result{}
	}
	m, err := manifest.Load(tuinetwork.ManifestJSON)
	if err != nil {
		return compat.Result{}
	}
	backend, ok := m.Backend(dhcpBackendName)
	if !ok {
		return compat.Result{}
	}
	return compat.Probe(ctx, backend)
}
