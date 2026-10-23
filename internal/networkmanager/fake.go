package networkmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tui-tools/tui-kit/runner"
	"github.com/tui-tools/tui-network/internal/network"
)

// The sample machine's profiles. Their UUIDs are fixed so a test can name
// them, and drawn from the documentation-friendly all-hex space.
const (
	demoWiredUUID  = "7d0c4a8e-1f3b-4c55-9a51-2f7e3c9b0a11"
	demoOfficeUUID = "3b9e2c71-8d44-4f0a-b6c2-5e1d7a9f4c22"
	demoWiredFile  = "/etc/NetworkManager/system-connections/Wired connection 1.nmconnection"
	demoOfficeFile = "/etc/NetworkManager/system-connections/Office.nmconnection"
)

// Fake is an in-memory NetworkManager. It backs --demo=networkmanager and the
// tests: every key works, every command is built and previewed exactly as the
// real backend builds it, and nothing reaches the system.
//
// The machine is held the way nmcli reports it, devices and profiles, and the
// model is built from them with the same BuildModel the real backend uses, so
// a parser-to-model bug shows in the demo too.
type Fake struct {
	run      *runner.Fake
	devices  []Device
	profiles []Profile
	routes   []network.Route
	// checkpoints are the rollback points CheckpointCreate took, by path.
	checkpoints map[string]fakeState
	// nextCheckpoint numbers them the way NetworkManager does.
	nextCheckpoint int
}

// fakeState is everything a checkpoint restores.
type fakeState struct {
	Devices  []Device
	Profiles []Profile
	Routes   []network.Route
}

// NewFake builds the sample machine: a wired device on DHCP, a wireless one
// on a static profile, a container bridge NetworkManager only observes, and
// the loopback.
func NewFake() *Fake {
	f := &Fake{checkpoints: map[string]fakeState{}}
	f.run = &runner.Fake{Prefix: "sudo -n", Hook: f.apply}
	f.reset()
	return f
}

