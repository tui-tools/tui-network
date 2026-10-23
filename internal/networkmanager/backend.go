// Package networkmanager is the NetworkManager backend of tui-network. Like
// the systemd-networkd backend, it is the only place its programs are started,
// and everything it starts goes through the kit runner, so the command a
// confirm dialog previews is the command that runs.
//
// Five programs are driven:
//
//	nmcli       devices, profiles and every change to them
//	ip          the kernel routing table and interface indexes, in JSON
//	resolvectl  the global resolver view and the cache flush
//	journalctl  what NetworkManager said about a device
//	busctl      NetworkManager's checkpoints, the automatic rollback that
//	            wraps a change which can cut the session
//
// Reads never escalate. Changes go through the configured prefix.
package networkmanager

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/runner"
	"github.com/tui-tools/tui-network/internal/network"
	"github.com/tui-tools/tui-network/internal/networkd"
)

// searchPaths are the locations a non-root PATH commonly omits.
var searchPaths = map[string][]string{
	"nmcli":      {"/usr/bin/nmcli", "/bin/nmcli"},
	"ip":         {"/usr/sbin/ip", "/sbin/ip", "/usr/bin/ip"},
	"resolvectl": {"/usr/bin/resolvectl", "/bin/resolvectl"},
	"journalctl": {"/usr/bin/journalctl", "/bin/journalctl"},
	"busctl":     {"/usr/bin/busctl", "/bin/busctl"},
}

// installHint is appended to the "not found" error.
const installHint = "it ships with NetworkManager; " +
	"or use --demo=networkmanager to explore the UI"

// journalLines bounds how much of NetworkManager's log the detail view pulls.
const journalLines = "200"

// maxProfiles bounds how many profiles are read in full. Each is one nmcli
// call, and a laptop that has joined a hundred Wi-Fi networks should not pay
// for all of them on every reload: profiles bound to a device come first.
const maxProfiles = 32

// Available reports whether nmcli is installed on this host.
func Available() bool {
	return runner.Available("nmcli", searchPaths["nmcli"]...)
}

// Real drives NetworkManager on the host. It satisfies network.Backend and
// network.Guard.
type Real struct {
	nmcli      *runner.Runner
	ip         *runner.Runner
	resolvectl *runner.Runner
	journalctl *runner.Runner
	busctl     *runner.Runner

	caps compat.Caps
	// foreign names another network manager that is running too, as the
	// backend selection found it ("systemd-networkd").
	foreign string
	// canWrite reports whether a change can run at all: as root, or through
	// an escalation prefix. Without either every link is read-only.
	canWrite bool
}

// Options are what the backend selection knows about the machine that the
// backend would otherwise have to probe again.
type Options struct {
	// Foreign names another manager that is running too.
	Foreign string
}

// NewReal locates the binaries. sudoPrefix comes from the configuration
// ("sudo -n"); nil runs the commands directly.
func NewReal(sudoPrefix []string, caps compat.Caps, opts Options) (*Real, error) {
	real := &Real{caps: caps, foreign: opts.Foreign}
	unprivileged := false
	for _, spec := range []struct {
		bin    string
		target **runner.Runner
		reads  *bool
	}{
		{"nmcli", &real.nmcli, &unprivileged},
		{"ip", &real.ip, &unprivileged},
		{"resolvectl", &real.resolvectl, &unprivileged},
		{"journalctl", &real.journalctl, &unprivileged},
		{"busctl", &real.busctl, nil},
	} {
		r, err := runner.New(runner.Options{
			Bin:             spec.bin,
			SearchPaths:     searchPaths[spec.bin],
			SudoPrefix:      sudoPrefix,
			InstallHint:     installHint,
			PrivilegedReads: spec.reads,
		})
		if err != nil {
			// Only nmcli is essential; the rest degrade where they are used.
			if spec.bin == "nmcli" {
				return nil, err
			}
			continue
		}
		*spec.target = r
	}
	real.canWrite = os.Geteuid() == 0 || len(sudoPrefix) > 0
	return real, nil
}

