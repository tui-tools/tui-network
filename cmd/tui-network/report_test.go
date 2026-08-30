package main

import (
	"os"
	"os/user"
	"strings"
	"testing"
)

// TestRunReportDemo checks the half of the block this tool owns. The kit's own
// tests cover the machine facts and the scrubbing; what has to be right here is
// that --demo says demo, that the backend the fake imitates is named rather
// than left to be guessed, and that no network was read to produce any of it.
func TestRunReportDemo(t *testing.T) {
	var out strings.Builder
	opts := options{demo: true, report: true}
	if err := runReport(baseConfig(), opts, &out); err != nil {
		t.Fatalf("runReport: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"backend: demo\n",
		"mode: demo (sample data, the system was not read)\n",
		"demo backend: systemd-networkd\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
	// There is no systemd behind the fake, so neither version gate must be
	// claimed either way.
	for _, unwanted := range []string{"reads:", "link up/down:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("demo report should not claim %q:\n%s", unwanted, got)
		}
	}
	if !strings.HasPrefix(got, toolName+" ") {
		t.Errorf("report should start with the tool name:\n%s", got)
	}
}

// TestRunReportLive checks that a live report carries the two lines the systemd
// version decides. They are what tells a parse bug in the JSON reader from one
// in the text reader, and a key that was never offered from a key that did
// nothing.
func TestRunReportLive(t *testing.T) {
	var out strings.Builder
	if err := runReport(baseConfig(), options{report: true}, &out); err != nil {
		t.Fatalf("runReport: %v", err)
	}
	got := out.String()
	for _, want := range []string{"mode: live\n", "reads: ", "link up/down: "} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
}

// TestRunReportIsPublishable is the privacy promise as a test. The block is
// written to be pasted into a public issue, so anything that names this
// machine or the person on it appearing in it is a bug rather than a cosmetic
// detail. It matters more here than in most of the family: everything else
// this tool can print is an address or an interface name.
func TestRunReportIsPublishable(t *testing.T) {
	var out strings.Builder
	if err := runReport(baseConfig(), options{report: true}, &out); err != nil {
		t.Fatalf("runReport: %v", err)
	}
	got := out.String()

	if strings.Contains(got, "/home/") || strings.Contains(got, "/root/") {
		t.Errorf("report carries a home path:\n%s", got)
	}
	// The host name is checked against the block as a whole. A machine whose
	// host name is also its distribution or in its kernel release — "fedora"
	// on a stock Fedora — would fail on a line that is not a leak, so that
	// case is skipped rather than asserted wrongly: the report is generated
	// with no way to reach the host name at all, and this test is the guard
	// against that changing.
	if host, err := os.Hostname(); err == nil && host != "" {
		switch {
		case strings.Contains(lineValue(got, "distro"), host),
			strings.Contains(lineValue(got, "kernel"), host):
			t.Logf("host name %q is not distinctive here, skipping", host)
		case strings.Contains(got, host):
			t.Errorf("report carries the host name:\n%s", got)
		}
	}
	if u, err := user.Current(); err == nil && len(u.Username) > 2 {
		if strings.Contains(got, u.Username) {
			t.Errorf("report carries the user name:\n%s", got)
		}
	}
}

// TestVersionGateLines covers the two lines the systemd version decides. Each
// must read differently on either side of 249, and the older side must name
// the version, because "it does not do that here" without a reason reads as a
// bug in the tool.
func TestVersionGateLines(t *testing.T) {
	if got := readsLine(true); got != "networkctl --json" {
		t.Errorf("readsLine(true) = %q", got)
	}
	if got := readsLine(false); !strings.Contains(got, "249") {
		t.Errorf("readsLine(false) should name the version gate, got %q", got)
	}
	if got := linkUpDownLine(true); got != "available" {
		t.Errorf("linkUpDownLine(true) = %q", got)
	}
	if got := linkUpDownLine(false); !strings.Contains(got, "249") {
		t.Errorf("linkUpDownLine(false) should name the version gate, got %q", got)
	}
}

// lineValue returns the value of one `key: value` line of a report block, or
// the empty string when the key is not there.
func lineValue(block, key string) string {
	for _, line := range strings.Split(block, "\n") {
		if value, ok := strings.CutPrefix(line, key+": "); ok {
			return value
		}
	}
	return ""
}