// reset builds the sample state.
func (f *Fake) reset() {
	f.devices = []Device{
		{Name: "lo", Type: "loopback", HWAddr: "00:00:00:00:00:00", MTU: 65536,
			StateCode: stateActivated, State: "connected (externally)", Software: true,
			Connection: "lo", IP4Addresses: []string{"127.0.0.1/8"},
			IP6Addresses: []string{"::1/128"}},
		{Name: "enp1s0", Type: "ethernet", Driver: "e1000e", HWAddr: "02:00:00:00:00:02",
			MTU: 1500, StateCode: stateActivated, State: "connected",
			Connection: "Wired connection 1", ConnectionUUID: demoWiredUUID,
			IP4Addresses: []string{"192.0.2.24/24"},
			IP6Addresses: []string{"fe80::ff:fe00:2/64"},
			IP4Gateway:   "192.0.2.1", DNS: []string{"192.0.2.53"},
			Searches: []string{"example.test"},
			DHCP4: map[string]string{
				"dhcp_server_identifier": "192.0.2.1",
				"ip_address":             "192.0.2.24",
				"dhcp_lease_time":        "86400",
				"renewal_time":           "43200",
				"rebinding_time":         "75600",
			}},
		{Name: "wlp2s0", Type: "wifi", Driver: "iwlwifi", HWAddr: "02:00:00:00:00:03",
			MTU: 1500, StateCode: stateActivated, State: "connected",
			Connection: "Office", ConnectionUUID: demoOfficeUUID,
			IP4Addresses: []string{"198.51.100.42/24"}, IP4Gateway: "198.51.100.1",
			DNS: []string{"198.51.100.1"}},
		{Name: "docker0", Type: "bridge", Driver: "bridge", HWAddr: "02:42:00:00:00:04",
			MTU: 1500, StateCode: stateActivated, State: "connected (externally)",
			Software: true, Connection: "docker0",
			IP4Addresses: []string{"203.0.113.1/24"}},
	}
	f.profiles = []Profile{
		{Name: "Wired connection 1", UUID: demoWiredUUID, Type: "802-3-ethernet",
			Device: "enp1s0", Filename: demoWiredFile,
			Settings: []network.Setting{
				{Section: "connection", Key: "id", Value: "Wired connection 1"},
				{Section: "connection", Key: "uuid", Value: demoWiredUUID},
				{Section: "connection", Key: "type", Value: "802-3-ethernet"},
				{Section: "connection", Key: "interface-name", Value: "enp1s0"},
				{Section: "connection", Key: "autoconnect", Value: "yes"},
				{Section: "ipv4", Key: "method", Value: "auto"},
				{Section: "ipv4", Key: "dns", Value: "192.0.2.53"},
				{Section: "ipv4", Key: "dns-search", Value: "example.test"},
				{Section: "ipv4", Key: "route-metric", Value: "100"},
				{Section: "ipv6", Key: "method", Value: "auto"},
			}},
		{Name: "Office", UUID: demoOfficeUUID, Type: "802-11-wireless",
			Device: "wlp2s0", Filename: demoOfficeFile,
			Settings: []network.Setting{
				{Section: "connection", Key: "id", Value: "Office"},
				{Section: "connection", Key: "uuid", Value: demoOfficeUUID},
				{Section: "connection", Key: "type", Value: "802-11-wireless"},
				{Section: "connection", Key: "interface-name", Value: "wlp2s0"},
				{Section: "connection", Key: "autoconnect", Value: "yes"},
				{Section: "ipv4", Key: "method", Value: "manual"},
				{Section: "ipv4", Key: "addresses", Value: "198.51.100.42/24"},
				{Section: "ipv4", Key: "gateway", Value: "198.51.100.1"},
				{Section: "ipv4", Key: "dns", Value: "198.51.100.1"},
				{Section: "ipv4", Key: "route-metric", Value: "-1"},
				{Section: "ipv6", Key: "method", Value: "disabled"},
			}},
	}
	f.routes = []network.Route{
		// Two uplinks, so the Gateways screen, the switch and the failover
		// all work on the sample machine.
		{Destination: "default", Gateway: "192.0.2.1", Link: "enp1s0",
			Protocol: "dhcp", Source: "192.0.2.24", Metric: 100, Family: "ipv4"},
		{Destination: "default", Gateway: "198.51.100.1", Link: "wlp2s0",
			Protocol: "static", Metric: 600, Family: "ipv4"},
		{Destination: "192.0.2.0/24", Link: "enp1s0", Protocol: "kernel",
			Scope: "link", Source: "192.0.2.24", Metric: 100, Family: "ipv4"},
		{Destination: "198.51.100.0/24", Link: "wlp2s0", Protocol: "kernel",
			Scope: "link", Source: "198.51.100.42", Metric: 600, Family: "ipv4"},
		{Destination: "203.0.113.0/24", Link: "docker0", Protocol: "kernel",
			Scope: "link", Source: "203.0.113.1", Family: "ipv4"},
	}
}

// Name identifies the backend: the real one's name, because --demo shows
// what the real one would show.
func (f *Fake) Name() string { return Name }

// Describe says plainly that nothing here is real.
func (f *Fake) Describe() string { return "demo (in-memory NetworkManager machine)" }

// Capabilities reports the same capabilities as the real backend.
func (f *Fake) Capabilities() network.Capabilities { return capabilities }

// Preview renders the command line the real backend would run.
func (f *Fake) Preview(cmd network.Command) string { return f.run.Preview(cmd) }

// Run records the command and applies its effect to the sample machine.
func (f *Fake) Run(ctx context.Context, cmd network.Command) (string, error) {
	return f.run.Run(ctx, cmd)
}

// Ran exposes the recorded commands, which is what a test asserts on.
func (f *Fake) Ran() []network.Command { return f.run.Ran }

// Load builds the model from the sample machine.
func (f *Fake) Load(_ context.Context) (network.Model, error) {
	indexes := map[string]int{}
	for i, device := range f.devices {
		indexes[device.Name] = i + 1
	}
	model := BuildModel(f.devices, f.profiles, indexes)
	model.ResolvedRunning = true
	model.GlobalDNS = []string{"192.0.2.53"}
	model.Routes = append([]network.Route(nil), f.routes...)
	return model, nil
}

