package dhcpd

import (
	"fmt"
	"strings"

	"github.com/tui-tools/tui-network/internal/dhcp"
	"github.com/tui-tools/tui-network/internal/textdiff"
)

// The manifest names of the two DHCP backends, keyed the way the compat probe
// and the header are.
const (
	// BackendDnsmasq is the dnsmasq backend name.
	BackendDnsmasq = "dnsmasq"
	// BackendKea is the ISC Kea backend name.
	BackendKea = "kea"
)

// FileMode is the mode a written dnsmasq drop-in gets: readable by everyone,
// writable only by root, which is what dnsmasq ships its own files with.
const FileMode = "644"

// dnsmasqUnit is the systemd unit the reload and restart commands drive.
const dnsmasqUnit = "dnsmasq"

// dnsmasqConfDir and dnsmasqMainConf are the only places the tool will write: a
// drop-in directory and the main file, which is what dnsmasq itself reads.
const (
	dnsmasqConfDir  = "/etc/dnsmasq.d"
	dnsmasqMainConf = "/etc/dnsmasq.conf"
)

// checkDnsmasqPath refuses to write anywhere dnsmasq would not read from.
func checkDnsmasqPath(path string) error {
	if path == dnsmasqMainConf || strings.HasPrefix(path, dnsmasqConfDir+"/") {
		return nil
	}
	return fmt.Errorf("dnsmasq: refusing to write outside %s and %s",
		dnsmasqMainConf, dnsmasqConfDir)
}

// BuildInstallFile copies a staged file into place. `install` sets the mode in
// the same call, so there is no window where the file is on disk with the wrong
// permissions.
func BuildInstallFile(tempPath, destination string) (dhcp.Command, error) {
	if err := checkDnsmasqPath(destination); err != nil {
		return dhcp.Command{}, err
	}
	return dhcp.Command{
		Argv:        []string{"install", "-m", FileMode, tempPath, destination},
		Description: fmt.Sprintf("Install %s as %s", tempPath, destination),
		Destructive: true,
	}, nil
}

// BuildReloadDnsmasq re-reads the configuration with a SIGHUP, which is enough
// for a reservation: dnsmasq re-reads dhcp-host on reload without dropping the
// leases it already granted.
func BuildReloadDnsmasq() dhcp.Command {
	return dhcp.Command{
		Argv:        []string{"systemctl", "reload", dnsmasqUnit},
		Description: "Reload dnsmasq (SIGHUP) so it re-reads its reservations",
	}
}

// BuildRestartDnsmasq restarts the service, which a pool range change needs:
// dnsmasq does not re-read dhcp-range on a reload. The restart briefly stops
// DNS and DHCP, so it is marked destructive.
func BuildRestartDnsmasq() dhcp.Command {
	return dhcp.Command{
		Argv: []string{"systemctl", "restart", dnsmasqUnit},
		Description: "Restart dnsmasq so it re-reads its pool ranges " +
			"(briefly interrupts DNS and DHCP)",
		Destructive: true,
	}
}

// writePlan assembles a dhcp.WritePlan from a rendered edit: the diff against
// what is on disk, the staged copy, and the commands that install and then
// reload or restart the server.
//
// stage writes the pending content somewhere the install command can copy from
// — a real temporary file on the host, or an in-memory name under --demo — and
// returns that path.
func writePlan(path, before, after string, restart bool,
	stage func(path, content string) (string, error)) (dhcp.WritePlan, error) {
	if err := checkDnsmasqPath(path); err != nil {
		return dhcp.WritePlan{}, err
	}
	if before == after {
		return dhcp.WritePlan{}, fmt.Errorf("%s already says exactly this", path)
	}
	temp, err := stage(path, after)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	install, err := BuildInstallFile(temp, path)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	apply := BuildReloadDnsmasq()
	if restart {
		apply = BuildRestartDnsmasq()
	}
	return dhcp.WritePlan{
		Path:     path,
		Content:  after,
		Diff:     textdiff.Unified(path, before, after),
		TempPath: temp,
		Commands: []dhcp.Command{install, apply},
	}, nil
}
