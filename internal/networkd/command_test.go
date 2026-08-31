package networkd

import (
	"strings"
	"testing"

	"github.com/tui-tools/tui-network/internal/network"
)

func TestBuildLinkAction(t *testing.T) {
	tests := []struct {
		action string
		want   string
		danger bool
	}{
		{network.ActionUp, "networkctl up enp1s0", false},
		{network.ActionDown, "networkctl down enp1s0", true},
		{network.ActionReconfigure, "networkctl reconfigure enp1s0", true},
		{network.ActionRenew, "networkctl renew enp1s0", false},
	}
	for _, test := range tests {
		cmd, err := BuildLinkAction(test.action, "enp1s0")
		if err != nil {
			t.Fatalf("%s: %v", test.action, err)
		}
		if got := cmd.String(); got != test.want {
			t.Errorf("%s: argv %q, want %q", test.action, got, test.want)
		}
		if cmd.Destructive != test.danger {
			t.Errorf("%s: destructive %v, want %v",
				test.action, cmd.Destructive, test.danger)
		}
		if cmd.Description == "" {
			t.Errorf("%s: a command with no description cannot be confirmed",
				test.action)
		}
	}
}

func TestBuildLinkActionRejects(t *testing.T) {
	// The link name reaches an argv, so anything that is not an interface
	// name is refused before a command exists at all.
	tests := []struct{ action, link string }{
		{network.ActionUp, "eth0; reboot"},
		{network.ActionUp, "../../etc"},
		{network.ActionUp, ""},
		{network.ActionUp, "an-interface-name-far-too-long"},
		{"restart", "eth0"},
	}
	for _, test := range tests {
		if _, err := BuildLinkAction(test.action, test.link); err == nil {
			t.Errorf("BuildLinkAction(%q, %q) built a command, want an error",
				test.action, test.link)
		}
	}
}

func TestBuildSimpleCommands(t *testing.T) {
	reload, err := BuildReload()
	if err != nil || reload.String() != "networkctl reload" {
		t.Errorf("reload = %q, %v", reload.String(), err)
	}
	flush, err := BuildFlushCaches()
	if err != nil || flush.String() != "resolvectl flush-caches" {
		t.Errorf("flush = %q, %v", flush.String(), err)
	}
}

func TestBuildSetDNS(t *testing.T) {
	cmd, err := BuildSetDNS("enp1s0", []string{"192.0.2.53", "2001:db8::53"})
	if err != nil {
		t.Fatalf("BuildSetDNS: %v", err)
	}
	want := "resolvectl dns enp1s0 192.0.2.53 2001:db8::53"
	if got := cmd.String(); got != want {
		t.Errorf("argv %q, want %q", got, want)
	}

	empty, err := BuildSetDNS("enp1s0", nil)
	if err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if got := empty.String(); got != "resolvectl dns enp1s0" {
		t.Errorf("clearing argv %q", got)
	}
	if !strings.Contains(empty.Description, "Clear") {
		t.Errorf("clearing description = %q", empty.Description)
	}

	for _, bad := range []string{"not a server", "8.8.8.8 ; reboot", "$(id)"} {
		if _, err := BuildSetDNS("enp1s0", []string{bad}); err == nil {
			t.Errorf("BuildSetDNS accepted %q", bad)
		}
	}
}

func TestBuildSetDomains(t *testing.T) {
	cmd, err := BuildSetDomains("enp1s0", []string{"example.test", "~corp.test", "."})
	if err != nil {
		t.Fatalf("BuildSetDomains: %v", err)
	}
	want := "resolvectl domain enp1s0 example.test ~corp.test ."
	if got := cmd.String(); got != want {
		t.Errorf("argv %q, want %q", got, want)
	}
	for _, bad := range []string{"a domain", "-lead", "x/y"} {
		if _, err := BuildSetDomains("enp1s0", []string{bad}); err == nil {
			t.Errorf("BuildSetDomains accepted %q", bad)
		}
	}
}

func TestBuildInstallFile(t *testing.T) {
	cmd, err := BuildInstallFile("/tmp/x/50-enp1s0.network",
		ConfigDir+"/50-enp1s0.network")
	if err != nil {
		t.Fatalf("BuildInstallFile: %v", err)
	}
	want := "install -m 644 /tmp/x/50-enp1s0.network " +
		ConfigDir + "/50-enp1s0.network"
	if got := cmd.String(); got != want {
		t.Errorf("argv %q, want %q", got, want)
	}
	if !cmd.Destructive {
		t.Errorf("overwriting a configuration file is a destructive change")
	}
}

func TestBuildInstallFileRefusesOtherPaths(t *testing.T) {
	// The destination is the one thing a user types that becomes a path in an
	// argv run as root, so everything outside the networkd directory, and
	// every name systemd would not read, is refused.
	for _, bad := range []string{
		"/etc/passwd",
		"/etc/systemd/network/../../passwd",
		ConfigDir + "/x.conf",
		ConfigDir + "/",
		"50-eth0.network",
	} {
		if _, err := BuildInstallFile("/tmp/x", bad); err == nil {
			t.Errorf("BuildInstallFile accepted %q", bad)
		}
	}
}

