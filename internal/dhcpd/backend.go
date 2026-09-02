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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tui-tools/tui-kit/runner"
	"github.com/tui-tools/tui-network/internal/dhcp"
)

// ErrNotAvailable reports that no DHCP backend can be driven on this machine.
var ErrNotAvailable = runner.ErrNotAvailable

// searchPaths are the locations a non-root PATH commonly omits.
var searchPaths = map[string][]string{
	"dnsmasq":    {"/usr/sbin/dnsmasq", "/sbin/dnsmasq", "/usr/bin/dnsmasq"},
	"kea-dhcp4":  {"/usr/sbin/kea-dhcp4", "/usr/bin/kea-dhcp4"},
	"networkctl": {"/usr/bin/networkctl", "/bin/networkctl"},
	"systemctl":  {"/usr/bin/systemctl", "/bin/systemctl"},
	"install":    {"/usr/bin/install", "/bin/install"},
	"cat":        {"/usr/bin/cat", "/bin/cat"},
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
	// networkctl reads systemd-networkd's own DHCP server — the units, the
	// version and the leases it has offered — and reloads it after a write.
	networkctl *runner.Runner
	// cat is the escalated fallback for a lease or config file a plain read
	// cannot open.
	cat *runner.Runner

	// now is the clock the lease expiries are measured against. A field so a
	// test can pin it.
	now func() time.Time

	// units caches the networkd .network units the last Load read, because a
	// mutation is rendered against the configuration that is in effect: the
	// drop-in overrides the unit wholesale, so it has to be written from the
	// merged picture rather than from its own previous contents. The mutex
	// guards it against the read running in one goroutine and the build in
	// another.
	mu    sync.Mutex
	units []NetworkdUnit
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
	// systemd-networkd is last, and it is detected by configuration rather
	// than by a binary: networkctl is present on every systemd machine, but it
	// only serves DHCP where a .network unit says so. Looking for the section
	// keeps a machine that merely runs networkd out of the DHCP screen.
	if len(networkdUnits(context.Background(), nil)) > 0 {
		return dhcp.KindNetworkd
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
	// networkctl reads unprivileged (the version, the status and the leases a
	// link's server has offered) and escalates only for the reload that
	// applies a write.
	r.networkctl, _ = runner.New(runner.Options{
		Bin: "networkctl", SearchPaths: searchPaths["networkctl"],
		SudoPrefix: sudoPrefix, PrivilegedReads: &unprivileged,
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
	case dhcp.KindNetworkd:
		return "systemd-networkd's own DHCP server"
	default:
		return "no DHCP server"
	}
}

// Capabilities reports which mutations this backend offers: dnsmasq is
// editable, Kea and a serverless machine are read-only.
func (r *Real) Capabilities() dhcp.Capabilities {
	switch r.kind {
	case dhcp.KindDnsmasq:
		return dnsmasqCapabilities
	case dhcp.KindNetworkd:
		caps := networkdCapabilities
		// Both files are the one drop-in, and its name is only known once a
		// unit has been read; before the first Load the screen shows the shape
		// of the path rather than a wrong one.
		caps.ManagedFile = r.networkdDropinPath()
		caps.OptionsFile = caps.ManagedFile
		return caps
	default:
		return dhcp.Capabilities{}
	}
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
	case "networkctl":
		return r.networkctl
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
	case dhcp.KindNetworkd:
		return r.loadNetworkd(ctx), nil
	default:
		return dhcp.Model{Server: dhcp.Server{
			Kind: dhcp.KindNone,
			Explain: "no DHCP server was found: dnsmasq and ISC Kea are not " +
				"installed, and no .network unit declares DHCPServer=yes",
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
		options := ParseDnsmasqOptions(path, raw)
		model.Options = MergeOptions(model.Options, options)
		if path == DnsmasqOptionsFile {
			model.OwnOptions = options
		}
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
	if r.kind == dhcp.KindNetworkd {
		return r.networkdAddReservation(res)
	}
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
	if r.kind == dhcp.KindNetworkd {
		return r.networkdRemoveReservation(res)
	}
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
	if r.kind == dhcp.KindNetworkd {
		return r.networkdSetPoolRange(orig, newStart, newEnd)
	}
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

// BuildSetOptions renders the tool-owned options drop-in in full. The file is
// regenerated from the form, so the diff is against whatever the drop-in says
// now — and only that file: options set by hand in dnsmasq.conf are never
// edited. dnsmasq does not re-read configuration files on SIGHUP, so the plan
// restarts the service rather than reloading it.
func (r *Real) BuildSetOptions(o dhcp.Options) (dhcp.WritePlan, error) {
	if r.kind == dhcp.KindNetworkd {
		return r.networkdSetOptions(o)
	}
	if err := r.requireDnsmasq(); err != nil {
		return dhcp.WritePlan{}, err
	}
	before := r.readOrEmpty(DnsmasqOptionsFile)
	after, err := RenderOptionsFile(o)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	return writePlan(DnsmasqOptionsFile, before, after, true, stageFile)
}

// requireDnsmasq refuses a mutation the dnsmasq path cannot make: Kea is
// read-only in this phase, and a machine with no server has nothing to change.
// The networkd server has its own path and never reaches here.
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

// networkdUnits reads every .network unit on the machine that runs a DHCP
// server, with its drop-ins folded in, in the order systemd-networkd reads
// them: a unit name an earlier search directory claims wins, and the drop-ins
// of that name are applied from every directory, /usr/lib first and /etc last.
//
// cat is the escalated fallback for a unit a plain read cannot open — netplan
// renders its files into /run as mode 0640 — and may be nil, in which case
// such a unit is simply not seen.
func networkdUnits(ctx context.Context, cat *runner.Runner) []NetworkdUnit {
	var units []NetworkdUnit
	seen := map[string]bool{}
	for _, dir := range networkdConfigDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, networkSuffix) || seen[name] {
				continue
			}
			seen[name] = true
			path := filepath.Join(dir, name)
			raw, err := readNetworkdFile(ctx, cat, path)
			if err != nil {
				continue
			}
			files := append([]NetworkdFile{{Path: path, Raw: raw}},
				networkdDropinFiles(ctx, cat, name)...)
			unit := ParseNetworkdUnit(files)
			if (unit.HasSection || unit.Enabled) && unit.HasSubnet() {
				units = append(units, unit)
			}
		}
	}
	sortNetworkdUnits(units)
	return units
}

// networkdDropinFiles reads the drop-ins of one unit name, in networkd's own
// order: the search directories from the most general to the most specific, and
// inside each one the files sorted by name.
func networkdDropinFiles(ctx context.Context, cat *runner.Runner,
	unitName string) []NetworkdFile {
	var files []NetworkdFile
	for i := len(networkdConfigDirs) - 1; i >= 0; i-- {
		dir := filepath.Join(networkdConfigDirs[i], unitName+".d")
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".conf") {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(dir, name)
			raw, err := readNetworkdFile(ctx, cat, path)
			if err != nil {
				continue
			}
			files = append(files, NetworkdFile{Path: path, Raw: raw})
		}
	}
	return files
}

// readNetworkdFile reads one unit or drop-in, escalating with `cat` only when a
// plain read is refused for want of privilege.
func readNetworkdFile(ctx context.Context, cat *runner.Runner, path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the path comes from systemd's own search directories
	if err == nil {
		return string(raw), nil
	}
	if !os.IsPermission(err) || cat == nil {
		return "", err
	}
	return cat.Read(ctx, "cat", "--", path)
}

// loadNetworkd reads systemd-networkd's own DHCP server: the units that
// declare one, the pool each hands out, the static leases, and the leases the
// server has actually offered.
func (r *Real) loadNetworkd(ctx context.Context) dhcp.Model {
	units := networkdUnits(ctx, r.cat)
	r.setUnits(units)

	model := NetworkdModel(units)
	server := dhcp.Server{
		Kind:    dhcp.KindNetworkd,
		Present: true,
		Active:  r.serviceActive(ctx, networkdUnitName),
		Version: r.networkdVersion(ctx),
	}
	for _, unit := range units {
		server.ConfPaths = append(server.ConfPaths, unit.Path)
		server.ConfPaths = append(server.ConfPaths, unit.Dropins...)
		model.Leases = append(model.Leases, r.networkdLeases(ctx, unit)...)
	}
	if len(units) > 0 {
		server.ManagedFile = NetworkdDropinPath(units[0].Path)
	}
	server.Explain = explainNetworkd(units)
	if server.Explain == "" {
		server.Explain = explainServer(server, len(model.Pools))
	}
	model.Server = server
	return model
}

// networkdLeases reads the leases one unit's server has offered. They come from
// `networkctl status`, which renders a bus property into its table; the JSON
// output does not carry them, so this is the read path on every systemd.
func (r *Real) networkdLeases(ctx context.Context, unit NetworkdUnit) []dhcp.Lease {
	if r.networkctl == nil || unit.Link == "" || !unit.Enabled {
		return nil
	}
	out, err := r.networkctl.Read(ctx, "networkctl", "status", "--no-pager", unit.Link)
	if err != nil {
		return nil
	}
	return ParseNetworkctlLeases(out)
}

// networkdVersion reads the systemd version behind the DHCP server, empty when
// it cannot.
func (r *Real) networkdVersion(ctx context.Context) string {
	if r.networkctl == nil {
		return ""
	}
	out, err := r.networkctl.Read(ctx, "networkctl", "--version")
	if err != nil {
		return ""
	}
	// "systemd 257 (257.13-1.fc42)" — the second field is the version.
	fields := strings.Fields(runner.FirstLine(strings.TrimSpace(out)))
	if len(fields) >= 2 && strings.EqualFold(fields[0], "systemd") {
		return fields[1]
	}
	return ""
}

// setUnits records what the last read found, for the mutations to render
// against.
func (r *Real) setUnits(units []NetworkdUnit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.units = units
}

// primaryUnit is the unit a mutation targets by default: the first
// DHCP-serving unit in path order, which on a router with one LAN is the only
// one there is.
func (r *Real) primaryUnit() (NetworkdUnit, error) {
	return r.unitFor("")
}

// unitFor is the unit a mutation targets. A pool carries the unit it was read
// from, so an edit on a machine with more than one LAN changes the pool the
// user was looking at rather than the first one; anything else takes the
// primary.
func (r *Real) unitFor(path string) (NetworkdUnit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.units) == 0 {
		return NetworkdUnit{}, fmt.Errorf(
			"networkd: no .network unit here declares a DHCP server to change")
	}
	for _, unit := range r.units {
		if path != "" && unit.Path == path {
			return unit, nil
		}
	}
	return r.units[0], nil
}