// Name identifies the backend.
func (r *Real) Name() string { return Name }

// Describe names the backend for the header.
func (r *Real) Describe() string { return r.nmcli.Describe() }

// Capabilities reports what this backend supports. The cache flush is
// systemd-resolved's, so it is only offered where resolvectl exists.
func (r *Real) Capabilities() network.Capabilities {
	caps := capabilities
	caps.SupportsFlushCaches = r.resolvectl != nil
	return caps
}

// runnerFor picks the runner that owns a command, by its argv[0].
func (r *Real) runnerFor(cmd network.Command) *runner.Runner {
	if len(cmd.Argv) == 0 {
		return nil
	}
	switch cmd.Argv[0] {
	case "nmcli":
		return r.nmcli
	case "ip":
		return r.ip
	case "resolvectl":
		return r.resolvectl
	case "journalctl":
		return r.journalctl
	case "busctl":
		return r.busctl
	default:
		return nil
	}
}

// Preview renders the exact command line Run will execute.
func (r *Real) Preview(cmd network.Command) string {
	if run := r.runnerFor(cmd); run != nil {
		return run.Preview(cmd)
	}
	return cmd.String()
}

// Run executes a previewed command.
func (r *Real) Run(ctx context.Context, cmd network.Command) (string, error) {
	run := r.runnerFor(cmd)
	if run == nil {
		name := "(empty command)"
		if len(cmd.Argv) > 0 {
			name = cmd.Argv[0]
		}
		return "", fmt.Errorf("networkmanager: %q is not available on this machine", name)
	}
	return run.Run(ctx, cmd)
}

// Load reads the machine's network state from NetworkManager.
//
// Only the device read is essential. Profiles, routes, indexes and the
// resolver view each degrade on their own, so a machine where one of them
// fails still shows its links.
func (r *Real) Load(ctx context.Context) (network.Model, error) {
	running, err := r.nmcli.Read(ctx, "nmcli", "-t", "-f", "RUNNING", "general")
	if err != nil || strings.TrimSpace(running) != "running" {
		return network.Model{}, fmt.Errorf("networkmanager: NetworkManager is not " +
			"running; start it, or pick the other backend with --backend networkd")
	}
	out, err := r.nmcli.Read(ctx, "nmcli", "-t", "-f", "GENERAL,IP4,DHCP4,IP6",
		"device", "show")
	if err != nil {
		return network.Model{}, err
	}
	devices := ParseDeviceShow(out)
	profiles := r.loadProfiles(ctx, devices)

	indexes := map[string]int{}
	if r.ip != nil {
		if out, err := r.ip.Read(ctx, "ip", "-j", "link"); err == nil {
			if parsed, err := ParseLinkIndexes(out); err == nil {
				indexes = parsed
			}
		}
	}

	model := BuildModel(devices, profiles, indexes)
	model.ForeignManager = r.foreign
	if r.ip != nil {
		if out, err := r.ip.Read(ctx, "ip", "-j", "route"); err == nil {
			if routes, err := networkd.ParseRoutesJSON(out); err == nil {
				model.Routes = routes
			}
		}
	}
	if r.resolvectl != nil {
		if out, err := r.resolvectl.Read(ctx, "resolvectl", "dns"); err == nil {
			model.ResolvedRunning = true
			_, model.GlobalDNS = networkd.ParseResolvectlDNS(out)
		}
	}
	if !r.canWrite {
		markReadOnly(&model, "tui-network runs as a normal user with "+
			"escalation turned off, so it cannot change anything here")
	}
	return model, nil
}

// markReadOnly makes every link read-only with one reason.
func markReadOnly(model *network.Model, reason string) {
	for i := range model.Links {
		if model.Links[i].Managed {
			model.Links[i].Managed = false
			model.Links[i].ReadOnlyReason = reason
		}
	}
}