func TestRenderFile(t *testing.T) {
	spec := network.FileSpec{
		Path:      ConfigDir + "/50-enp1s0.network",
		MatchName: "enp1s0",
		DHCP:      "no",
		Address:   "192.0.2.10/24",
		Gateway:   "192.0.2.1",
		DNS:       []string{"192.0.2.53", "2001:db8::53"},
		Domains:   []string{"example.test"},
	}
	text, err := RenderFile(spec, "")
	if err != nil {
		t.Fatalf("RenderFile: %v", err)
	}
	for _, want := range []string{
		"[Match]\nName=enp1s0\n",
		"DHCP=no\n",
		"Address=192.0.2.10/24\n",
		"Gateway=192.0.2.1\n",
		"DNS=192.0.2.53\nDNS=2001:db8::53\n",
		"Domains=example.test\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered file is missing %q:\n%s", want, text)
		}
	}

	// What is written must parse back into the same settings: the file the
	// form writes is a file this tool can read.
	back := ParseNetworkFile(spec.Path, text)
	if back.MatchName != "enp1s0" {
		t.Errorf("round trip lost the match name")
	}
	if got := SpecFromFile(back, "enp1s0"); got.Address != spec.Address ||
		got.Gateway != spec.Gateway || len(got.DNS) != 2 ||
		len(got.Domains) != 1 || got.DHCP != "no" {
		t.Errorf("round trip = %+v, want %+v", got, spec)
	}
}

func TestRenderFileRejects(t *testing.T) {
	base := network.FileSpec{
		Path: ConfigDir + "/50-enp1s0.network", MatchName: "enp1s0", DHCP: "yes",
	}
	tests := []struct {
		name   string
		mutate func(*network.FileSpec)
	}{
		{"no match name", func(s *network.FileSpec) { s.MatchName = "" }},
		{"bad match name", func(s *network.FileSpec) { s.MatchName = "eth0 eth1" }},
		{"unknown dhcp mode", func(s *network.FileSpec) { s.DHCP = "maybe" }},
		{"static without address", func(s *network.FileSpec) { s.DHCP = "no" }},
		{"address without prefix", func(s *network.FileSpec) {
			s.Address = "192.0.2.10"
		}},
		{"bad gateway", func(s *network.FileSpec) { s.Gateway = "the router" }},
		{"bad dns", func(s *network.FileSpec) { s.DNS = []string{"nope!"} }},
		{"bad domain", func(s *network.FileSpec) { s.Domains = []string{"a b"} }},
		{"path outside the config dir", func(s *network.FileSpec) {
			s.Path = "/etc/passwd"
		}},
	}
	for _, test := range tests {
		spec := base
		test.mutate(&spec)
		if _, err := RenderFile(spec, ""); err == nil {
			t.Errorf("%s: RenderFile accepted %+v", test.name, spec)
		}
	}
}

func TestSpecFromFileRedirectsDistributionFiles(t *testing.T) {
	// A file shipped under /usr/lib is never edited in place: the form writes
	// an administrator copy that overrides it.
	file := ParseNetworkFile("/usr/lib/systemd/network/99-default.network",
		"[Match]\nName=en*\n\n[Network]\nDHCP=yes\n")
	spec := SpecFromFile(file, "enp1s0")
	if spec.Path != ConfigDir+"/50-enp1s0.network" {
		t.Errorf("path = %q, want a file under %s", spec.Path, ConfigDir)
	}
	if spec.DHCP != "yes" {
		t.Errorf("dhcp = %q", spec.DHCP)
	}
}

