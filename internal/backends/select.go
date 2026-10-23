// Package backends chooses which network manager tui-network drives, from the
// configuration and from what the machine actually runs.
//
// The question is not "which one is installed": Fedora and Ubuntu desktops
// ship networkctl next to NetworkManager, and an Omarchy machine can have
// NetworkManager installed and idle. It is "which one is running, and which
// one owns the links", and the answer comes with the sentence that explains
// it, because on a machine running both that sentence is what the operator
// actually wants to read.
package backends

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/tui-tools/tui-kit/runner"
	"github.com/tui-tools/tui-network/internal/networkd"
	"github.com/tui-tools/tui-network/internal/networkmanager"
)

// KeyBackend is the configuration key naming the backend to drive.
const KeyBackend = "backend"

// The values KeyBackend accepts.
const (
	BackendAuto           = "auto"
	BackendNetworkd       = "networkd"
	BackendNetworkManager = "networkmanager"
)

// Names lists every accepted value, for configuration validation and the flag
// help.
func Names() []string {
	return []string{BackendAuto, BackendNetworkd, BackendNetworkManager}
}

// The manifest names of the two backends, which is what Selection.Name holds.
const (
	NetworkdName       = "systemd-networkd"
	NetworkManagerName = networkmanager.Name
)

// Selection is the backend that was chosen, why, and the other manager that is
// running too, if any.
type Selection struct {
	// Name is the manifest name of the chosen backend.
	Name string `json:"name"`
	// Detail is the one sentence explaining the choice.
	Detail string `json:"detail"`
	// Foreign names the other manager when it is running as well.
	Foreign string `json:"foreign,omitempty"`
}

// Facts is what the detector reads about the machine. It is a value so the
// decision can be tested as a table, without a machine.
type Facts struct {
	// NetworkdInstalled and NetworkManagerInstalled report the binaries.
	NetworkdInstalled       bool
	NetworkManagerInstalled bool
	// NetworkdActive and NetworkManagerActive report the services.
	NetworkdActive       bool
	NetworkManagerActive bool
	// NetworkdLinks and NetworkManagerLinks count the links each one manages.
	// They are only read when both services are active.
	NetworkdLinks       int
	NetworkManagerLinks int
}

// Decide applies the selection rules to what was read. See Resolve.
func Decide(f Facts) (Selection, error) {
	switch {
	case f.NetworkManagerActive && !f.NetworkdActive && f.NetworkManagerInstalled:
		return Selection{Name: NetworkManagerName,
			Detail: "NetworkManager is running and systemd-networkd is not"}, nil
	case f.NetworkdActive && !f.NetworkManagerActive && f.NetworkdInstalled:
		return Selection{Name: NetworkdName,
			Detail: "systemd-networkd is running and NetworkManager is not"}, nil
	case f.NetworkdActive && f.NetworkManagerActive:
		counts := fmt.Sprintf("NetworkManager manages %d link(s), systemd-networkd %d",
			f.NetworkManagerLinks, f.NetworkdLinks)
		if f.NetworkManagerLinks > f.NetworkdLinks && f.NetworkManagerInstalled {
			return Selection{Name: NetworkManagerName, Foreign: NetworkdName,
				Detail: "both are running; " + counts +
					", so NetworkManager is the one in charge"}, nil
		}
		if f.NetworkdInstalled {
			detail := "both are running; " + counts +
				", so systemd-networkd is the one in charge"
			if f.NetworkManagerLinks == f.NetworkdLinks {
				detail = "both are running and " + counts +
					"; a tie keeps systemd-networkd"
			}
			return Selection{Name: NetworkdName, Foreign: NetworkManagerName,
				Detail: detail}, nil
		}
	}
	switch {
	case f.NetworkdInstalled:
		return Selection{Name: NetworkdName,
			Detail: "neither manager is running; networkctl is installed, " +
				"so the links are shown read-only"}, nil
	case f.NetworkManagerInstalled:
		return Selection{Name: NetworkManagerName,
			Detail: "neither manager is running and only nmcli is installed"}, nil
	}
	return Selection{}, fmt.Errorf("no network manager found: install " +
		"systemd-networkd (networkctl) or NetworkManager (nmcli), " +
		"or run with --demo")
}

