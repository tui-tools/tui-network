// Command tui-network is a terminal UI for the machine's network: links,
// addresses, routes, DNS and the .network files behind them. It previews the
// exact command line of every change before running it. It drives
// systemd-networkd or NetworkManager, whichever the machine runs, through one
// backend interface.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/config"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-network/internal/backends"
	"github.com/tui-tools/tui-network/internal/dhcp"
	"github.com/tui-tools/tui-network/internal/dhcpd"
	"github.com/tui-tools/tui-network/internal/network"
	"github.com/tui-tools/tui-network/internal/networkd"
	"github.com/tui-tools/tui-network/internal/networkmanager"
)

// toolName is the binary name, which is also the configuration directory:
// /etc/tui-network/config.toml and ~/.config/tui-network/config.toml.
const toolName = "tui-network"

// backendName is the manifest's name for the backend a machine gets when
// nothing else is known: what --report names on a machine where selection
// itself failed.
const backendName = backends.NetworkdName

// The values -demo-dhcp takes: the sample router runs dnsmasq, or
// systemd-networkd's own DHCP server, which is what an Omarchy Router runs.
const (
	demoDHCPDnsmasq  = "dnsmasq"
	demoDHCPNetworkd = "networkd"
)

// version is stamped by the release build (-ldflags "-X main.version=…").
var version = "dev"

// defaults declares the configuration keys tui-network understands. Only these
// are read from the environment (TUI_NETWORK_SUDO, …).
func defaults() map[string]string {
	return map[string]string{
		backends.KeyBackend: backends.BackendAuto,
		config.KeySudo:      "sudo -n",
		config.KeyTheme:     "",
	}
}

// demoFlag is the value of --demo: which manager the in-memory sample machine
// imitates.
//
// It is a flag.Value rather than a bool so that both spellings work: plain
// `--demo` gives the networkd sample, and `--demo=networkmanager` the
// NetworkManager one. IsBoolFlag is what lets the bare form parse.
type demoFlag struct {
	on      bool
	backend string
}

// String renders the flag's current value for the usage text.
func (d *demoFlag) String() string {
	if d == nil || !d.on {
		return ""
	}
	return d.backend
}

// Set accepts the bare form ("true", from `--demo`) and a backend name.
func (d *demoFlag) Set(value string) error {
	d.on = true
	switch value {
	case "", "true":
		d.backend = backends.BackendNetworkd
		return nil
	case backends.BackendNetworkd, backends.BackendNetworkManager:
		d.backend = value
		return nil
	case "false":
		d.on = false
		return nil
	default:
		return fmt.Errorf("unknown demo backend %q: use %s or %s",
			value, backends.BackendNetworkd, backends.BackendNetworkManager)
	}
}

// IsBoolFlag lets `--demo` stand on its own.
func (d *demoFlag) IsBoolFlag() bool { return true }

// options holds the parsed command line.
type options struct {
	demo        demoFlag
	backend     string
	check       bool
	report      bool
	themePath   string
	sudo        string
	showVersion bool
	// sudoSet records whether -sudo was passed, so `--sudo ""` can disable
	// escalation instead of reading as "not given".
	sudoSet bool
	// demoDHCP names the DHCP server the sample router runs under --demo.
	// A router serves DHCP from a package (dnsmasq) or from systemd-networkd
	// itself, and the two screens differ enough to be worth showing both.
	demoDHCP string
}

// parseFlags defines and reads the command line.
func parseFlags(args []string, out *os.File) (options, error) {
	var opts options
	fs := flag.NewFlagSet(toolName, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Var(&opts.demo, "demo",
		"run against a sample machine, without touching the real network; "+
			"--demo=networkmanager imitates a NetworkManager machine instead "+
			"of a systemd-networkd one")
	fs.StringVar(&opts.backend, "backend", "",
		"network manager to drive: auto, networkd or networkmanager "+
			"(overrides the config file)")
	fs.BoolVar(&opts.check, "check", false,
		"read the network and print the parsed model as JSON, then exit "+
			"(no UI, no changes); exit 1 if the backend cannot be read")
	fs.StringVar(&opts.demoDHCP, "demo-dhcp", demoDHCPDnsmasq,
		"with --demo: the DHCP server the sample router runs, \"dnsmasq\" or "+
			"\"networkd\" (systemd-networkd's own)")
	fs.BoolVar(&opts.report, "report", false, reportUsage)
	fs.StringVar(&opts.themePath, "theme", "",
		"path to an Omarchy-style colors.toml (overrides the config file)")
	fs.StringVar(&opts.sudo, "sudo", "",
		"privilege escalation prefix, e.g. \"sudo -n\" or \"\" to disable")
	fs.BoolVar(&opts.showVersion, "version", false, "print the version and exit")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(out, "tui-network — a terminal UI for the machine's network\n\n"+
			"Usage:\n  tui-network [flags]\n\nFlags:\n")
		fs.PrintDefaults()
		_, _ = fmt.Fprintf(out, "\nConfiguration is read from %s, then %s, "+
			"then TUI_NETWORK_* in the environment.\n",
			config.SystemPathFor(toolName), config.UserPathFor(toolName))
	}
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "sudo" {
			opts.sudoSet = true
		}
	})
	return opts, nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, toolName+":", err)
		os.Exit(1)
	}
}