// loadProfiles lists the profiles and reads the settings of the ones that
// matter: those active on a device first, then those bound to a device by
// name, up to maxProfiles.
func (r *Real) loadProfiles(ctx context.Context, devices []Device) []Profile {
	out, err := r.nmcli.Read(ctx, "nmcli", "-t", "-f", "NAME,UUID,TYPE,DEVICE,FILENAME",
		"connection", "show")
	if err != nil {
		return nil
	}
	profiles := ParseConnectionList(out)
	read := 0
	for i := range profiles {
		if read >= maxProfiles {
			break
		}
		if profiles[i].Device == "" || checkUUID(profiles[i].UUID) != nil {
			continue
		}
		r.readSettings(ctx, &profiles[i])
		read++
	}
	return profiles
}

// readSettings reads one profile's settings.
func (r *Real) readSettings(ctx context.Context, profile *Profile) {
	out, err := r.nmcli.Read(ctx, "nmcli", "-t", "-f", "connection,ipv4,ipv6",
		"connection", "show", "uuid", profile.UUID)
	if err != nil {
		return
	}
	profile.Settings = ParseProfileSettings(out)
}

// LoadLink re-reads one link in full. NetworkManager's device read already
// carries everything the detail screen shows, so it is the same read.
func (r *Real) LoadLink(ctx context.Context, name string) (network.Link, error) {
	if err := checkLink(name); err != nil {
		return network.Link{}, err
	}
	model, err := r.Load(ctx)
	if err != nil {
		return network.Link{}, err
	}
	link, ok := model.Link(name)
	if !ok {
		return network.Link{}, fmt.Errorf("networkmanager: no device named %q", name)
	}
	return link, nil
}

