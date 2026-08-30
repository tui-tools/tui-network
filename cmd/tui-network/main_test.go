package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/config"
)

// baseConfig is the configuration as it stands before the flags are folded in.
func baseConfig() config.Config {
	return config.Config{Tool: toolName, Values: defaults()}
}

func TestParseFlags(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer func() { _ = devNull.Close() }()

	opts, err := parseFlags([]string{"--demo", "--theme", "/t/colors.toml"}, devNull)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !opts.demo || opts.themePath != "/t/colors.toml" {
		t.Errorf("opts = %+v", opts)
	}
	if opts.sudoSet {
		t.Error("sudoSet should be false when -sudo is absent")
	}
}

func TestApplyOverrides(t *testing.T) {
	cfg := baseConfig()
	applyOverrides(&cfg, options{themePath: "/t/colors.toml"})
	if got := cfg.Theme(); got != "/t/colors.toml" {
		t.Errorf("Theme() = %q", got)
	}
	// An untouched -sudo must not clear the configured prefix.
	if got := cfg.String(config.KeySudo, ""); got != "sudo -n" {
		t.Errorf("sudo = %q, want the config value", got)
	}

	// An explicit empty -sudo disables escalation.
	cfg = baseConfig()
	applyOverrides(&cfg, options{sudoSet: true, sudo: ""})
	if got := cfg.String(config.KeySudo, "unset"); got != "" {
		t.Errorf("sudo = %q, want empty", got)
	}
	if got := cfg.SudoPrefix(); got != nil {
		t.Errorf("SudoPrefix = %q, want nil", got)
	}
}

func TestDefaultsCoverEveryFlag(t *testing.T) {
	// Every key a flag can override must be declared, otherwise the
	// environment layer silently skips it.
	for _, key := range []string{config.KeySudo, config.KeyTheme} {
		if _, ok := defaults()[key]; !ok {
			t.Errorf("defaults() is missing %q", key)
		}
	}
}

func TestPickBackendDemo(t *testing.T) {
	backend, err := pickBackend(baseConfig(), options{demo: true}, compat.Result{})
	if err != nil {
		t.Fatalf("pickBackend: %v", err)
	}
	if !strings.Contains(backend.Describe(), "demo") {
		t.Errorf("Describe = %q, want it to say it is a demo", backend.Describe())
	}
}

// TestCheckReportsTheModel covers the contract the smoke test depends on: the
// counts and the manager state a shell script can grep for.
func TestCheckReportsTheModel(t *testing.T) {
	backend, err := pickBackend(baseConfig(), options{demo: true}, compat.Result{})
	if err != nil {
		t.Fatalf("pickBackend: %v", err)
	}
	var out bytes.Buffer
	if err := runCheck(backend, compat.Result{}, &out); err != nil {
		t.Fatalf("runCheck: %v", err)
	}
	for _, want := range []string{
		`"tool": "tui-network"`,
		`"backend": "systemd-networkd"`,
		`"links": 3`,
		`"managed": 1`,
		`"foreignManager": "NetworkManager"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("--check output is missing %s", want)
		}
	}
}
