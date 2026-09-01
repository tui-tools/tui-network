package networkd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/tui-tools/tui-kit/runner"
	"github.com/tui-tools/tui-network/internal/network"
)

// demoConfigPath is the .network file the sample machine has.
const demoConfigPath = ConfigDir + "/10-wired.network"

// demoConfig is that file's text.
const demoConfig = `# The wired link on the sample machine.

[Match]
Name=enp1s0

[Network]
DHCP=ipv4
DNS=192.0.2.53
Domains=example.test
`

// Fake is an in-memory systemd-networkd. It backs --demo and the tests: every
// key works, every command is built and previewed exactly as the real backend
// builds it, and nothing reaches the system.
//
// The commands are recorded rather than run, and a hook applies to the
// in-memory model the change the real command would have made — so taking a
// link down in --demo really does show it down, and the argv the confirm
// dialog displayed is the argv a test can assert on.
type Fake struct {
	model network.Model
	run   *runner.Fake
	// staged is the pending file content, keyed by destination path. --demo
	// writes no file at all, so the "staging directory" is this map.
	staged map[string]string
}

// NewFake builds the sample machine: a loopback, a wired link configured by
// networkd over DHCP, and a wireless link another manager owns.
func NewFake() *Fake {
	f := &Fake{staged: map[string]string{}}
	f.run = &runner.Fake{Prefix: "sudo -n", Hook: f.apply}
	f.reset()
	return f
}

// reset builds the sample state. It is a function rather than a literal so
// --demo starts from the same machine every time, however it was left.
func (f *Fake) reset() {
	f.model = network.Model{
		Backend:         "systemd-networkd",
		Running:         true,
		ResolvedRunning: true,
		ForeignManager:  "NetworkManager",
		GlobalDNS:       []string{"192.0.2.53"},
		Links: []network.Link{
			{
				Index: 1, Name: "lo", Type: "loopback",
				Setup: network.SetupUnmanaged, Operational: "carrier",
				Carrier: "carrier", MTU: 65536,
				Addresses: []network.Address{
					{Address: "127.0.0.1", Prefix: 8, Family: "ipv4",
						Source: "foreign", Scope: "host"},
					{Address: "::1", Prefix: 128, Family: "ipv6",
						Source: "foreign", Scope: "host"},
				},
				ReadOnlyReason: "the loopback link is not managed by " +
					"systemd-networkd and never needs to be",
			},
			{
				Index: 2, Name: "enp1s0", Type: "ether", Driver: "e1000e",
				Setup: network.SetupConfigured, Operational: "routable",
				Carrier: "carrier", Online: "online",
				MAC: "02:00:00:00:00:02", MTU: 1500,
				Addresses: []network.Address{
					{Address: "192.0.2.24", Prefix: 24, Family: "ipv4",
						Source: "DHCPv4", Scope: "global", Provider: "192.0.2.1"},
					{Address: "fe80::ff:fe00:2", Prefix: 64, Family: "ipv6",
						Source: "foreign", Scope: "link"},
				},
				Gateways:      []string{"192.0.2.1"},
				DNS:           []string{"192.0.2.53"},
				SearchDomains: []string{"example.test"},
				NetworkFile:   demoConfigPath,
				DHCP: network.DHCP{
					Enabled: true, Server: "192.0.2.1", Address: "192.0.2.24",
					ClientID:       "IAID:0x945c2505/DUID",
					LeaseTimestamp: "2h13m4s since boot",
					Timeout1:       "8h13m4s since boot",
					Timeout2:       "14h43m4s since boot",
				},
				Managed: true,
			},
			{
				Index: 3, Name: "wlan0", Type: "wlan", Driver: "iwlwifi",
				Setup: network.SetupUnmanaged, Operational: "routable",
				Carrier: "carrier", MAC: "02:00:00:00:00:03", MTU: 1500,
				Addresses: []network.Address{
					{Address: "198.51.100.42", Prefix: 24, Family: "ipv4",
						Source: "foreign", Scope: "global"},
				},
				Gateways: []string{"198.51.100.1"},
				DNS:      []string{"198.51.100.1"},
				ReadOnlyReason: "NetworkManager is running and this link is " +
					"not managed by systemd-networkd, so tui-network shows it read-only",
			},
		},
		Routes: []network.Route{
			// Two uplinks: the wired link is the active default at a low
			// metric, the wireless one a standby at a higher metric — so the
			// Gateways screen, the switch and the manual failover all work on
			// the sample machine with nothing special installed.
			{Destination: "default", Gateway: "192.0.2.1", Link: "enp1s0",
				Protocol: "dhcp", Source: "192.0.2.24", Metric: 100, Family: "ipv4"},
			{Destination: "default", Gateway: "198.51.100.1", Link: "wlan0",
				Protocol: "static", Source: "198.51.100.42", Metric: 200, Family: "ipv4"},
			{Destination: "192.0.2.0/24", Link: "enp1s0", Protocol: "kernel",
				Scope: "link", Source: "192.0.2.24", Metric: 100, Family: "ipv4"},
			{Destination: "198.51.100.0/24", Link: "wlan0", Protocol: "kernel",
				Scope: "link", Source: "198.51.100.42", Family: "ipv4"},
			{Destination: "fe80::/64", Link: "enp1s0", Protocol: "kernel",
				Metric: 256, Family: "ipv6"},
		},
		ConfigFiles: []network.ConfigFile{
			withLinks(ParseNetworkFile(demoConfigPath, demoConfig), "enp1s0"),
		},
	}
}