// Journal returns NetworkManager's recent log lines about a device.
func (r *Real) Journal(ctx context.Context, link string) ([]string, error) {
	if err := checkLink(link); err != nil {
		return nil, err
	}
	if r.journalctl == nil {
		return nil, fmt.Errorf("networkmanager: the journalctl command is not available")
	}
	out, err := r.journalctl.Read(ctx, "journalctl", "-u", "NetworkManager",
		"--no-pager", "-n", journalLines, "--grep", link)
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// splitLines splits output into lines, without the trailing empty one.
func splitLines(text string) []string {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// activeProfile asks NetworkManager which profile is active on a device. The
// builders that need it are called with a link name only, and the answer
// must be the one in effect when the operator pressed the key.
func (r *Real) activeProfile(link string) (string, error) {
	if err := checkLink(link); err != nil {
		return "", err
	}
	out, err := r.nmcli.Read(context.Background(), "nmcli", "-g", "GENERAL.CON-UUID",
		"device", "show", link)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// BuildLinkAction builds connect, disconnect, reapply or re-activate.
func (r *Real) BuildLinkAction(action, link string) (network.Command, error) {
	uuid := ""
	if action == network.ActionRenew {
		var err error
		if uuid, err = r.activeProfile(link); err != nil {
			return network.Command{}, err
		}
	}
	return BuildLinkAction(action, link, uuid)
}

// BuildReload re-reads the profiles from disk.
func (r *Real) BuildReload() (network.Command, error) { return BuildReload() }

// BuildFlushCaches empties the resolver cache.
func (r *Real) BuildFlushCaches() (network.Command, error) {
	if r.resolvectl == nil {
		return network.Command{}, fmt.Errorf(
			"networkmanager: systemd-resolved is not installed, so there is no cache to flush")
	}
	return BuildFlushCaches()
}

// BuildSetDNS sets a device's DNS servers at runtime.
func (r *Real) BuildSetDNS(link string, servers []string) (network.Command, error) {
	return BuildSetDNS(link, servers)
}

// BuildSetDomains sets a device's search domains at runtime.
func (r *Real) BuildSetDomains(link string, domains []string) (network.Command, error) {
	return BuildSetDomains(link, domains)
}

// SpecFor seeds the editor from the profile active on the link.
func (r *Real) SpecFor(model network.Model, link network.Link) network.FileSpec {
	return SpecFor(model, link)
}

// BuildWriteConfig builds the profile change the editor asked for: the
// settings diff and the commands that write and activate it.
func (r *Real) BuildWriteConfig(spec network.FileSpec) (network.WritePlan, error) {
	current := []network.Setting{}
	if spec.Connection != "" {
		if err := checkUUID(spec.Connection); err != nil {
			return network.WritePlan{}, err
		}
		out, err := r.nmcli.Read(context.Background(), "nmcli", "-t", "-f",
			"connection,ipv4,ipv6", "connection", "show", "uuid", spec.Connection)
		if err != nil {
			return network.WritePlan{}, err
		}
		current = ParseProfileSettings(out)
	} else {
		kind, err := r.nmcli.Read(context.Background(), "nmcli", "-g", "GENERAL.TYPE",
			"device", "show", spec.MatchName)
		if err != nil {
			return network.WritePlan{}, err
		}
		if strings.TrimSpace(kind) != "ethernet" {
			return network.WritePlan{}, fmt.Errorf(
				"networkmanager: %s has no profile, and tui-network only creates "+
					"wired ones; create it with nmcli", spec.MatchName)
		}
	}
	return PlanWriteConfig(spec, current)
}

// Egress resolves how the kernel would reach a gateway.
func (r *Real) Egress(ctx context.Context, gw network.Gateway) (network.Egress, error) {
	if r.ip == nil {
		return network.Egress{}, fmt.Errorf("networkmanager: the ip command is not available")
	}
	args := []string{"ip", "-j"}
	if gw.Family == "ipv6" {
		args = append(args, "-6")
	}
	args = append(args, "route", "get", gw.Address)
	out, err := r.ip.Read(ctx, args...)
	if err != nil {
		return network.Egress{}, err
	}
	return networkd.ParseRouteGet(out)
}

// BuildSetDefaultGateway builds the runtime default-route switch.
func (r *Real) BuildSetDefaultGateway(gw network.Gateway, metric int) (network.Command, error) {
	return BuildSetDefaultGateway(gw, metric)
}

// BuildPersistGateway writes the uplink's priority into its profile.
func (r *Real) BuildPersistGateway(gw network.Gateway, metric int) (network.WritePlan, error) {
	current := ""
	if gw.ConfigID != "" && checkUUID(gw.ConfigID) == nil {
		out, err := r.nmcli.Read(context.Background(), "nmcli", "-g",
			routeMetricKey(gw.Family), "connection", "show", "uuid", gw.ConfigID)
		if err == nil {
			current = strings.TrimSpace(out)
		}
	}
	return PlanPersistGateway(gw, metric, current)
}

// BuildCreateNetdev is not offered on NetworkManager yet.
func (r *Real) BuildCreateNetdev(network.Model, network.NetdevSpec) (network.WritePlan, error) {
	return network.WritePlan{}, errNoNetdev
}

// BuildRemoveNetdev is not offered on NetworkManager yet.
func (r *Real) BuildRemoveNetdev(network.Model, string) (network.WritePlan, error) {
	return network.WritePlan{}, errNoNetdev
}

// errNoNetdev is the refusal for the VLAN and bridge keys.
var errNoNetdev = fmt.Errorf("networkmanager: VLANs and bridges are not built " +
	"on NetworkManager yet; create them with nmcli")

// BuildArm takes a checkpoint, when busctl is there to ask for one.
func (r *Real) BuildArm(timeout time.Duration) (network.Command, error) {
	if r.busctl == nil {
		return network.Command{}, fmt.Errorf(
			"networkmanager: busctl is not installed, so no checkpoint can be taken")
	}
	return BuildArm(timeout)
}

// ArmedID reads the checkpoint from CheckpointCreate's answer.
func (r *Real) ArmedID(output string) (string, error) { return ArmedID(output) }

// BuildKeep discards the checkpoint.
func (r *Real) BuildKeep(id string) (network.Command, error) { return BuildKeep(id) }

// BuildRollback restores the checkpoint.
func (r *Real) BuildRollback(id string) (network.Command, error) { return BuildRollback(id) }

// SpecFor seeds the guided editor from the profile active on a link. The
// method, address, gateway and DNS come from the profile, not from what the
// device carries now, because the profile is what the editor rewrites.
func SpecFor(model network.Model, link network.Link) network.FileSpec {
	spec := network.FileSpec{MatchName: link.Name, DHCP: MethodAuto}
	file, ok := model.ConfigFor(link)
	if !ok || file.ID == "" {
		return spec
	}
	spec.Connection = file.ID
	spec.Path = file.Path
	if method, ok := file.Get("ipv4", "method"); ok {
		spec.DHCP = method
	}
	if addresses, ok := file.Get("ipv4", "addresses"); ok {
		first, _, _ := strings.Cut(addresses, ",")
		spec.Address = strings.TrimSpace(first)
	}
	if gateway, ok := file.Get("ipv4", "gateway"); ok {
		spec.Gateway = gateway
	}
	for _, family := range []string{"ipv4", "ipv6"} {
		if dns, ok := file.Get(family, "dns"); ok {
			spec.DNS = append(spec.DNS, splitList(dns)...)
		}
	}
	if search, ok := file.Get("ipv4", "dns-search"); ok {
		spec.Domains = splitList(search)
	}
	return spec
}

// splitList splits one of nmcli's comma-separated lists.
func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// PlanWriteConfig turns a validated spec and the profile's current settings
// into the plan the confirm dialog shows: the settings diff, and either the
// modify-and-activate pair or, for a device with no profile, the
// create-and-connect pair. It is shared by the real backend and the fake.
func PlanWriteConfig(spec network.FileSpec, current []network.Setting) (network.WritePlan, error) {
	if err := checkLink(spec.MatchName); err != nil {
		return network.WritePlan{}, err
	}
	settings, err := SpecSettings(spec)
	if err != nil {
		return network.WritePlan{}, err
	}
	path := spec.Path
	if path == "" {
		path = "new profile " + spec.MatchName
	}
	diff := SettingsDiff(path, current, settings)
	if diff == "" {
		return network.WritePlan{}, fmt.Errorf("the profile of %s already says exactly this",
			spec.MatchName)
	}
	var commands []network.Command
	if spec.Connection == "" {
		commands, err = BuildAddProfile(spec.MatchName, settings)
	} else {
		commands, err = BuildModifyProfile(spec.Connection, spec.MatchName, settings)
	}
	if err != nil {
		return network.WritePlan{}, err
	}
	return network.WritePlan{
		Path:     path,
		Content:  RenderSettings(settings),
		Diff:     diff,
		Commands: commands,
		Note:     "The profile is written first and then activated on " + spec.MatchName + ".",
	}, nil
}

// PlanPersistGateway builds the plan that keeps an uplink's priority in its
// profile, with a one-setting diff against the metric it holds today.
func PlanPersistGateway(gw network.Gateway, metric int, current string) (network.WritePlan, error) {
	commands, err := BuildPersistGateway(gw, metric)
	if err != nil {
		return network.WritePlan{}, err
	}
	key := routeMetricKey(gw.Family)
	section, name, _ := strings.Cut(key, ".")
	want := []network.Setting{{Section: section, Key: name, Value: fmt.Sprint(metric)}}
	var have []network.Setting
	if current != "" {
		have = []network.Setting{{Section: section, Key: name, Value: current}}
	}
	if current == fmt.Sprint(metric) {
		return network.WritePlan{}, fmt.Errorf("the profile of %s already keeps metric %d",
			gw.Interface, metric)
	}
	path := gw.ConfigFile
	if path == "" {
		path = "profile " + gw.ConfigID
	}
	return network.WritePlan{
		Path:     path,
		Content:  RenderSettings(want),
		Diff:     SettingsDiff(path, have, want),
		Commands: commands,
		Note: "The metric is kept in the profile and applied to " + gw.Interface +
			" now with `nmcli device reapply`.",
	}, nil
}
