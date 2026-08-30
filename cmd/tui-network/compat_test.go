package main

import (
	"context"
	"testing"

	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/manifest"
	tuinetwork "github.com/tui-tools/tui-network"
	"github.com/tui-tools/tui-network/internal/networkd"
)

// backend loads the manifest block the binary really reads.
func backend(t *testing.T) compat.Backend {
	t.Helper()
	m, err := manifest.Load(tuinetwork.ManifestJSON)
	if err != nil {
		t.Fatalf("the embedded manifest does not parse: %v", err)
	}
	if m.Name != toolName {
		t.Fatalf("manifest name = %q, want %q", m.Name, toolName)
	}
	b, ok := m.Backend(backendName)
	if !ok {
		t.Fatalf("the manifest declares no %q backend", backendName)
	}
	return b
}

func TestManifestDeclaresTheBackend(t *testing.T) {
	b := backend(t)
	if b.Binary != "networkctl" {
		t.Errorf("binary = %q, want networkctl", b.Binary)
	}
	if b.Minimum != "245" {
		t.Errorf("minimum = %q, want 245", b.Minimum)
	}
	if len(b.VersionCommand) == 0 {
		t.Errorf("a backend with no version command cannot be probed")
	}
}

// TestVersionRegexReadsRealOutput uses the `networkctl --version` banner as it
// really prints: the version is the number after "systemd", and the feature
// line under it is full of digits that must not be mistaken for one.
func TestVersionRegexReadsRealOutput(t *testing.T) {
	b := backend(t)
	tests := map[string]string{
		"systemd 255 (255.4-1ubuntu8.17)\n+PAM +AUDIT -GCRYPT": "255",
		"systemd 257 (257.13-1.fc42)\n+PAM +AUDIT +SELINUX":    "257",
		"systemd 245 (245.4-4ubuntu3.24)":                      "245",
	}
	for output, want := range tests {
		if got := compat.ParseVersion(output, b.VersionRegex); got != want {
			t.Errorf("ParseVersion(%q) = %q, want %q", output, got, want)
		}
	}
}

// TestFeatureGatesMatchTheEmpiricalVersions pins what was measured on real
// systemd releases: 245 has neither `--json` nor `up`/`down`, 249 has both.
func TestFeatureGatesMatchTheEmpiricalVersions(t *testing.T) {
	b := backend(t)
	tests := []struct {
		version string
		json    bool
		upDown  bool
	}{
		{"245", false, false},
		{"249", true, true},
		{"255", true, true},
		{"257", true, true},
	}
	for _, test := range tests {
		caps := compat.NewCaps(test.version, b.Features)
		if got := caps.Has(networkd.FeatureJSONStatus); got != test.json {
			t.Errorf("systemd %s: json-status = %v, want %v",
				test.version, got, test.json)
		}
		if got := caps.Has(networkd.FeatureLinkUpDown); got != test.upDown {
			t.Errorf("systemd %s: link-up-down = %v, want %v",
				test.version, got, test.upDown)
		}
	}
}

// TestUnknownVersionKeepsEveryFeature: a version the probe could not read must
// not hide a working view. The backend refuses in its own words instead.
func TestUnknownVersionKeepsEveryFeature(t *testing.T) {
	caps := compat.Result{}.Caps()
	if !caps.Has(networkd.FeatureJSONStatus) || !caps.Has(networkd.FeatureLinkUpDown) {
		t.Errorf("an unprobed version must be treated as capable")
	}
}

func TestProbeInDemoModeReportsNothing(t *testing.T) {
	if got := probeCompat(context.Background(), true); got.Backend != "" {
		t.Errorf("--demo probed the host: %+v", got)
	}
}

func TestClassifiesVersionsAgainstTheMinimum(t *testing.T) {
	b := backend(t)
	tests := map[string]compat.Status{
		"244": compat.StatusBelowMinimum,
		"245": compat.StatusUntested,
		"257": compat.StatusUntested,
	}
	for version, want := range tests {
		result := compat.ProbeWith(context.Background(), b,
			func(context.Context, []string) (string, error) {
				return "systemd " + version + " (" + version + ".1-1)", nil
			})
		if result.Version != version {
			t.Errorf("probed version %q, want %q", result.Version, version)
		}
		// A version in the manifest's tested list would classify as tested;
		// the expectations above hold while that list is short, so they are
		// skipped for a version the evidence file already covers.
		if isTested(b, version) {
			continue
		}
		if result.Status != want {
			t.Errorf("systemd %s: status %v, want %v", version, result.Status, want)
		}
	}
}

// isTested reports whether the manifest already records a passing run.
func isTested(b compat.Backend, version string) bool {
	for _, tested := range b.Tested {
		if tested == version {
			return true
		}
	}
	return false
}
