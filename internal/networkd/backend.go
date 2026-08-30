// Package networkd is the systemd-networkd backend of tui-network, and the
// only place in the repository that starts a process.
//
// Everything about reaching the machine — resolving the binaries, applying the
// privilege prefix, bounding each call, turning a failure into one readable
// line — belongs to the kit runner. What is left here is the translation
// between systemd's output and the backend-neutral model in internal/network,
// and the assembly of the argv that a confirm dialog will show before it runs.
//
// Four programs are driven, each through its own runner:
//
//	networkctl   links, their configuration and the link verbs
//	resolvectl   DNS servers, search domains and the resolver cache
//	ip           the kernel routing table, in JSON
//	journalctl   what networkd said about a link
//
// A fifth, `install`, copies a staged .network file into place; it is the only
// way this tool writes to /etc, and the copy is previewed like every other
// change.
package networkd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/runner"
	"github.com/tui-tools/tui-network/internal/network"
)

// ErrNotAvailable reports that the networkd backend cannot be used on this
// machine (networkctl missing, or no non-interactive privilege escalation).
var ErrNotAvailable = runner.ErrNotAvailable

// searchPaths are the locations a non-root PATH commonly omits.
var searchPaths = map[string][]string{
	"networkctl": {"/usr/bin/networkctl", "/bin/networkctl"},
	"resolvectl": {"/usr/bin/resolvectl", "/bin/resolvectl"},
	"ip":         {"/usr/sbin/ip", "/sbin/ip", "/usr/bin/ip"},
	"journalctl": {"/usr/bin/journalctl", "/bin/journalctl"},
	"install":    {"/usr/bin/install", "/bin/install"},
}

// installHint is appended to the "not found" error.
const installHint = "it ships with systemd; " +
	"or use --demo to explore the UI"

// nmRuntimeDir exists while NetworkManager is running. It is read rather than
// probed with a command, because "is another manager running" is a question
// about the machine, not a change to it, and a stat needs no process.
const nmRuntimeDir = "/run/NetworkManager"

// journalLines bounds how much of networkd's log the detail view pulls.
const journalLines = "200"

// Real drives systemd-networkd on the host. It satisfies network.Backend.
type Real struct {
	networkctl *runner.Runner
	resolvectl *runner.Runner
	ip         *runner.Runner
	journalctl *runner.Runner
	install    *runner.Runner

	// caps gates the reads that only exist on a new enough systemd. It comes
	// from the manifest, so no version number is written into this file.
	caps compat.Caps
}

// Available reports whether networkctl is installed on this host.
func Available() bool {
	return runner.Available("networkctl", searchPaths["networkctl"]...)
}

// NewReal locates the binaries and, when not running as root, validates the
// configured privilege prefix. sudoPrefix comes from the configuration
// ("sudo -n"); pass nil to run the commands directly.
//
// Reads are unprivileged: `networkctl list`, `resolvectl dns` and `ip route`
// all answer to any user, and a tool that escalated to read would be asking
// for a password it does not need.
func NewReal(sudoPrefix []string, caps compat.Caps) (*Real, error) {
	real := &Real{caps: caps}
	unprivileged := false
	for _, spec := range []struct {
		bin    string
		target **runner.Runner
		reads  *bool
	}{
		{"networkctl", &real.networkctl, &unprivileged},
		{"resolvectl", &real.resolvectl, &unprivileged},
		{"ip", &real.ip, &unprivileged},
		{"journalctl", &real.journalctl, &unprivileged},
		{"install", &real.install, nil},
	} {
		r, err := runner.New(runner.Options{
			Bin:             spec.bin,
			SearchPaths:     searchPaths[spec.bin],
			SudoPrefix:      sudoPrefix,
			InstallHint:     installHint,
			PrivilegedReads: spec.reads,
		})
		if err != nil {
			// Only networkctl is essential: the tool still reads links and
			// still previews commands without the others, and says so where
			// the missing data would have been.
			if spec.bin == "networkctl" {
				return nil, err
			}
			continue
		}
		*spec.target = r
	}
	return real, nil
}

// Name identifies the backend. It is the manifest's backend name, which is
// what the version probe is keyed on.
func (r *Real) Name() string { return "systemd-networkd" }

// Describe names the backend for the header.
func (r *Real) Describe() string { return r.networkctl.Describe() }

// Capabilities reports what this backend supports.
func (r *Real) Capabilities() network.Capabilities { return capabilities }

// Preview renders the exact command line Run will execute. Every command goes
// through the runner of its own binary, so the preview carries the privilege
// prefix that binary will really be called with.
func (r *Real) Preview(cmd network.Command) string {
	if run := r.runnerFor(cmd); run != nil {
		return run.Preview(cmd)
	}
	return cmd.String()
}

// runnerFor picks the runner that owns a command, by its argv[0].
func (r *Real) runnerFor(cmd network.Command) *runner.Runner {
	if len(cmd.Argv) == 0 {
		return nil
	}
	switch cmd.Argv[0] {
	case "networkctl":
		return r.networkctl
	case "resolvectl":
		return r.resolvectl
	case "ip":
		return r.ip
	case "journalctl":
		return r.journalctl
	case "install":
		return r.install
	default:
		return nil
	}
}

