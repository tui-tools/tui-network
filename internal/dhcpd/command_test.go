package dhcpd

import (
	"strings"
	"testing"
)

func TestCheckDnsmasqPath(t *testing.T) {
	ok := []string{
		"/etc/dnsmasq.conf",
		"/etc/dnsmasq.d/tui-network.conf",
		"/etc/dnsmasq.d/other.conf",
	}
	for _, path := range ok {
		if err := checkDnsmasqPath(path); err != nil {
			t.Errorf("checkDnsmasqPath(%q) = %v, want nil", path, err)
		}
	}
	bad := []string{
		"/etc/passwd",
		"/etc/dnsmasq.d",
		"/tmp/evil.conf",
		"../../etc/shadow",
	}
	for _, path := range bad {
		if err := checkDnsmasqPath(path); err == nil {
			t.Errorf("checkDnsmasqPath(%q) accepted a path outside dnsmasq's dirs", path)
		}
	}
}

func TestBuildInstallFile(t *testing.T) {
	cmd, err := BuildInstallFile("/tmp/staged.conf", "/etc/dnsmasq.d/tui-network.conf")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := cmd.String(); got != "install -m 644 /tmp/staged.conf /etc/dnsmasq.d/tui-network.conf" {
		t.Errorf("argv = %q", got)
	}
	if !cmd.Destructive {
		t.Error("installing a file is a change and should be marked destructive")
	}
	// A destination outside dnsmasq's directories is refused.
	if _, err := BuildInstallFile("/tmp/staged.conf", "/etc/passwd"); err == nil {
		t.Error("install to /etc/passwd should be refused")
	}
}

func TestReloadAndRestart(t *testing.T) {
	if got := BuildReloadDnsmasq().String(); got != "systemctl reload dnsmasq" {
		t.Errorf("reload argv = %q", got)
	}
	restart := BuildRestartDnsmasq()
	if restart.String() != "systemctl restart dnsmasq" {
		t.Errorf("restart argv = %q", restart.String())
	}
	// A restart interrupts DNS and DHCP, so it warns.
	if !restart.Destructive {
		t.Error("a restart should be marked destructive")
	}
	if !strings.Contains(restart.Description, "interrupt") {
		t.Errorf("restart should say it interrupts service: %q", restart.Description)
	}
}
