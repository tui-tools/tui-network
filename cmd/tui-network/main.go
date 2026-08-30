// Command tui-network is a terminal UI for the machine's network: links,
// addresses, routes, DNS and the .network files behind them. It previews the
// exact command line of every change before running it. systemd-networkd with
// systemd-resolved is the backend implemented today; the code is written
// against a generic interface so NetworkManager can follow.
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
	"github.com/tui-tools/tui-network/internal/network"
	"github.com/tui-tools/tui-network/internal/networkd"
)

// toolName is the binary name, which is also the configuration directory:
// /etc/tui-network/config.toml and ~/.config/tui-network/config.toml.
const toolName = "tui-network"

// backendName is the manifest's name for the backend this tool drives. It is
// what the version probe and the compatibility block are keyed on.
const backendName = "systemd-networkd"

// version is stamped by the release build (-ldflags "-X main.version=…").
var version = "dev"

// defaults declares the configuration keys tui-network understands. Only these
// are read from the environment (TUI_NETWORK_SUDO, …).
func defaults() map[string]string {
	return map[string]string{
		config.KeySudo:  "sudo -n",
		config.KeyTheme: "",
	}
}

// options holds the parsed command line.
type options struct {
	demo        bool
	check       bool
	themePath   string
	sudo        string
	showVersion bool
	// sudoSet records whether -sudo was passed, so `--sudo ""` can disable
	// escalation instead of reading as "not given".
	sudoSet bool
}

// parseFlags defines and reads the command line.
func parseFlags(args []string, out *os.File) (options, error) {
	var opts options
	fs := flag.NewFlagSet(toolName, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.BoolVar(&opts.demo, "demo", false,
		"run against a sample machine, without touching the real network")
	fs.BoolVar(&opts.check, "check", false,
		"read the network and print the parsed model as JSON, then exit "+
			"(no UI, no changes); exit 1 if the backend cannot be read")
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

	cfg, err := config.Load(config.Options{Tool: toolName, Defaults: defaults()})
	if err != nil {
		return err
	}
	applyOverrides(&cfg, opts)

	// The backend version is probed once, before the backend is built, because
	// the backend needs the capability set: which reads exist on this systemd
	// is a version question, and the answer comes from the manifest.
	backendCompat := probeCompat(context.Background(), opts.demo)

	backend, err := pickBackend(cfg, opts, backendCompat)
	if err != nil {
		return err
	}

	// --check is the non-interactive path: it reads the backend and prints,
	// and never starts a terminal program.
	if opts.check {
		return runCheck(backend, backendCompat, os.Stdout)
	}

	// The configured theme is handed to the kit through the same variable the
	// user could set by hand, so precedence stays in one place.
	if path := cfg.Theme(); path != "" {
		if err := os.Setenv("TUI_THEME", path); err != nil {
			return err
		}
	}

	program := tea.NewProgram(newApp(backend, theme.New(), backendCompat),
		tea.WithAltScreen())
	_, err = program.Run()
	return err
}

// applyOverrides folds the command line into the configuration, which is the
// last and highest-precedence layer.
func applyOverrides(cfg *config.Config, opts options) {
	if opts.themePath != "" {
		cfg.Set(config.KeyTheme, opts.themePath)
	}
	// An explicitly empty -sudo disables escalation, so the flag is applied
	// whenever it was passed, empty value included.
	if opts.sudoSet {
		cfg.Set(config.KeySudo, opts.sudo)
	}
}

// pickBackend returns the demo backend or the real one.
func pickBackend(cfg config.Config, opts options,
	backendCompat compat.Result) (network.Backend, error) {
	if opts.demo {
		return networkd.NewFake(), nil
	}
	return networkd.NewReal(cfg.SudoPrefix(), backendCompat.Caps())
}