// LoadLink returns one link of the sample machine.
func (f *Fake) LoadLink(ctx context.Context, name string) (network.Link, error) {
	model, _ := f.Load(ctx)
	link, ok := model.Link(name)
	if !ok {
		return network.Link{}, fmt.Errorf("no device named %q", name)
	}
	return link, nil
}

// Journal returns a plausible slice of what NetworkManager says about a link.
func (f *Fake) Journal(_ context.Context, link string) ([]string, error) {
	if f.device(link) == nil {
		return nil, fmt.Errorf("no device named %q", link)
	}
	prefix := "Oct 20 09:12:01 demo NetworkManager[812]: <info>  [1792480321.0001] device (" +
		link + "): "
	return []string{
		prefix + "state change: disconnected -> prepare (reason 'none', managed-type: 'full')",
		prefix + "state change: prepare -> config (reason 'none', managed-type: 'full')",
		prefix + "state change: ip-config -> ip-check (reason 'none', managed-type: 'full')",
		prefix + "Activation: successful, device activated.",
	}, nil
}

// device finds a device of the sample machine.
func (f *Fake) device(name string) *Device {
	for i := range f.devices {
		if f.devices[i].Name == name {
			return &f.devices[i]
		}
	}
	return nil
}

// profile finds a profile of the sample machine by UUID.
func (f *Fake) profile(uuid string) *Profile {
	for i := range f.profiles {
		if f.profiles[i].UUID == uuid {
			return &f.profiles[i]
		}
	}
	return nil
}

// BuildLinkAction builds the link verbs against the sample machine.
func (f *Fake) BuildLinkAction(action, link string) (network.Command, error) {
	uuid := ""
	if device := f.device(link); device != nil {
		uuid = device.ConnectionUUID
	}
	return BuildLinkAction(action, link, uuid)
}

// BuildReload re-reads the profiles.
func (f *Fake) BuildReload() (network.Command, error) { return BuildReload() }

// BuildFlushCaches empties the resolver cache.
func (f *Fake) BuildFlushCaches() (network.Command, error) { return BuildFlushCaches() }

// BuildSetDNS sets a device's DNS servers at runtime.
func (f *Fake) BuildSetDNS(link string, servers []string) (network.Command, error) {
	return BuildSetDNS(link, servers)
}

// BuildSetDomains sets a device's search domains at runtime.
func (f *Fake) BuildSetDomains(link string, domains []string) (network.Command, error) {
	return BuildSetDomains(link, domains)
}

// SpecFor seeds the editor from the profile active on the link.
func (f *Fake) SpecFor(model network.Model, link network.Link) network.FileSpec {
	return SpecFor(model, link)
}

// BuildWriteConfig builds the profile change against the sample machine.
func (f *Fake) BuildWriteConfig(spec network.FileSpec) (network.WritePlan, error) {
	var current []network.Setting
	if spec.Connection != "" {
		profile := f.profile(spec.Connection)
		if profile == nil {
			return network.WritePlan{}, fmt.Errorf("no profile %s", spec.Connection)
		}
		current = profile.Settings
	} else if device := f.device(spec.MatchName); device == nil || device.Type != "ethernet" {
		return network.WritePlan{}, fmt.Errorf(
			"networkmanager: %s has no profile, and tui-network only creates "+
				"wired ones; create it with nmcli", spec.MatchName)
	}
	return PlanWriteConfig(spec, current)
}

// Egress answers the reachability probe: a gateway is reachable over the
// interface its default route leaves by.
func (f *Fake) Egress(_ context.Context, gw network.Gateway) (network.Egress, error) {
	if f.device(gw.Interface) != nil {
		return network.Egress{Dev: gw.Interface}, nil
	}
	return network.Egress{}, nil
}

// BuildSetDefaultGateway builds the runtime default-route switch.
func (f *Fake) BuildSetDefaultGateway(gw network.Gateway, metric int) (network.Command, error) {
	return BuildSetDefaultGateway(gw, metric)
}

