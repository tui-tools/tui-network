package main

import (
	"context"
	"fmt"
	"io"

	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/config"
	"github.com/tui-tools/tui-kit/report"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-network/internal/dhcp"
)

// The two manifest features the systemd version gates. Between them they
// decide which parser produced the model on screen and which keys the hint bar
// offered, which is most of what a report about either has to say.
const (
	jsonStatusFeature = "json-status"
	linkUpDownFeature = "link-up-down"
)

// runReport prints the block a bug report needs and exits. Everything generic
// — the kit version, the distribution, the kernel, the terminal, where the
// binary came from — is collected by the kit, so the whole family answers
// --report in the same shape. What this function adds is the part only
// tui-network knows: the systemd the compat probe read off networkctl, and the
// two behaviours that version decides — whether the model was parsed from
// `networkctl --json` or from its text output, and whether the link up and
// down keys existed at all.
//
// It never reads the network. --check is the flag that does that; a report has
// to work before it, because a read that came back wrong is the usual reason
// for filing one. For the same reason a machine with no networkctl still gets
// a report, with the backend error as one of its lines: "there is nothing here
// to drive" is a bug report, not a refusal.
func runReport(cfg config.Config, opts options, dhcpBackend dhcp.Backend,
	dhcpCompat compat.Result, out io.Writer) error {
	palette, _ := theme.ResolvePalette()

	// The same probe --check and the header use. There is one version probe in
	// this tool and this is it.
	backendCompat := probeCompat(context.Background(), opts.demo)

	// The backend is built so that a machine without networkctl says so, but
	// its absence must not cost the report: the name is known from the
	// manifest either way.
	name := backendName
	var selectError string
	if backend, err := pickBackend(cfg, opts, backendCompat); err != nil {
		selectError = err.Error()
	} else {
		name = backend.Name()
	}

	info := report.Info{
		Tool:           toolName,
		Version:        version,
		Backend:        name,
		BackendVersion: backendCompat.Version,
		BackendDetail:  backendCompat.Detail,
		Demo:           opts.demo,
		Sudo:           cfg.String(config.KeySudo, ""),
		Theme:          palette.Name,
	}
	if opts.demo {
		// The fake imitates systemd-networkd down to the argv it builds, so
		// the backend line says demo and the imitated backend is named next to
		// it rather than left to be guessed from the tool's name.
		info.Backend = "demo"
		info.Extra = append(info.Extra, report.Field{
			Key: "demo backend", Value: backendName,
		})
	} else {
		// Which parser ran and which keys existed are facts about the systemd
		// on this machine, and there is no systemd behind the fake, so both
		// lines are live-only rather than invented.
		caps := backendCompat.Caps()
		info.Extra = append(info.Extra,
			report.Field{Key: "reads", Value: readsLine(caps.Has(jsonStatusFeature))},
			report.Field{Key: "link up/down", Value: linkUpDownLine(caps.Has(linkUpDownFeature))},
		)
	}
	// The DHCP server is the router half of this tool, and a report about a
	// wrong lease or an unread pool has to name which server was behind it and
	// what version it was. Like the rest of --report this stays out of the
	// state: it names the detected server and the version the probe read, never
	// whether the service is up (that is a read --check does).
	info.Extra = append(info.Extra, report.Field{
		Key: "dhcp backend", Value: dhcpBackendLine(opts.demo, dhcpBackend.Name(), dhcpCompat),
	})

	if selectError != "" {
		info.Extra = append(info.Extra, report.Field{
			Key: "backend error", Value: selectError,
		})
	}

	_, err := io.WriteString(out, report.Render(info))
	return err
}

// dhcpBackendLine names the DHCP/DNS server behind the router screen and the
// version the probe read. dnsmasq is called out as the server that answers DNS
// too, because on such a router the DNS the links screen reads from
// systemd-resolved is not the whole picture.
func dhcpBackendLine(demo bool, kind string, dhcpCompat compat.Result) string {
	switch {
	case demo:
		return "demo (dnsmasq, DNS and DHCP)"
	case kind == dhcp.KindDnsmasq:
		return "dnsmasq " + versionOrUnknown(dhcpCompat) + " (serves DNS and DHCP)"
	case kind == dhcp.KindKea:
		return "ISC Kea " + versionOrUnknown(dhcpCompat) + " (DHCP only)"
	default:
		return "none detected (no dnsmasq or Kea)"
	}
}

// versionOrUnknown is the probed version, or a placeholder when it could not be
// read.
func versionOrUnknown(dhcpCompat compat.Result) string {
	if dhcpCompat.Version == "" {
		return "(version unknown)"
	}
	return dhcpCompat.Version
}

// readsLine says which of the two parsers built the model. `networkctl --json`
// arrived in systemd 249; below it the columns of `networkctl list` and the
// `Key: value` block of `networkctl status` are read instead, and an address
// there carries no prefix length and no lease clock. A field that is missing
// or wrong is a bug in whichever parser this machine used, so the report names
// it.
func readsLine(json bool) string {
	if json {
		return "networkctl --json"
	}
	return "parsed from the text output (systemd before 249)"
}

// linkUpDownLine says whether the up and down keys were on offer. Below
// systemd 249 `networkctl up` and `down` do not exist, the keys are dropped
// from the hint bar and reconfigure stands in their place — which is the first
// thing to check when a report says a key did nothing.
func linkUpDownLine(available bool) string {
	if available {
		return "available"
	}
	return "not offered (systemd before 249), reconfigure instead"
}

// reportUsage is the flag's one-line help, kept here next to what it prints.
var reportUsage = fmt.Sprintf(
	"print the versions and machine facts a bug report needs, then exit "+
		"(no UI, no privileges, nothing about you: paste it into a %s issue)",
	toolName)