// Resolve names the backend to build, without building it.
//
// With "auto" the rules are: the one manager whose service is running; when
// both run, the one that manages more links, a tie keeping systemd-networkd;
// when neither runs, networkd if networkctl is installed (its read-only view
// of a machine with no manager), NetworkManager if only nmcli is.
func Resolve(name string) (Selection, error) {
	switch name {
	case BackendNetworkd:
		return Selection{Name: NetworkdName,
			Detail: "chosen by configuration, not by detection"}, nil
	case BackendNetworkManager:
		return Selection{Name: NetworkManagerName,
			Detail: "chosen by configuration, not by detection"}, nil
	case BackendAuto, "":
		return Decide(ReadFacts())
	default:
		return Selection{}, fmt.Errorf("unknown backend %q: use %s",
			name, strings.Join(Names(), ", "))
	}
}

// ReadFacts reads the machine. It is a variable so tests can answer for a
// machine they are not running on.
var ReadFacts = func() Facts {
	f := Facts{
		NetworkdInstalled:       networkd.Available(),
		NetworkManagerInstalled: networkmanager.Available(),
	}
	f.NetworkdActive = unitActive("systemd-networkd.service")
	f.NetworkManagerActive = unitActive("NetworkManager.service")
	if f.NetworkdActive && f.NetworkManagerActive {
		f.NetworkdLinks = networkdLinks()
		f.NetworkManagerLinks = networkManagerLinks()
	}
	return f
}

// unitActive asks systemd whether a unit is running. A host without
// systemctl answers false.
func unitActive(unit string) bool {
	bin, err := exec.LookPath("systemctl")
	if err != nil {
		return false
	}
	out, err := exec.Command(bin, "is-active", unit).Output() //nolint:gosec // fixed verb and unit names
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

// read runs one unprivileged read, bounded so a wedged daemon cannot hold the
// terminal before the UI is on screen.
func read(bin string, searchPaths []string, argv ...string) (string, bool) {
	unprivileged := false
	run, err := runner.New(runner.Options{
		Bin:             bin,
		SearchPaths:     searchPaths,
		Timeout:         5 * time.Second,
		PrivilegedReads: &unprivileged,
	})
	if err != nil {
		return "", false
	}
	out, err := run.Read(context.Background(), argv...)
	return out, err == nil
}

// networkdLinks counts the links systemd-networkd configures.
func networkdLinks() int {
	out, ok := read("networkctl", []string{"/usr/bin/networkctl", "/bin/networkctl"},
		"networkctl", "--no-legend", "list")
	if !ok {
		return 0
	}
	return CountNetworkdLinks(out)
}

// CountNetworkdLinks counts the rows of `networkctl --no-legend list` whose
// SETUP column says networkd is (or will be) configuring the link.
func CountNetworkdLinks(out string) int {
	count := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		switch fields[len(fields)-1] {
		case "configured", "configuring", "pending":
			count++
		}
	}
	return count
}

// networkManagerLinks counts the devices NetworkManager configures.
func networkManagerLinks() int {
	out, ok := read("nmcli", []string{"/usr/bin/nmcli", "/bin/nmcli"},
		"nmcli", "-t", "-f", "DEVICE,TYPE,STATE", "device")
	if !ok {
		return 0
	}
	return CountNetworkManagerLinks(out)
}

// CountNetworkManagerLinks counts the rows of `nmcli -t -f DEVICE,TYPE,STATE
// device` NetworkManager manages itself: not unmanaged, not merely observed
// ("connected (externally)"), and not the loopback.
func CountNetworkManagerLinks(out string) int {
	count := 0
	for _, line := range strings.Split(out, "\n") {
		fields := networkmanager.SplitTerse(strings.TrimSpace(line))
		if len(fields) < 3 || fields[1] == "loopback" {
			continue
		}
		state := fields[2]
		if state == "unmanaged" || strings.Contains(state, "externally") {
			continue
		}
		count++
	}
	return count
}