// withLinks attaches the links a config file applies to.
func withLinks(file network.ConfigFile, links ...string) network.ConfigFile {
	file.Links = links
	return file
}

// Name identifies the backend. It is the real backend's name, because --demo
// shows what the real one would show.
func (f *Fake) Name() string { return "systemd-networkd" }

// Describe says plainly that nothing here is real.
func (f *Fake) Describe() string { return "demo (in-memory sample machine)" }

// Capabilities reports the same capabilities as the real backend.
func (f *Fake) Capabilities() network.Capabilities { return capabilities }

// Preview renders the command line the real backend would run.
func (f *Fake) Preview(cmd network.Command) string { return f.run.Preview(cmd) }

// Load returns the sample machine.
func (f *Fake) Load(_ context.Context) (network.Model, error) { return f.model, nil }

// LoadLink returns one link of the sample machine.
func (f *Fake) LoadLink(_ context.Context, name string) (network.Link, error) {
	link, ok := f.model.Link(name)
	if !ok {
		return network.Link{}, fmt.Errorf("no link named %q", name)
	}
	return link, nil
}

// Journal returns a plausible slice of what networkd says about a link.
func (f *Fake) Journal(_ context.Context, link string) ([]string, error) {
	if _, ok := f.model.Link(link); !ok {
		return nil, fmt.Errorf("no link named %q", link)
	}
	return []string{
		"Aug 29 09:12:01 demo systemd-networkd[412]: " + link + ": Link UP",
		"Aug 29 09:12:01 demo systemd-networkd[412]: " + link + ": Gained carrier",
		"Aug 29 09:12:02 demo systemd-networkd[412]: " + link +
			": DHCPv4 address 192.0.2.24/24 via 192.0.2.1",
		"Aug 29 09:12:02 demo systemd-networkd[412]: " + link + ": Gained IPv6LL",
		"Aug 29 09:12:03 demo systemd-networkd[412]: " + link +
			": Configuring with " + demoConfigPath,
	}, nil
}

// Run records the command and applies its effect to the sample machine.
func (f *Fake) Run(ctx context.Context, cmd network.Command) (string, error) {
	return f.run.Run(ctx, cmd)
}

// Ran exposes the recorded commands, which is what a test asserts on.
func (f *Fake) Ran() []network.Command { return f.run.Ran }

