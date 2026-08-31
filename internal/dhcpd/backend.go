// Package dhcpd is the DHCP-server backend of tui-network, and — with
// internal/networkd — one of the two places in the repository that starts a
// process. It reads what a small router's DHCP server hands out, for the two
// servers such a router runs: dnsmasq and ISC Kea.
//
// The layout mirrors internal/networkd. The pure parsers (dnsmasq lease lines
// and configuration, Kea JSON configuration and lease CSV) are the fuzzable
// core and touch nothing; this file is the host-facing part — detecting which
// server is present, reading its files (escalating only when a plain read
// cannot open one), and assembling the previewed commands that a confirm dialog
// shows before they run. dnsmasq mutations are file edits: stage the file, show
// the diff, then install it and reload or restart the service. Kea is read-only
// in this phase.
package dhcpd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tui-tools/tui-kit/runner"
	"github.com/tui-tools/tui-network/internal/dhcp"
)

// ErrNotAvailable reports that no DHCP backend can be driven on this machine.
var ErrNotAvailable = runner.ErrNotAvailable

// searchPaths are the locations a non-root PATH commonly omits.
var searchPaths = map[string][]string{
	"dnsmasq":   {"/usr/sbin/dnsmasq", "/sbin/dnsmasq", "/usr/bin/dnsmasq"},
	"kea-dhcp4": {"/usr/sbin/kea-dhcp4", "/usr/bin/kea-dhcp4"},
	"systemctl": {"/usr/bin/systemctl", "/bin/systemctl"},
	"install":   {"/usr/bin/install", "/bin/install"},
	"cat":       {"/usr/bin/cat", "/bin/cat"},
}

// The files each server keeps its state in. These are the defaults a
// distribution package installs; a machine that moved them is read as if the
// file were simply absent, which the empty state explains.
const (
	dnsmasqLeases = "/var/lib/misc/dnsmasq.leases"
	keaConfPath   = "/etc/kea/kea-dhcp4.conf"
	keaLeasesPath = "/var/lib/kea/kea-leases4.csv"
)

// keaUnits are the systemd unit names Kea's DHCPv4 server ships under across
// distributions; the first that answers `is-active` is used.
var keaUnits = []string{"kea-dhcp4-server", "kea-dhcp4"}

// dnsmasqSkipSuffixes are the backup extensions dnsmasq itself ignores in its
// drop-in directory, so tui-network ignores them too.
var dnsmasqSkipSuffixes = []string{".bak", "~", ".dpkg-dist", ".dpkg-old",
	".rpmnew", ".rpmsave", ".swp"}

// Real reads the DHCP server on the host. It satisfies dhcp.Backend.
type Real struct {
	kind string

	dnsmasq   *runner.Runner
	keaDHCP4  *runner.Runner
	systemctl *runner.Runner
	install   *runner.Runner
	// cat is the escalated fallback for a lease or config file a plain read
	// cannot open.
	cat *runner.Runner

	// now is the clock the lease expiries are measured against. A field so a
	// test can pin it.
	now func() time.Time
}

// Available reports whether either DHCP server is installed on this host.
func Available() bool {
	return detectKind() != dhcp.KindNone
}

// detectKind picks the server to read: the one that is present, preferring
// dnsmasq when both are, since it is the one that also serves DNS.
func detectKind() string {
	if runner.Available("dnsmasq", searchPaths["dnsmasq"]...) || fileExists(dnsmasqMainConf) {
		return dhcp.KindDnsmasq
	}
	if runner.Available("kea-dhcp4", searchPaths["kea-dhcp4"]...) || fileExists(keaConfPath) {
		return dhcp.KindKea
	}
	return dhcp.KindNone
}