// run wires the configuration, the backend and the Bubble Tea program.
func run(args []string) error {
	opts, err := parseFlags(args, os.Stdout)
	if err != nil {
		// flag already printed the reason and the usage.
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if opts.showVersion {
		fmt.Println(toolName, version)
		return nil
	}
	if opts.demoDHCP != demoDHCPDnsmasq && opts.demoDHCP != demoDHCPNetworkd {
		return fmt.Errorf("-demo-dhcp: %q is not a sample DHCP server "+
			"(%s or %s)", opts.demoDHCP, demoDHCPDnsmasq, demoDHCPNetworkd)
	}

	cfg, err := config.Load(config.Options{Tool: toolName, Defaults: defaults()})
	if err != nil {
		return err
	}
	applyOverrides(&cfg, opts)
	if err := cfg.OneOf(backends.KeyBackend, backends.Names()...); err != nil {
		return err
	}

	// The backend version is probed once, before the backend is built, because
	// the backend needs the capability set: which reads exist on this systemd
	// is a version question, and the answer comes from the manifest.
	// The configured theme is handed to the kit through the same variable the
	// user could set by hand, so precedence stays in one place. It is set
	// before the backend is built so --report can name the theme the UI would
	// have used even on a machine where no backend can be.
	if path := cfg.Theme(); path != "" {
		if err := os.Setenv("TUI_THEME", path); err != nil {
			return err
		}
	}

	// The DHCP backend is detected the same way whichever path we take: it
	// reads the router's DHCP server (dnsmasq or Kea), and answers with an
	// explained empty model on a machine that runs neither.
	dhcpBackend := pickDHCP(cfg, opts)
	dhcpCompat := probeDHCPCompat(context.Background(), opts.demo.on, dhcpBackend.Name())

	// --report is the non-interactive path that must work everywhere. It reads
	// nothing privileged and it survives a machine with no networkctl at all,
	// because "there is no backend here" is one of the things a bug report has
	// to be able to say. So it comes before the backend is required.
	if opts.report {
		return runReport(cfg, opts, dhcpBackend, dhcpCompat, os.Stdout)
	}

	// Which manager runs this machine is decided before anything is built,
	// because the version probe is keyed on the answer.
	selection, err := selectBackend(cfg, opts)
	if err != nil {
		return err
	}
	backendCompat := probeCompat(context.Background(), opts.demo.on, selection.Name)

	backend, err := buildBackend(cfg, opts, selection, backendCompat)
	if err != nil {
		return err
	}

	// --check is the non-interactive path: it reads the backend and prints,
	// and never starts a terminal program.
	if opts.check {
		return runCheck(backend, selection, backendCompat, dhcpBackend, dhcpCompat, os.Stdout)
	}

	program := tea.NewProgram(
		newApp(backend, dhcpBackend, theme.New(), backendCompat, dhcpCompat),
		tea.WithAltScreen())
	_, err = program.Run()
	return err
}

// applyOverrides folds the command line into the configuration, which is the
// last and highest-precedence layer.
func applyOverrides(cfg *config.Config, opts options) {
	if opts.backend != "" {
		cfg.Set(backends.KeyBackend, opts.backend)
	}
	if opts.themePath != "" {
		cfg.Set(config.KeyTheme, opts.themePath)
	}
	// An explicitly empty -sudo disables escalation, so the flag is applied
	// whenever it was passed, empty value included.
	if opts.sudoSet {
		cfg.Set(config.KeySudo, opts.sudo)
	}
}

// selectBackend decides which manager to drive: the one --demo imitates, the
// one the configuration names, or the one the machine runs.
func selectBackend(cfg config.Config, opts options) (backends.Selection, error) {
	if opts.demo.on {
		name := backends.NetworkdName
		if opts.demo.backend == backends.BackendNetworkManager {
			name = backends.NetworkManagerName
		}
		return backends.Selection{Name: name,
			Detail: "--demo: a sample machine, nothing on this one was read"}, nil
	}
	return backends.Resolve(cfg.String(backends.KeyBackend, backends.BackendAuto))
}

// buildBackend returns the demo backend or the real one for a selection. Each
// manager brings its own fake, so --demo=networkmanager exercises the nmcli
// builders rather than a NetworkManager-flavoured networkd.
func buildBackend(cfg config.Config, opts options, selection backends.Selection,
	backendCompat compat.Result) (network.Backend, error) {
	if opts.demo.on {
		if selection.Name == backends.NetworkManagerName {
			return networkmanager.NewFake(), nil
		}
		return networkd.NewFake(), nil
	}
	if selection.Name == backends.NetworkManagerName {
		return networkmanager.NewReal(cfg.SudoPrefix(), backendCompat.Caps(),
			networkmanager.Options{Foreign: selection.Foreign})
	}
	return networkd.NewReal(cfg.SudoPrefix(), backendCompat.Caps())
}

// pickBackend selects and builds the backend in one step, for the paths that
// do not need the selection itself.
func pickBackend(cfg config.Config, opts options,
	backendCompat compat.Result) (network.Backend, error) {
	selection, err := selectBackend(cfg, opts)
	if err != nil {
		return nil, err
	}
	return buildBackend(cfg, opts, selection, backendCompat)
}

// pickDHCP returns the demo DHCP backend or the real one. The real one never
// fails to construct: a machine with no DHCP server still gets a backend whose
// Load explains the emptiness.
func pickDHCP(cfg config.Config, opts options) dhcp.Backend {
	if opts.demo.on {
		if opts.demoDHCP == demoDHCPNetworkd {
			return dhcpd.NewFakeNetworkd()
		}
		return dhcpd.NewFake()
	}
	backend, _ := dhcpd.NewReal(cfg.SudoPrefix())
	return backend
}