// Run executes a previewed command.
func (r *Real) Run(ctx context.Context, cmd network.Command) (string, error) {
	run := r.runnerFor(cmd)
	if run == nil {
		return "", fmt.Errorf("networkd: %q is not available on this machine",
			firstArg(cmd))
	}
	return run.Run(ctx, cmd)
}

// firstArg names the binary a command wanted, for an error message.
func firstArg(cmd network.Command) string {
	if len(cmd.Argv) == 0 {
		return "(empty command)"
	}
	return cmd.Argv[0]
}

// Load reads the machine's network state.
//
// The read is layered, and every layer is allowed to fail on its own: a
// machine where networkd is not running still shows its links and routes, and
// says in the header why nothing is managed. Only a total failure to list the
// links is an error.
func (r *Real) Load(ctx context.Context) (network.Model, error) {
	model := network.Model{Backend: r.Name()}

	links, running, err := r.loadLinks(ctx)
	if err != nil {
		return network.Model{}, err
	}
	model.Links, model.Running = links, running

	if _, statErr := os.Stat(nmRuntimeDir); statErr == nil {
		model.ForeignManager = "NetworkManager"
	}

	if routes, routeErr := r.loadRoutes(ctx); routeErr == nil {
		model.Routes = routes
	}
	r.loadDNS(ctx, &model)
	model.ConfigFiles = LoadConfigFiles(model.Links)
	r.markUnmanaged(&model)
	return model, nil
}

// loadLinks reads the link list, preferring JSON and falling back to the
// column output. The second return value reports whether networkd answered:
// the JSON path goes through networkd itself, while the text path is served
// from the kernel even when the daemon is down.
func (r *Real) loadLinks(ctx context.Context) ([]network.Link, bool, error) {
	if r.caps.Has(FeatureJSONStatus) {
		out, err := r.networkctl.Read(ctx, "networkctl", "--json=short", "list")
		if err == nil {
			if links, parseErr := ParseListJSON(out); parseErr == nil {
				return links, true, nil
			}
		}
	}
	out, err := r.networkctl.Read(ctx, "networkctl", "--no-legend", "list")
	if err != nil {
		return nil, false, err
	}
	return ParseListText(out), false, nil
}

// LoadLink re-reads one link in full. The list already carries most of it; the
// detail view asks again because `status` is where a link's own DNS, search
// domains and lease live.
func (r *Real) LoadLink(ctx context.Context, name string) (network.Link, error) {
	if err := checkLink(name); err != nil {
		return network.Link{}, err
	}
	if r.caps.Has(FeatureJSONStatus) {
		out, err := r.networkctl.Read(ctx, "networkctl", "status",
			"--json=short", name)
		if err == nil {
			return ParseStatusJSON(out)
		}
	}
	out, err := r.networkctl.Read(ctx, "networkctl", "status", "--no-pager",
		"--full", name)
	if err != nil {
		return network.Link{}, err
	}
	return ParseStatusText(out), nil
}

// loadRoutes reads the kernel routing table.
func (r *Real) loadRoutes(ctx context.Context) ([]network.Route, error) {
	if r.ip == nil {
		return nil, fmt.Errorf("networkd: the ip command is not available")
	}
	out, err := r.ip.Read(ctx, "ip", "-j", "route")
	if err != nil {
		return nil, err
	}
	return ParseRoutesJSON(out)
}

// loadDNS folds systemd-resolved's view into the model.
//
// resolvectl has a --json flag, but on every systemd released so far it only
// applies to the query verbs: `resolvectl status` and `resolvectl dns` print
// text whatever is asked of them. So the text is what is parsed, and the
// manifest's `resolvectl-json` feature is what will switch this over the day
// a systemd emits JSON here.
func (r *Real) loadDNS(ctx context.Context, model *network.Model) {
	if r.resolvectl == nil {
		return
	}
	if out, err := r.resolvectl.Read(ctx, "resolvectl", "dns"); err == nil {
		model.ResolvedRunning = true
		perLink, global := ParseResolvectlDNS(out)
		model.GlobalDNS = global
		for i := range model.Links {
			for _, server := range perLink[model.Links[i].Name] {
				model.Links[i].DNS = appendUnique(model.Links[i].DNS, server)
			}
		}
	}
	if out, err := r.resolvectl.Read(ctx, "resolvectl", "domain"); err == nil {
		perLink, global := ParseResolvectlDNS(out)
		model.GlobalSearchDomains = global
		for i := range model.Links {
			for _, domain := range perLink[model.Links[i].Name] {
				model.Links[i].SearchDomains =
					appendUnique(model.Links[i].SearchDomains, domain)
			}
		}
	}
}