// BuildPersistGateway keeps the uplink's priority in its profile.
func (f *Fake) BuildPersistGateway(gw network.Gateway, metric int) (network.WritePlan, error) {
	current := ""
	if profile := f.profile(gw.ConfigID); profile != nil {
		current = profile.Get(gw.Family, "route-metric")
	}
	return PlanPersistGateway(gw, metric, current)
}

// BuildCreateNetdev is not offered on NetworkManager yet.
func (f *Fake) BuildCreateNetdev(network.Model, network.NetdevSpec) (network.WritePlan, error) {
	return network.WritePlan{}, errNoNetdev
}

// BuildRemoveNetdev is not offered on NetworkManager yet.
func (f *Fake) BuildRemoveNetdev(network.Model, string) (network.WritePlan, error) {
	return network.WritePlan{}, errNoNetdev
}

// BuildArm takes a checkpoint of the sample machine.
func (f *Fake) BuildArm(timeout time.Duration) (network.Command, error) {
	return BuildArm(timeout)
}

// ArmedID reads the checkpoint from CheckpointCreate's answer.
func (f *Fake) ArmedID(output string) (string, error) { return ArmedID(output) }

// BuildKeep discards the checkpoint.
func (f *Fake) BuildKeep(id string) (network.Command, error) { return BuildKeep(id) }

// BuildRollback restores the checkpoint.
func (f *Fake) BuildRollback(id string) (network.Command, error) { return BuildRollback(id) }

// Expire does what NetworkManager does when a checkpoint's timeout runs out:
// it restores every checkpoint still armed. A test uses it to play the
// operator who never answered because the change cut them off.
func (f *Fake) Expire() {
	for id := range f.checkpoints {
		f.restore(id)
	}
}

// Checkpoints reports how many rollback points are armed.
func (f *Fake) Checkpoints() int { return len(f.checkpoints) }

// snapshot copies the sample machine for a checkpoint. A JSON round trip is
// the simplest deep copy of slices of structs holding slices and maps.
func (f *Fake) snapshot() fakeState {
	var state fakeState
	raw, _ := json.Marshal(fakeState{Devices: f.devices, Profiles: f.profiles, Routes: f.routes})
	_ = json.Unmarshal(raw, &state)
	return state
}

// restore puts a checkpoint back and forgets it.
func (f *Fake) restore(id string) bool {
	state, ok := f.checkpoints[id]
	if !ok {
		return false
	}
	f.devices, f.profiles, f.routes = state.Devices, state.Profiles, state.Routes
	delete(f.checkpoints, id)
	return true
}

// apply is the hook the fake runner calls: it makes to the in-memory machine
// the change the real command would have made.
func (f *Fake) apply(cmd network.Command) (string, error) {
	argv := cmd.Argv
	if len(argv) < 2 {
		return "", nil
	}
	switch argv[0] {
	case "busctl":
		return f.applyBusctl(argv)
	case "ip":
		return f.applyIPRoute(argv)
	case "nmcli":
		return f.applyNmcli(argv)
	}
	return "", nil
}

// applyBusctl plays NetworkManager's checkpoint API.
func (f *Fake) applyBusctl(argv []string) (string, error) {
	if len(argv) < 6 {
		return "", nil
	}
	switch argv[5] {
	case "CheckpointCreate":
		if len(f.checkpoints) > 0 {
			return "", fmt.Errorf("call failed: device 'lo' is already included in a checkpoint")
		}
		f.nextCheckpoint++
		id := "/org/freedesktop/NetworkManager/Checkpoint/" + strconv.Itoa(f.nextCheckpoint)
		f.checkpoints[id] = f.snapshot()
		return `o "` + id + `"`, nil
	case "CheckpointDestroy", "CheckpointRollback":
		id := argv[len(argv)-1]
		if _, ok := f.checkpoints[id]; !ok {
			return "", fmt.Errorf("call failed: checkpoint %s does not exist", id)
		}
		if argv[5] == "CheckpointDestroy" {
			delete(f.checkpoints, id)
			return "", nil
		}
		f.restore(id)
		return "a{su} 0", nil
	}
	return "", nil
}