// apply is the hook the fake runner calls: it makes to the in-memory machine
// the change the real command would have made, so the demo stays coherent as
// keys are pressed.
func (f *Fake) apply(cmd network.Command) (string, error) {
	argv := cmd.Argv
	if len(argv) < 2 {
		return "ok", nil
	}
	switch argv[0] + " " + argv[1] {
	case "networkctl up":
		return f.setOperational(argv[2], "routable", network.SetupConfigured)
	case "networkctl down":
		return f.setOperational(argv[2], "off", network.SetupPending)
	case "networkctl reconfigure", "networkctl renew":
		return f.setOperational(argv[2], "routable", network.SetupConfigured)
	case "networkctl reload":
		return "", nil
	case "resolvectl flush-caches":
		return "", nil
	case "resolvectl dns":
		return f.setLinkList(argv[2], argv[3:], func(l *network.Link, v []string) {
			l.DNS = v
		})
	case "resolvectl domain":
		return f.setLinkList(argv[2], argv[3:], func(l *network.Link, v []string) {
			l.SearchDomains = v
		})
	case "install -m", "install -D":
		return f.installFile(argv)
	case "rm -f":
		return f.removeFile(argv)
	case "ip route", "ip -6":
		return f.replaceDefault(argv)
	}
	return "ok", nil
}

// replaceDefault applies an `ip route replace default via <gw> dev <if> metric
// <n>` to the sample machine, so the demo's active default really moves when
// the operator switches or fails over. It re-points the matching default route
// (or adds one), and lowers no other route, which is exactly what the kernel
// does with the command's metric.
func (f *Fake) replaceDefault(argv []string) (string, error) {
	fields := map[string]string{}
	for i := 0; i+1 < len(argv); i++ {
		switch argv[i] {
		case "via", "dev", "metric":
			fields[argv[i]] = argv[i+1]
		}
	}
	dev, gateway := fields["dev"], fields["via"]
	metric := 0
	if m, err := strconv.Atoi(fields["metric"]); err == nil {
		metric = m
	}
	if dev == "" {
		return "", nil
	}
	for i := range f.model.Routes {
		route := &f.model.Routes[i]
		if route.Destination == "default" && route.Link == dev {
			route.Metric = metric
			if gateway != "" {
				route.Gateway = gateway
			}
			return "", nil
		}
	}
	f.model.Routes = append(f.model.Routes, network.Route{
		Destination: "default", Gateway: gateway, Link: dev,
		Metric: metric, Family: "ipv4", Protocol: "static",
	})
	return "", nil
}

// Egress answers the reachability probe from the sample machine: a gateway is
// reachable over the interface its default route leaves by.
func (f *Fake) Egress(_ context.Context, gw network.Gateway) (network.Egress, error) {
	if link, ok := f.model.Link(gw.Interface); ok {
		return network.Egress{Dev: gw.Interface, Source: link.PrimaryAddress()}, nil
	}
	return network.Egress{}, nil
}

// BuildSetDefaultGateway builds the live default-route switch.
func (f *Fake) BuildSetDefaultGateway(gw network.Gateway, metric int) (network.Command, error) {
	return BuildSetDefaultGateway(gw, metric)
}