// networkdDropinPath is where a change lands, or the shape of that path before
// a unit has been read.
func (r *Real) networkdDropinPath() string {
	unit, err := r.primaryUnit()
	if err != nil {
		return NetworkdDropinPath("<unit>" + networkSuffix)
	}
	return NetworkdDropinPath(unit.Path)
}

// networkdSpec builds the drop-in as it stands today: the pool and the
// advertised options that are in effect across the unit and its drop-ins, and
// the static leases this tool's own drop-in declares.
//
// The options are the effective ones rather than the drop-in's, because the
// drop-in is rendered whole and clears each list before setting it: seeding
// from the drop-in alone would drop a DNS= the unit sets the first time any
// other field was changed. The leases are the opposite — a
// [DHCPServerStaticLease] section is additive, so re-declaring the unit's own
// would hand two leases to one client.
func (r *Real) networkdSpec(unitPath string) (NetworkdDropin, NetworkdUnit, error) {
	unit, err := r.unitFor(unitPath)
	if err != nil {
		return NetworkdDropin{}, NetworkdUnit{}, err
	}
	dropin := NetworkdDropinPath(unit.Path)
	spec := NetworkdDropin{
		Link:       unit.Link,
		PoolOffset: unit.PoolOffset,
		PoolSize:   unit.PoolSize,
		Options:    NetworkdOptions(unit),
	}
	for _, lease := range unit.Leases {
		if lease.Source == dropin {
			spec.Leases = append(spec.Leases, lease)
		}
	}
	return spec, unit, nil
}