func TestNormalizeDHCP(t *testing.T) {
	tests := map[string]string{
		"yes": "yes", "true": "yes", "both": "yes",
		"no": "no", "false": "no", "": "no",
		"ipv4": "ipv4", "ipv6": "ipv6",
	}
	for in, want := range tests {
		if got := normalizeDHCP(in); got != want {
			t.Errorf("normalizeDHCP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiff(t *testing.T) {
	before := "[Match]\nName=enp1s0\n\n[Network]\nDHCP=ipv4\n"
	after := "[Match]\nName=enp1s0\n\n[Network]\nDHCP=no\nAddress=192.0.2.10/24\n"
	diff := Diff("/etc/systemd/network/10-wired.network", before, after)

	for _, want := range []string{
		"--- /etc/systemd/network/10-wired.network",
		"+++ /etc/systemd/network/10-wired.network",
		"-DHCP=ipv4",
		"+DHCP=no",
		"+Address=192.0.2.10/24",
	} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff is missing %q:\n%s", want, diff)
		}
	}
	// The unchanged head is not repeated in the hunk.
	if strings.Contains(diff, "-[Match]") {
		t.Errorf("the diff repeated an unchanged line:\n%s", diff)
	}
}

func TestDiffOnANewFile(t *testing.T) {
	diff := Diff("/etc/systemd/network/50-x.network", "", "[Match]\nName=x\n")
	if !strings.Contains(diff, "--- /dev/null") {
		t.Errorf("a new file must diff against /dev/null:\n%s", diff)
	}
	if Diff("/etc/systemd/network/50-x.network", "same\n", "same\n") != "" {
		t.Errorf("an identical file must produce no diff")
	}
}

func TestFileName(t *testing.T) {
	if got := FileName("enp1s0"); got != ConfigDir+"/50-enp1s0.network" {
		t.Errorf("FileName = %q", got)
	}
}

func TestBuildSetDefaultGateway(t *testing.T) {
	gw := network.Gateway{Interface: "wan0", Address: "192.0.2.1", Family: "ipv4"}
	cmd, err := BuildSetDefaultGateway(gw, 99)
	if err != nil {
		t.Fatalf("BuildSetDefaultGateway: %v", err)
	}
	want := "ip route replace default via 192.0.2.1 dev wan0 metric 99"
	if got := cmd.String(); got != want {
		t.Errorf("argv %q, want %q", got, want)
	}
	if !cmd.Destructive {
		t.Errorf("re-pointing the default route is a destructive change")
	}
	if cmd.Description == "" {
		t.Errorf("a command with no description cannot be confirmed")
	}

	// An IPv6 gateway needs the -6 selector so the command touches the v6 table.
	v6 := network.Gateway{Interface: "wan0", Address: "2001:db8::1", Family: "ipv6"}
	cmd6, err := BuildSetDefaultGateway(v6, 50)
	if err != nil {
		t.Fatalf("BuildSetDefaultGateway v6: %v", err)
	}
	if got := cmd6.String(); got != "ip -6 route replace default via 2001:db8::1 dev wan0 metric 50" {
		t.Errorf("v6 argv %q", got)
	}
}

func TestBuildSetDefaultGatewayRejects(t *testing.T) {
	// The interface, the gateway and the metric all reach an argv run as root,
	// so anything that is not what it claims to be is refused before a command
	// exists.
	tests := []struct {
		gw     network.Gateway
		metric int
	}{
		{network.Gateway{Interface: "wan0; reboot", Address: "192.0.2.1"}, 1},
		{network.Gateway{Interface: "wan0", Address: "not-an-ip"}, 1},
		{network.Gateway{Interface: "wan0", Address: "192.0.2.1 ; id"}, 1},
		{network.Gateway{Interface: "wan0", Address: "192.0.2.1"}, -1},
	}
	for _, test := range tests {
		if _, err := BuildSetDefaultGateway(test.gw, test.metric); err == nil {
			t.Errorf("BuildSetDefaultGateway accepted %+v metric %d", test.gw, test.metric)
		}
	}
}

func TestRenderGatewayDropin(t *testing.T) {
	gw := network.Gateway{Interface: "wan0", Address: "192.0.2.1", Family: "ipv4"}
	text, err := RenderGatewayDropin(gw, 100)
	if err != nil {
		t.Fatalf("RenderGatewayDropin: %v", err)
	}
	for _, want := range []string{"[Route]\n", "Gateway=192.0.2.1\n", "Metric=100\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("drop-in is missing %q:\n%s", want, text)
		}
	}
	// What is written must parse back into a setting the reader recognises.
	file := ParseNetworkFile("/etc/systemd/network/10-wan0.network.d/50-tui-gateway.conf", text)
	if got, _ := file.Get("Route", "Gateway"); got != "192.0.2.1" {
		t.Errorf("round trip lost the gateway, got %q", got)
	}
}

func TestDropinPathAndCheck(t *testing.T) {
	path := dropinPath("/usr/lib/systemd/network/10-wan0.network")
	want := ConfigDir + "/10-wan0.network.d/50-tui-gateway.conf"
	if path != want {
		t.Errorf("dropinPath = %q, want %q", path, want)
	}
	if err := checkDropinPath(path); err != nil {
		t.Errorf("checkDropinPath rejected its own path: %v", err)
	}
	for _, bad := range []string{
		"/etc/passwd",
		ConfigDir + "/10-wan0.network.d/other.conf",
		ConfigDir + "/50-tui-gateway.conf",
		"/tmp/x/10-wan0.network.d/50-tui-gateway.conf",
	} {
		if err := checkDropinPath(bad); err == nil {
			t.Errorf("checkDropinPath accepted %q", bad)
		}
	}
}

func TestBuildInstallDropinCreatesTheDirectory(t *testing.T) {
	dest := ConfigDir + "/10-wan0.network.d/50-tui-gateway.conf"
	cmd, err := BuildInstallDropin("/tmp/x/50-tui-gateway.conf", dest)
	if err != nil {
		t.Fatalf("BuildInstallDropin: %v", err)
	}
	// -D makes install create the .network.d directory, so there is no separate
	// mkdir to preview and no window with the wrong mode.
	if !strings.Contains(cmd.String(), "install -D -m 644") {
		t.Errorf("install command = %q, want -D", cmd.String())
	}
	if !cmd.Destructive {
		t.Errorf("writing a drop-in is a destructive change")
	}
}