// BuildPersistGateway stages the drop-in in memory and returns the same plan
// the real backend returns — the same diff, and the same install-and-reload
// commands. --demo writes nothing, so the staging path is a name.
func (f *Fake) BuildPersistGateway(gw network.Gateway, metric int) (network.WritePlan, error) {
	if !gw.Managed {
		return network.WritePlan{}, fmt.Errorf(
			"networkd: %s is not managed by systemd-networkd, "+
				"so its gateway cannot be made persistent here", gw.Interface)
	}
	if gw.ConfigFile == "" {
		return network.WritePlan{}, fmt.Errorf(
			"networkd: %s has no .network file to attach a gateway drop-in to; "+
				"write one first with the link editor", gw.Interface)
	}
	dest := dropinPath(gw.ConfigFile)
	before := ""
	for _, file := range f.model.ConfigFiles {
		if file.Path == dest {
			before = file.Raw
		}
	}
	content, err := RenderGatewayDropin(gw, metric)
	if err != nil {
		return network.WritePlan{}, err
	}
	if before == content {
		return network.WritePlan{}, fmt.Errorf("%s already says exactly this", dest)
	}
	temp := "/tmp/tui-network/" + baseName(dest)
	f.staged[dest] = content
	installCmd, err := BuildInstallDropin(temp, dest)
	if err != nil {
		return network.WritePlan{}, err
	}
	reloadCmd, err := BuildReload()
	if err != nil {
		return network.WritePlan{}, err
	}
	return network.WritePlan{
		Path:     dest,
		Content:  content,
		Diff:     Diff(dest, before, content),
		TempPath: temp,
		Commands: []network.Command{installCmd, reloadCmd},
	}, nil
}

// setOperational moves a link to a new state, refusing an unmanaged one the
// way networkctl itself would.
func (f *Fake) setOperational(name, operational, setup string) (string, error) {
	for i := range f.model.Links {
		link := &f.model.Links[i]
		if link.Name != name {
			continue
		}
		if !link.Managed {
			// The wording is networkctl's own, capital included: the demo has
			// to fail the way the real command fails.
			//nolint:staticcheck // ST1005: this is the backend's message, quoted
			return "", fmt.Errorf("Link %s is not managed by systemd-networkd", name)
		}
		link.Operational, link.Setup = operational, setup
		return "", nil
	}
	//nolint:staticcheck // ST1005: this is networkctl's own message, quoted
	return "", fmt.Errorf("Cannot find device %s", name)
}

// setLinkList applies a resolvectl list assignment to a link.
func (f *Fake) setLinkList(name string, values []string,
	assign func(*network.Link, []string)) (string, error) {
	for i := range f.model.Links {
		if f.model.Links[i].Name == name {
			assign(&f.model.Links[i], values)
			return "", nil
		}
	}
	//nolint:staticcheck // ST1005: this is resolvectl's own message, quoted
	return "", fmt.Errorf("Cannot find device %s", name)
}

// installFile applies the staged file to the sample machine's configuration.
func (f *Fake) installFile(argv []string) (string, error) {
	if len(argv) < 5 {
		return "", fmt.Errorf("install: not enough arguments")
	}
	// The destination is the last argument whichever install form ran: `install
	// -m 644 temp dest` or the drop-in's `install -D -m 644 temp dest`.
	destination := argv[len(argv)-1]
	content, ok := f.staged[destination]
	if !ok {
		return "", fmt.Errorf("install: nothing staged for %s", destination)
	}
	if strings.HasSuffix(destination, NetdevSuffix) {
		return f.installNetdev(destination, content)
	}
	file := ParseNetworkFile(destination, content)
	for i := range f.model.ConfigFiles {
		if f.model.ConfigFiles[i].Path == destination {
			file.Links = f.model.ConfigFiles[i].Links
			f.model.ConfigFiles[i] = file
			return "", nil
		}
	}
	f.model.ConfigFiles = append(f.model.ConfigFiles, file)
	return "", nil
}

// installNetdev applies a staged .netdev unit to the sample machine: the unit
// is recorded, and the device it declares shows up as a link — which is what
// `networkctl reload` really does, and what makes removing it from the demo's
// links screen work the way it will on a real machine.
func (f *Fake) installNetdev(destination, content string) (string, error) {
	unit := ParseNetdevFile(destination, content)
	for i := range f.model.NetdevFiles {
		if f.model.NetdevFiles[i].Path == destination {
			f.model.NetdevFiles[i] = unit
			return "", nil
		}
	}
	f.model.NetdevFiles = append(f.model.NetdevFiles, unit)
	if _, exists := f.model.Link(unit.Name); !exists && unit.Name != "" {
		f.model.Links = append(f.model.Links, network.Link{
			Index: len(f.model.Links) + 1, Name: unit.Name, Type: "ether",
			Kind: unit.Kind, Setup: network.SetupConfigured,
			Operational: "carrier", Carrier: "carrier", MTU: 1500, Managed: true,
		})
	}
	return "", nil
}

