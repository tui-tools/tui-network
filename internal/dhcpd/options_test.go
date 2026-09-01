package dhcpd

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tui-tools/tui-network/internal/dhcp"
)

// TestRenderOptionsRoundTrip: the file the renderer writes parses back into
// the very options it was rendered from, which is what makes the form's
// seed-edit-render loop stable.
func TestRenderOptionsRoundTrip(t *testing.T) {
	want := dhcp.Options{
		DNS:       []string{"192.0.2.1", "2001:db8::53"},
		Gateway:   "192.0.2.1",
		Domain:    "lan.example.test",
		Upstreams: []string{"198.51.100.53", "203.0.113.53"},
	}
	text, err := RenderOptionsFile(want)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := ParseDnsmasqOptions(DnsmasqOptionsFile, text)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the options:\n got %+v\nwant %+v\nfile:\n%s",
			got, want, text)
	}
	if !strings.HasPrefix(text, "# Written by tui-network") {
		t.Errorf("the rendered file does not name its owner:\n%s", text)
	}
}

// TestRenderOptionsOmitsEmptyFields: an empty field renders no line at all —
// with option 6 or 3 absent, dnsmasq advertises its own address, which is the
// router default this tool documents rather than pinning.
func TestRenderOptionsOmitsEmptyFields(t *testing.T) {
	text, err := RenderOptionsFile(dhcp.Options{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, banned := range []string{"dhcp-option", "domain=", "server="} {
		if strings.Contains(text, banned) {
			t.Errorf("an empty options set rendered %q:\n%s", banned, text)
		}
	}
}

// TestParseDnsmasqOptionsSeedsFromExisting covers a hand-written drop-in: the
// named option forms, a range-carrying domain, tag-scoped options that must be
// left alone, and server= forms that are not plain upstreams.
func TestParseDnsmasqOptionsSeedsFromExisting(t *testing.T) {
	raw := strings.Join([]string{
		"# a comment",
		"dhcp-option=option:dns-server,192.0.2.1,192.0.2.2",
		"dhcp-option=option:router,192.0.2.254",
		"domain=lan.example.test,192.0.2.0/24",
		"server=198.51.100.53",
		"server=/corp.example.test/203.0.113.5", // one zone, not an upstream
		"server=198.51.100.53",                  // duplicate line, kept once by merge
		"dhcp-option=tag:guests,6,203.0.113.99", // scoped to a class: not ours
		"--dhcp-option=3,198.51.100.254",        // command-line form, later wins
	}, "\n")

	got := ParseDnsmasqOptions(DnsmasqOptionsFile, raw)
	merged := MergeOptions(dhcp.Options{}, got)
	want := dhcp.Options{
		DNS:       []string{"192.0.2.1", "192.0.2.2"},
		Gateway:   "198.51.100.254",
		Domain:    "lan.example.test",
		Upstreams: []string{"198.51.100.53"},
	}
	if !reflect.DeepEqual(merged, want) {
		t.Errorf("parsed %+v, want %+v", merged, want)
	}
}

// TestMergeOptionsAcrossFiles mirrors dnsmasq's read order: a later file's
// scalar wins, a later DNS list replaces, and upstream forwarders accumulate.
func TestMergeOptionsAcrossFiles(t *testing.T) {
	main := dhcp.Options{DNS: []string{"192.0.2.9"}, Domain: "lan.example.test",
		Upstreams: []string{"198.51.100.53"}}
	dropIn := dhcp.Options{DNS: []string{"192.0.2.1"}, Gateway: "192.0.2.254",
		Upstreams: []string{"203.0.113.53", "198.51.100.53"}}

	got := MergeOptions(main, dropIn)
	want := dhcp.Options{
		DNS:       []string{"192.0.2.1"},
		Gateway:   "192.0.2.254",
		Domain:    "lan.example.test",
		Upstreams: []string{"198.51.100.53", "203.0.113.53"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged %+v, want %+v", got, want)
	}
}

// TestRenderOptionsRefusesInjection: every user-typed value goes through an
// address parse or the domain gate, so nothing can smuggle a newline, another
// directive or a shell metacharacter into the rendered file.
func TestRenderOptionsRefusesInjection(t *testing.T) {
	bad := []dhcp.Options{
		{DNS: []string{"192.0.2.1\ndhcp-range=203.0.113.1,203.0.113.99"}},
		{DNS: []string{"not-an-ip"}},
		{Gateway: "192.0.2.1,set:evil"},
		{Gateway: "192.0.2.1 192.0.2.2"},
		{Domain: "lan;reboot"},
		{Domain: "lan example"},
		{Domain: "lan,203.0.113.0/24"},
		{Domain: "-lan.example"},
		{Upstreams: []string{"198.51.100.53#5353"}},
		{Upstreams: []string{"/example.test/198.51.100.53"}},
	}
	for _, opts := range bad {
		if text, err := RenderOptionsFile(opts); err == nil {
			t.Errorf("options %+v were rendered rather than refused:\n%s", opts, text)
		}
	}
}

// TestBuildSetOptionsPlan checks the plan the confirm dialog holds: the
// tool-owned path, the full regenerated content, and install + restart —
// restart, because dnsmasq does not re-read configuration files on SIGHUP.
func TestBuildSetOptionsPlan(t *testing.T) {
	f := NewFake()
	plan, err := f.BuildSetOptions(dhcp.Options{
		DNS:       []string{"192.0.2.1"},
		Domain:    "lan.example.test",
		Upstreams: []string{"9.9.9.9"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if plan.Path != DnsmasqOptionsFile {
		t.Errorf("plan writes %s, want %s", plan.Path, DnsmasqOptionsFile)
	}
	for _, want := range []string{"dhcp-option=6,192.0.2.1", "domain=lan.example.test",
		"server=9.9.9.9"} {
		if !strings.Contains(plan.Content, want) {
			t.Errorf("plan content misses %q:\n%s", want, plan.Content)
		}
	}
	if !strings.Contains(plan.Diff, "+server=9.9.9.9") {
		t.Errorf("the diff does not show the added forwarder:\n%s", plan.Diff)
	}
	if len(plan.Commands) != 2 ||
		plan.Commands[0].String() != "install -m 644 "+plan.TempPath+" "+DnsmasqOptionsFile ||
		plan.Commands[1].String() != "systemctl restart dnsmasq" {
		t.Errorf("plan commands are not install + restart: %v", plan.Commands)
	}
}

// TestFakeParityOnOptions: the demo backend reports the same capability, seeds
// the same OwnOptions from its in-memory drop-in, and applying the plan really
// changes what the model advertises — so --demo behaves like a real router.
func TestFakeParityOnOptions(t *testing.T) {
	f := NewFake()
	if !f.Capabilities().SupportsSetOptions ||
		f.Capabilities().OptionsFile != DnsmasqOptionsFile {
		t.Fatalf("the fake does not offer the options editor: %+v", f.Capabilities())
	}
	model, _ := f.Load(t.Context())
	if len(model.OwnOptions.DNS) == 0 || len(model.OwnOptions.Upstreams) == 0 {
		t.Fatalf("the demo drop-in seeds nothing: %+v", model.OwnOptions)
	}
	// The main file's domain reaches the merged view but not the seed.
	if model.Options.Domain != "lan.example.test" {
		t.Errorf("merged options miss the main file's domain: %+v", model.Options)
	}
	if model.OwnOptions.Domain != "" {
		t.Errorf("the seed leaked a value from a file the tool does not own: %+v",
			model.OwnOptions)
	}

	plan, err := f.BuildSetOptions(dhcp.Options{DNS: []string{"192.0.2.8"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, cmd := range plan.Commands {
		if _, err := f.Run(t.Context(), cmd); err != nil {
			t.Fatalf("run %v: %v", cmd, err)
		}
	}
	model, _ = f.Load(t.Context())
	if !reflect.DeepEqual(model.Options.DNS, []string{"192.0.2.8"}) {
		t.Errorf("after applying, the model advertises %v", model.Options.DNS)
	}
}

// TestKeaRefusesOptions: the read-only backend refuses the mutation in words.
func TestKeaRefusesOptions(t *testing.T) {
	r := &Real{kind: dhcp.KindKea}
	if _, err := r.BuildSetOptions(dhcp.Options{Domain: "lan.example.test"}); err == nil {
		t.Error("Kea should be read-only for options in this phase")
	}
}