// markUnmanaged decides which links tui-network refuses to change, and why.
//
// A link is read-only when networkd calls it unmanaged. When NetworkManager is
// running the reason says so by name, because "unmanaged" on such a machine
// means "NetworkManager owns this", and a user reading the screen deserves the
// actual explanation rather than the daemon's word for it.
func (r *Real) markUnmanaged(model *network.Model) {
	for i := range model.Links {
		link := &model.Links[i]
		if link.Managed && model.Running {
			continue
		}
		link.Managed = false
		switch {
		case model.ForeignManager != "":
			link.ReadOnlyReason = model.ForeignManager +
				" is running and this link is not managed by systemd-networkd, " +
				"so tui-network shows it read-only"
		case !model.Running:
			link.ReadOnlyReason = "systemd-networkd is not running, " +
				"so there is nothing here to change"
		default:
			link.ReadOnlyReason = unmanagedReason
		}
	}
}

// Journal returns the recent systemd-networkd log lines that mention a link.
func (r *Real) Journal(ctx context.Context, link string) ([]string, error) {
	if err := checkLink(link); err != nil {
		return nil, err
	}
	if r.journalctl == nil {
		return nil, fmt.Errorf("networkd: the journalctl command is not available")
	}
	out, err := r.journalctl.Read(ctx, "journalctl", "-u", "systemd-networkd",
		"--no-pager", "-n", journalLines, "--grep", link)
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// BuildLinkAction builds up, down, reconfigure or renew.
func (r *Real) BuildLinkAction(action, link string) (network.Command, error) {
	if action == network.ActionUp || action == network.ActionDown {
		if !r.caps.Has(FeatureLinkUpDown) {
			since, _ := r.caps.Since(FeatureLinkUpDown)
			return network.Command{}, fmt.Errorf(
				"networkctl gained `%s` in systemd %s; this machine runs %s",
				action, since, r.caps.Version())
		}
	}
	return BuildLinkAction(action, link)
}

// BuildReload re-reads the configuration files.
func (r *Real) BuildReload() (network.Command, error) { return BuildReload() }

// BuildFlushCaches empties the resolver cache.
func (r *Real) BuildFlushCaches() (network.Command, error) { return BuildFlushCaches() }

// BuildSetDNS sets a link's DNS servers at runtime.
func (r *Real) BuildSetDNS(link string, servers []string) (network.Command, error) {
	return BuildSetDNS(link, servers)
}

// BuildSetDomains sets a link's search domains at runtime.
func (r *Real) BuildSetDomains(link string, domains []string) (network.Command, error) {
	return BuildSetDomains(link, domains)
}

// BuildWriteConfig stages the new .network file in a temporary directory and
// returns the diff plus the two commands that apply it.
//
// Staging first is what makes the change reviewable: the file the user
// approves is a file that already exists, and `install` copies exactly that
// one. Nothing is written to /etc until the confirmed commands run.
func (r *Real) BuildWriteConfig(spec network.FileSpec) (network.WritePlan, error) {
	current, err := os.ReadFile(spec.Path)
	if err != nil && !os.IsNotExist(err) {
		return network.WritePlan{}, err
	}
	before := string(current)
	content, err := RenderFile(spec, before)
	if err != nil {
		return network.WritePlan{}, err
	}
	if before == content {
		return network.WritePlan{}, fmt.Errorf(
			"%s already says exactly this", spec.Path)
	}

	temp, err := stageFile(spec.Path, content)
	if err != nil {
		return network.WritePlan{}, err
	}
	installCmd, err := BuildInstallFile(temp, spec.Path)
	if err != nil {
		return network.WritePlan{}, err
	}
	reloadCmd, err := BuildReload()
	if err != nil {
		return network.WritePlan{}, err
	}
	return network.WritePlan{
		Path:     spec.Path,
		Content:  content,
		Diff:     Diff(spec.Path, before, content),
		TempPath: temp,
		Commands: []network.Command{installCmd, reloadCmd},
	}, nil
}

// stageFile writes the pending file to a private temporary directory and
// returns its path. The directory is the user's own, so staging needs no
// privileges; only the install step does.
func stageFile(destination, content string) (string, error) {
	dir, err := os.MkdirTemp("", "tui-network-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, filepath.Base(destination))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// LoadConfigFiles reads the .network files from systemd's search path.
//
// Reading them here rather than through `networkctl cat` is deliberate: the
// files are world-readable, a plain read needs no process at all, and it works
// on a machine where networkd is not running — which is exactly the machine
// where a user most wants to see what the configuration says.
func LoadConfigFiles(links []network.Link) []network.ConfigFile {
	byPath := map[string][]string{}
	for _, link := range links {
		if link.NetworkFile != "" {
			byPath[link.NetworkFile] = append(byPath[link.NetworkFile], link.Name)
		}
	}

	var files []network.ConfigFile
	seen := map[string]bool{}
	for _, dir := range ConfigDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".network") {
				continue
			}
			// A file in an earlier directory wins, the way networkd resolves
			// a name that appears in several of them.
			if seen[entry.Name()] {
				continue
			}
			seen[entry.Name()] = true
			path := filepath.Join(dir, entry.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			file := ParseNetworkFile(path, string(raw))
			file.Links = byPath[path]
			files = append(files, file)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}