// removeFile applies an `rm -f -- <path>` to the sample machine: the unit goes,
// and so does the device it declared.
func (f *Fake) removeFile(argv []string) (string, error) {
	path := argv[len(argv)-1]
	var units []network.NetdevFile
	removed := ""
	for _, unit := range f.model.NetdevFiles {
		if unit.Path == path {
			removed = unit.Name
			continue
		}
		units = append(units, unit)
	}
	f.model.NetdevFiles = units
	if removed == "" {
		return "", nil
	}
	var links []network.Link
	for _, link := range f.model.Links {
		if link.Name == removed {
			continue
		}
		links = append(links, link)
	}
	f.model.Links = links
	return "", nil
}

// fileIO gives the netdev planner the sample machine: it reads the in-memory
// files and "stages" into the same map the install hook reads back, so --demo
// builds exactly the plan a real run builds and writes nothing at all.
func (f *Fake) fileIO() FileIO {
	return FileIO{
		Read: func(path string) (string, error) {
			for _, file := range f.model.ConfigFiles {
				if file.Path == path {
					return file.Raw, nil
				}
			}
			for _, unit := range f.model.NetdevFiles {
				if unit.Path == path {
					return unit.Raw, nil
				}
			}
			return "", nil
		},
		Stage: func(path, content string) (string, error) {
			f.staged[path] = content
			return "/tmp/tui-network/" + baseName(path), nil
		},
	}
}

// BuildCreateNetdev builds the same multi-file plan the real backend builds.
func (f *Fake) BuildCreateNetdev(model network.Model,
	spec network.NetdevSpec) (network.WritePlan, error) {
	return BuildCreateNetdev(model, spec, f.fileIO())
}

// BuildRemoveNetdev builds the same removal plan the real backend builds.
func (f *Fake) BuildRemoveNetdev(model network.Model,
	name string) (network.WritePlan, error) {
	return BuildRemoveNetdev(model, name, f.fileIO())
}

// BuildLinkAction builds a link verb.
func (f *Fake) BuildLinkAction(action, link string) (network.Command, error) {
	return BuildLinkAction(action, link)
}

// BuildReload re-reads the configuration files.
func (f *Fake) BuildReload() (network.Command, error) { return BuildReload() }

// BuildFlushCaches empties the resolver cache.
func (f *Fake) BuildFlushCaches() (network.Command, error) { return BuildFlushCaches() }

// BuildSetDNS sets a link's DNS servers.
func (f *Fake) BuildSetDNS(link string, servers []string) (network.Command, error) {
	return BuildSetDNS(link, servers)
}

// BuildSetDomains sets a link's search domains.
func (f *Fake) BuildSetDomains(link string, domains []string) (network.Command, error) {
	return BuildSetDomains(link, domains)
}

// BuildWriteConfig stages the file in memory and returns the same plan the
// real backend returns — the same diff, and the same two commands. --demo
// writes nothing at all, so the staging path is a name rather than a file.
func (f *Fake) BuildWriteConfig(spec network.FileSpec) (network.WritePlan, error) {
	before := ""
	for _, file := range f.model.ConfigFiles {
		if file.Path == spec.Path {
			before = file.Raw
		}
	}
	content, err := RenderFile(spec, before)
	if err != nil {
		return network.WritePlan{}, err
	}
	if before == content {
		return network.WritePlan{}, fmt.Errorf("%s already says exactly this", spec.Path)
	}

	temp := "/tmp/tui-network/" + baseName(spec.Path)
	f.staged[spec.Path] = content
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

// baseName is filepath.Base for a path that is always absolute and always
// uses forward slashes.
func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}