// applyIPRoute applies an `ip route replace default via <gw> dev <if> metric
// <n>` to the sample machine.
func (f *Fake) applyIPRoute(argv []string) (string, error) {
	fields := map[string]string{}
	for i := 0; i+1 < len(argv); i++ {
		switch argv[i] {
		case "via", "dev", "metric":
			fields[argv[i]] = argv[i+1]
		}
	}
	metric, _ := strconv.Atoi(fields["metric"])
	f.setDefaultMetric(fields["dev"], metric)
	return "", nil
}

// setDefaultMetric re-points a device's default route to a new metric.
func (f *Fake) setDefaultMetric(dev string, metric int) {
	for i := range f.routes {
		if f.routes[i].Destination == "default" && f.routes[i].Link == dev {
			f.routes[i].Metric = metric
		}
	}
}

// applyNmcli applies the nmcli verbs the backend builds.
func (f *Fake) applyNmcli(argv []string) (string, error) {
	verb := argv[1]
	if len(argv) > 2 {
		verb += " " + argv[2]
	}
	switch verb {
	case "connection reload":
		return "", nil
	case "device connect":
		return f.setState(argv[3], stateActivated, "connected")
	case "device disconnect":
		return f.setState(argv[3], stateDisconnected, "disconnected")
	case "device reapply":
		if f.device(argv[3]) == nil {
			return "", fmt.Errorf("device '%s' not found", argv[3])
		}
		return "Connection successfully reapplied to device '" + argv[3] + "'.", nil
	case "device modify":
		return f.modifyDevice(argv[3], argv[4:])
	case "connection modify":
		return f.modifyProfile(argv[4], argv[5:])
	case "connection up":
		return f.activate(argv[4], argv[6])
	case "connection add":
		return f.addProfile(argv[3:])
	}
	return "", nil
}

// setState moves a device to a new state.
func (f *Fake) setState(name string, code int, state string) (string, error) {
	device := f.device(name)
	if device == nil {
		return "", fmt.Errorf("device '%s' not found", name)
	}
	device.StateCode, device.State = code, state
	if code != stateActivated {
		device.IP4Addresses, device.IP4Gateway = nil, ""
		device.Connection, device.ConnectionUUID = "", ""
		return "Device '" + name + "' successfully disconnected.", nil
	}
	for _, profile := range f.profiles {
		if profile.Device == name || profile.Get("connection", "interface-name") == name {
			f.applyProfile(device, profile)
			break
		}
	}
	return "Device '" + name + "' successfully activated.", nil
}

// modifyDevice applies `nmcli device modify <dev> key value…` to the device
// alone, the way NetworkManager reapplies a changed applied connection.
func (f *Fake) modifyDevice(name string, pairs []string) (string, error) {
	device := f.device(name)
	if device == nil {
		return "", fmt.Errorf("device '%s' not found", name)
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		value := pairs[i+1]
		switch pairs[i] {
		case "ipv4.dns":
			device.DNS = append(splitList(value), ipv6Only(device.DNS)...)
		case "ipv6.dns":
			device.DNS = append(ipv4Only(device.DNS), splitList(value)...)
		case "ipv4.dns-search":
			device.Searches = splitList(value)
		case "ipv4.ignore-auto-dns", "ipv6.ignore-auto-dns":
			// The sample's lease carries no DNS of its own, so there is
			// nothing to drop or bring back.
		case "ipv4.route-metric", "ipv6.route-metric":
			metric, _ := strconv.Atoi(value)
			f.setDefaultMetric(name, metric)
		}
	}
	return "Connection successfully reapplied to device '" + name + "'.", nil
}

// ipv4Only and ipv6Only keep one family of a server list.
func ipv4Only(list []string) []string { return familyOf(list, false) }
func ipv6Only(list []string) []string { return familyOf(list, true) }