// NewReal locates the binaries and, when not running as root, validates the
// privilege prefix used by the mutating commands. Reads are unprivileged where
// they can be; only writing a file and reloading the service escalate.
//
// It never fails for lack of a server: a machine with neither dnsmasq nor Kea
// still gets a backend whose Load returns an explained empty model, because
// "there is no DHCP server here" is something the screen has to be able to say.
func NewReal(sudoPrefix []string) (*Real, error) {
	r := &Real{kind: detectKind(), now: time.Now}
	unprivileged := false

	// The version probes and `systemctl is-active` are unprivileged reads.
	r.dnsmasq, _ = runner.New(runner.Options{
		Bin: "dnsmasq", SearchPaths: searchPaths["dnsmasq"],
		SudoPrefix: sudoPrefix, PrivilegedReads: &unprivileged,
	})
	r.keaDHCP4, _ = runner.New(runner.Options{
		Bin: "kea-dhcp4", SearchPaths: searchPaths["kea-dhcp4"],
		SudoPrefix: sudoPrefix, PrivilegedReads: &unprivileged,
	})
	r.systemctl, _ = runner.New(runner.Options{
		Bin: "systemctl", SearchPaths: searchPaths["systemctl"],
		SudoPrefix: sudoPrefix, PrivilegedReads: &unprivileged,
	})
	// The escalated writers and the read fallback.
	r.install, _ = runner.New(runner.Options{
		Bin: "install", SearchPaths: searchPaths["install"], SudoPrefix: sudoPrefix,
	})
	r.cat, _ = runner.New(runner.Options{
		Bin: "cat", SearchPaths: searchPaths["cat"], SudoPrefix: sudoPrefix,
	})
	return r, nil
}

// Name identifies the backend for the header and the compat probe.
func (r *Real) Name() string {
	if r.kind == dhcp.KindNone {
		return "dhcp"
	}
	return r.kind
}

// Describe names the backend and how it is reached.
func (r *Real) Describe() string {
	switch r.kind {
	case dhcp.KindDnsmasq:
		return "dnsmasq (DNS and DHCP)"
	case dhcp.KindKea:
		return "ISC Kea (read-only)"
	default:
		return "no DHCP server"
	}
}

// Capabilities reports which mutations this backend offers: dnsmasq is
// editable, Kea and a serverless machine are read-only.
func (r *Real) Capabilities() dhcp.Capabilities {
	if r.kind == dhcp.KindDnsmasq {
		return dnsmasqCapabilities
	}
	return dhcp.Capabilities{}
}

// Preview renders the exact command line Run will execute.
func (r *Real) Preview(cmd dhcp.Command) string {
	if run := r.runnerFor(cmd); run != nil {
		return run.Preview(cmd)
	}
	return cmd.String()
}

// runnerFor picks the runner that owns a command, by its argv[0].
func (r *Real) runnerFor(cmd dhcp.Command) *runner.Runner {
	if len(cmd.Argv) == 0 {
		return nil
	}
	switch cmd.Argv[0] {
	case "install":
		return r.install
	case "systemctl":
		return r.systemctl
	default:
		return nil
	}
}

// Run executes a previewed command.
func (r *Real) Run(ctx context.Context, cmd dhcp.Command) (string, error) {
	run := r.runnerFor(cmd)
	if run == nil {
		return "", fmt.Errorf("dhcpd: %q is not a command this backend runs", firstArg(cmd))
	}
	return run.Run(ctx, cmd)
}

// firstArg names the binary a command wanted, for an error message.
func firstArg(cmd dhcp.Command) string {
	if len(cmd.Argv) == 0 {
		return "(empty command)"
	}
	return cmd.Argv[0]
}

// Load reads the server, its pools, its reservations and its leases. Every
// layer may fail on its own: a machine where the server is installed but not
// running still shows the pools its configuration declares.
func (r *Real) Load(ctx context.Context) (dhcp.Model, error) {
	switch r.kind {
	case dhcp.KindDnsmasq:
		return r.loadDnsmasq(ctx), nil
	case dhcp.KindKea:
		return r.loadKea(ctx), nil
	default:
		return dhcp.Model{Server: dhcp.Server{
			Kind:    dhcp.KindNone,
			Explain: "no DHCP server was found: neither dnsmasq nor ISC Kea is installed here",
		}}, nil
	}
}