// buildNetworkdDropin renders a spec and returns the previewed plan: the diff
// against the drop-in on disk, and the install and reload that apply it.
func (r *Real) buildNetworkdDropin(spec NetworkdDropin,
	unit NetworkdUnit) (dhcp.WritePlan, error) {
	path := NetworkdDropinPath(unit.Path)
	after, err := RenderNetworkdDropin(spec)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	return networkdWritePlan(path, r.readOrEmpty(path), after, stageFile)
}

// networkdAddReservation adds a static lease to the drop-in.
func (r *Real) networkdAddReservation(res dhcp.Reservation) (dhcp.WritePlan, error) {
	spec, unit, err := r.networkdSpec("")
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	// The clash check is against every lease the unit hands out, not only the
	// drop-in's, while the section written is the drop-in's own.
	lease, err := NewNetworkdLease(unit.Leases, res)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	spec.Leases = append(spec.Leases, lease)
	return r.buildNetworkdDropin(spec, unit)
}

// networkdRemoveReservation takes a static lease out of the drop-in. A lease
// the unit itself declares cannot be removed this way — a drop-in can add a
// [DHCPServerStaticLease] section but not take one back — so it is refused
// with the file to edit named.
func (r *Real) networkdRemoveReservation(res dhcp.Reservation) (dhcp.WritePlan, error) {
	spec, unit, err := r.networkdSpec("")
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	dropin := NetworkdDropinPath(unit.Path)
	if res.Source != "" && res.Source != dropin {
		return dhcp.WritePlan{}, fmt.Errorf(
			"networkd: %s is declared in %s, which tui-network does not rewrite; "+
				"a drop-in can add a static lease but not remove one",
			leaseLabel(res), res.Source)
	}
	spec.Leases, err = RemoveNetworkdLease(spec.Leases, res)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	return r.buildNetworkdDropin(spec, unit)
}

// networkdSetPoolRange turns a first and last address into the PoolOffset= and
// PoolSize= that hand out exactly that range of the LAN's subnet.
func (r *Real) networkdSetPoolRange(orig dhcp.Pool,
	newStart, newEnd string) (dhcp.WritePlan, error) {
	spec, unit, err := r.networkdSpec(orig.Source)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	offset, size, err := PoolOffsetSize(unit.Address, newStart, newEnd)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	spec.PoolOffset, spec.PoolSize = offset, size
	return r.buildNetworkdDropin(spec, unit)
}

// networkdSetOptions rewrites what the server advertises and how long a lease
// lasts.
func (r *Real) networkdSetOptions(o dhcp.Options) (dhcp.WritePlan, error) {
	spec, unit, err := r.networkdSpec("")
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	spec.Options = o
	return r.buildNetworkdDropin(spec, unit)
}