// familyOf keeps the addresses of one family.
func familyOf(list []string, six bool) []string {
	var out []string
	for _, item := range list {
		if strings.Contains(item, ":") == six {
			out = append(out, item)
		}
	}
	return out
}

// modifyProfile writes `nmcli connection modify uuid <uuid> key value…` into a
// profile, without touching the device.
func (f *Fake) modifyProfile(uuid string, pairs []string) (string, error) {
	profile := f.profile(uuid)
	if profile == nil {
		return "", fmt.Errorf("unknown connection '%s'", uuid)
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		section, key, _ := strings.Cut(pairs[i], ".")
		setSetting(profile, section, key, pairs[i+1])
	}
	return "", nil
}

// setSetting replaces or adds one setting, dropping it when emptied, which is
// how nmcli prints an unset value back.
func setSetting(profile *Profile, section, key, value string) {
	for i, s := range profile.Settings {
		if s.Section == section && s.Key == key {
			if value == "" {
				profile.Settings = append(profile.Settings[:i], profile.Settings[i+1:]...)
				return
			}
			profile.Settings[i].Value = value
			return
		}
	}
	if value != "" {
		profile.Settings = append(profile.Settings,
			network.Setting{Section: section, Key: key, Value: value})
	}
}

// activate applies `nmcli connection up uuid <uuid> ifname <dev>`.
func (f *Fake) activate(uuid, name string) (string, error) {
	profile := f.profile(uuid)
	device := f.device(name)
	if profile == nil || device == nil {
		return "", fmt.Errorf("connection activation failed: unknown connection or device")
	}
	f.applyProfile(device, *profile)
	return "Connection successfully activated.", nil
}

// applyProfile makes a device carry what a profile says, the way an
// activation does: its addresses, gateway, DNS and route metric.
func (f *Fake) applyProfile(device *Device, profile Profile) {
	device.StateCode, device.State = stateActivated, "connected"
	device.Connection, device.ConnectionUUID = profile.Name, profile.UUID
	for i := range f.profiles {
		if f.profiles[i].UUID == profile.UUID {
			f.profiles[i].Device = device.Name
		}
	}
	switch profile.Get("ipv4", "method") {
	case MethodManual:
		device.IP4Addresses = splitList(profile.Get("ipv4", "addresses"))
		device.IP4Gateway = profile.Get("ipv4", "gateway")
		device.DHCP4 = nil
	case MethodDisabled:
		device.IP4Addresses, device.IP4Gateway, device.DHCP4 = nil, "", nil
	}
	if dns := profile.Get("ipv4", "dns"); dns != "" {
		device.DNS = append(splitList(dns), splitList(profile.Get("ipv6", "dns"))...)
	}
	device.Searches = splitList(profile.Get("ipv4", "dns-search"))
	if metric, err := strconv.Atoi(profile.Get("ipv4", "route-metric")); err == nil && metric >= 0 {
		f.setDefaultMetric(device.Name, metric)
	}
}

// addProfile applies `nmcli connection add type ethernet con-name <n> ifname
// <dev> key value…`.
func (f *Fake) addProfile(args []string) (string, error) {
	fields := map[string]string{}
	for i := 0; i+1 < len(args); i += 2 {
		fields[args[i]] = args[i+1]
	}
	uuid := fmt.Sprintf("00000000-0000-4000-8000-%012d", len(f.profiles)+1)
	profile := Profile{Name: fields["con-name"], UUID: uuid, Type: "802-3-ethernet",
		Filename: "/etc/NetworkManager/system-connections/" + fields["con-name"] + ".nmconnection",
		Settings: []network.Setting{
			{Section: "connection", Key: "id", Value: fields["con-name"]},
			{Section: "connection", Key: "uuid", Value: uuid},
			{Section: "connection", Key: "interface-name", Value: fields["ifname"]},
		}}
	for key, value := range fields {
		if section, name, ok := strings.Cut(key, "."); ok {
			setSetting(&profile, section, name, value)
		}
	}
	f.profiles = append(f.profiles, profile)
	return "Connection '" + profile.Name + "' (" + uuid + ") successfully added.", nil
}