// loadDnsmasq reads the dnsmasq configuration and lease file.
func (r *Real) loadDnsmasq(ctx context.Context) dhcp.Model {
	server := dhcp.Server{
		Kind:        dhcp.KindDnsmasq,
		Present:     true,
		CombinedDNS: true,
		Active:      r.serviceActive(ctx, dnsmasqUnit),
		Version:     r.dnsmasqVersion(ctx),
		LeasesPath:  dnsmasqLeases,
		ManagedFile: DnsmasqManagedFile,
	}

	model := dhcp.Model{}
	for _, path := range r.dnsmasqConfFiles() {
		raw, err := r.readFile(ctx, path)
		if err != nil {
			continue
		}
		server.ConfPaths = append(server.ConfPaths, path)
		pools, reservations := ParseDnsmasqConf(path, raw)
		model.Pools = append(model.Pools, pools...)
		model.Reservations = append(model.Reservations, reservations...)
	}

	if raw, err := r.readFile(ctx, dnsmasqLeases); err == nil {
		model.Leases = ParseDnsmasqLeases(raw, r.now())
	} else {
		server.LeasesPath = ""
	}
	server.Explain = explainServer(server, len(model.Pools))
	model.Server = server
	return model
}

// loadKea reads the Kea configuration and lease CSV, read-only.
func (r *Real) loadKea(ctx context.Context) dhcp.Model {
	server := dhcp.Server{
		Kind:       dhcp.KindKea,
		Present:    true,
		Active:     r.serviceActive(ctx, keaUnits...),
		Version:    r.keaVersion(ctx),
		LeasesPath: keaLeasesPath,
	}

	model := dhcp.Model{}
	if raw, err := r.readFile(ctx, keaConfPath); err == nil {
		server.ConfPaths = append(server.ConfPaths, keaConfPath)
		model.Pools, model.Reservations = ParseKeaConfig(keaConfPath, raw)
	}
	if raw, err := r.readFile(ctx, keaLeasesPath); err == nil {
		model.Leases = ParseKeaLeasesCSV(raw, r.now())
	} else {
		server.LeasesPath = ""
	}
	server.Explain = explainServer(server, len(model.Pools))
	model.Server = server
	return model
}

// explainServer is the one-line note the empty state shows when there is
// nothing else to see: a server present but stopped, or present with no pool.
func explainServer(server dhcp.Server, pools int) string {
	switch {
	case !server.Active && pools == 0:
		return server.Kind + " is installed but not running, and declares no pool"
	case !server.Active:
		return server.Kind + " is installed but not running"
	case pools == 0:
		return server.Kind + " is running but declares no DHCP pool"
	default:
		return ""
	}
}

// dnsmasqConfFiles is the list of configuration files dnsmasq reads: the main
// file, then the drop-in directory, skipping the backup extensions dnsmasq
// itself ignores.
func (r *Real) dnsmasqConfFiles() []string {
	files := []string{dnsmasqMainConf}
	entries, err := os.ReadDir(dnsmasqConfDir)
	if err != nil {
		return files
	}
	for _, entry := range entries {
		if entry.IsDir() || skipDnsmasqFile(entry.Name()) {
			continue
		}
		files = append(files, filepath.Join(dnsmasqConfDir, entry.Name()))
	}
	return files
}

