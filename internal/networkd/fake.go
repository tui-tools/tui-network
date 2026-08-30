package networkd

import (
	"context"
	"fmt"
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
			{Destination: "default", Gateway: "192.0.2.1", Link: "enp1s0",
				Protocol: "dhcp", Source: "192.0.2.24", Metric: 1024, Family: "ipv4"},
			{Destination: "default", Gateway: "198.51.100.1", Link: "wlan0",
				Protocol: "static", Source: "198.51.100.42", Metric: 600, Family: "ipv4"},
			{Destination: "192.0.2.0/24", Link: "enp1s0", Protocol: "kernel",
				Scope: "link", Source: "192.0.2.24", Metric: 1024, Family: "ipv4"},
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
	case "install -m":
		return f.installFile(argv)
	}
	return "ok", nil
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
	destination := argv[4]
	content, ok := f.staged[destination]
	if !ok {
		return "", fmt.Errorf("install: nothing staged for %s", destination)
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