// skipDnsmasqFile reports whether a drop-in file name is a backup dnsmasq
// ignores.
func skipDnsmasqFile(name string) bool {
	for _, suffix := range dnsmasqSkipSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// serviceActive reports whether any of the named units is active. It is an
// unprivileged read.
func (r *Real) serviceActive(ctx context.Context, units ...string) bool {
	if r.systemctl == nil {
		return false
	}
	for _, unit := range units {
		out, _ := r.systemctl.Read(ctx, "systemctl", "is-active", unit)
		if strings.TrimSpace(out) == "active" {
			return true
		}
	}
	return false
}

// dnsmasqVersion reads the dnsmasq version, empty when it cannot.
func (r *Real) dnsmasqVersion(ctx context.Context) string {
	if r.dnsmasq == nil {
		return ""
	}
	out, err := r.dnsmasq.Read(ctx, "dnsmasq", "--version")
	if err != nil {
		return ""
	}
	// "Dnsmasq version 2.90  Copyright ..." — the third field is the version.
	fields := strings.Fields(out)
	if len(fields) >= 3 && strings.EqualFold(fields[0], "dnsmasq") {
		return fields[2]
	}
	return ""
}

// keaVersion reads the Kea version, whose `-V` prints the bare version first.
func (r *Real) keaVersion(ctx context.Context) string {
	if r.keaDHCP4 == nil {
		return ""
	}
	out, err := r.keaDHCP4.Read(ctx, "kea-dhcp4", "-V")
	if err != nil && strings.TrimSpace(out) == "" {
		return ""
	}
	if line := runner.FirstLine(strings.TrimSpace(out)); line != "" {
		return strings.Fields(line)[0]
	}
	return ""
}

// readFile reads one file, escalating with `cat` only when a plain read is
// refused for want of privilege — the same fallback the links screen uses for a
// .network file a non-root process cannot open.
func (r *Real) readFile(ctx context.Context, path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the paths are this package's own constants and directory listing
	if err == nil {
		return string(raw), nil
	}
	if !os.IsPermission(err) || r.cat == nil {
		return "", err
	}
	return r.cat.Read(ctx, "cat", "--", path)
}

// BuildAddReservation renders the managed drop-in with a reservation appended.
func (r *Real) BuildAddReservation(res dhcp.Reservation) (dhcp.WritePlan, error) {
	if err := r.requireDnsmasq(); err != nil {
		return dhcp.WritePlan{}, err
	}
	before := r.readOrEmpty(DnsmasqManagedFile)
	after, err := AddReservationText(before, res)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	return writePlan(DnsmasqManagedFile, before, after, false, stageFile)
}

// BuildRemoveReservation renders the reservation's own file with its line gone.
func (r *Real) BuildRemoveReservation(res dhcp.Reservation) (dhcp.WritePlan, error) {
	if err := r.requireDnsmasq(); err != nil {
		return dhcp.WritePlan{}, err
	}
	if res.Source == "" {
		return dhcp.WritePlan{}, fmt.Errorf("dnsmasq: this reservation has no file to edit")
	}
	before := r.readOrEmpty(res.Source)
	after, err := RemoveReservationText(before, res)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	return writePlan(res.Source, before, after, false, stageFile)
}

// BuildSetPoolRange renders the pool's file with its range adjusted.
func (r *Real) BuildSetPoolRange(orig dhcp.Pool, newStart, newEnd string) (dhcp.WritePlan, error) {
	if err := r.requireDnsmasq(); err != nil {
		return dhcp.WritePlan{}, err
	}
	if orig.Source == "" {
		return dhcp.WritePlan{}, fmt.Errorf("dnsmasq: this pool has no file to edit")
	}
	before := r.readOrEmpty(orig.Source)
	after, err := SetPoolRangeText(before, orig, newStart, newEnd)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	return writePlan(orig.Source, before, after, true, stageFile)
}

// requireDnsmasq refuses a mutation the backend cannot make: Kea is read-only
// in this phase, and a machine with no server has nothing to change.
func (r *Real) requireDnsmasq() error {
	if r.kind != dhcp.KindDnsmasq {
		return fmt.Errorf("dhcpd: %s is read-only in this version", r.Name())
	}
	return nil
}

// readOrEmpty reads a file, treating a missing one as empty: a reservation
// added to a drop-in that does not exist yet creates it.
func (r *Real) readOrEmpty(path string) string {
	raw, err := r.readFile(context.Background(), path)
	if err != nil {
		return ""
	}
	return raw
}

// stageFile writes pending content to a private temporary directory and returns
// its path. The directory is the user's own, so staging needs no privileges;
// only the install step does.
func stageFile(destination, content string) (string, error) {
	dir, err := os.MkdirTemp("", "tui-network-dhcp-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, filepath.Base(destination))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// fileExists reports whether a path is a regular file present on disk.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
