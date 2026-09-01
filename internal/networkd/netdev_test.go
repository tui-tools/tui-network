package networkd

import (
	"strings"
	"testing"

	"github.com/tui-tools/tui-network/internal/network"
)

// sampleModel is a small machine to plan against: two links networkd manages,
// one it does not, and a .network file for the first.
func sampleModel() network.Model {
	wired := ConfigDir + "/10-wired.network"
	return network.Model{
		Running: true,
		Links: []network.Link{
			{Index: 2, Name: "enp1s0", Type: "ether", Managed: true,
				NetworkFile: wired},
			{Index: 3, Name: "enp2s0", Type: "ether", Managed: true},
			{Index: 4, Name: "wlan0", Type: "wlan",
				ReadOnlyReason: "NetworkManager owns it"},
		},
		ConfigFiles: []network.ConfigFile{
			withLinks(ParseNetworkFile(wired,
				"[Match]\nName=enp1s0\n\n[Network]\nDHCP=ipv4\n"), "enp1s0"),
		},
	}
}

// memoryIO is a FileIO over a map, which is what a test plans against.
func memoryIO(files map[string]string) (FileIO, map[string]string) {
	staged := map[string]string{}
	return FileIO{
		Read: func(path string) (string, error) { return files[path], nil },
		Stage: func(path, content string) (string, error) {
			staged[path] = content
			return "/tmp/staged/" + baseName(path), nil
		},
	}, staged
}

// modelIO is the FileIO a plan over sampleModel wants: the .network files it
// carries, read back by path.
func modelIO(model network.Model) (FileIO, map[string]string) {
	files := map[string]string{}
	for _, file := range model.ConfigFiles {
		files[file.Path] = file.Raw
	}
	for _, unit := range model.NetdevFiles {
		files[unit.Path] = unit.Raw
	}
	return memoryIO(files)
}

// TestVLANIDBounds is the range guard: 0 and 4095 are reserved by 802.1Q, so
// the only ids a device can carry are 1 through 4094 — and the check has to
// hold in the renderer as well as in the form's parser, because the renderer is
// the one the plan actually goes through.
func TestVLANIDBounds(t *testing.T) {
	tests := []struct {
		id    int
		valid bool
	}{
		{-1, false},
		{0, false},
		{1, true},
		{10, true},
		{4094, true},
		{4095, false},
		{9000, false},
	}
	model := sampleModel()
	for _, test := range tests {
		spec := network.NetdevSpec{
			Kind: network.NetdevVLAN, Name: "vlan10",
			Parent: "enp1s0", VLANID: test.id,
		}
		_, err := RenderNetdev(spec)
		if (err == nil) != test.valid {
			t.Errorf("RenderNetdev with id %d: err = %v, want valid=%v",
				test.id, err, test.valid)
		}
		if err := checkNetdevSpec(spec, model); (err == nil) != test.valid {
			t.Errorf("checkNetdevSpec with id %d: err = %v, want valid=%v",
				test.id, err, test.valid)
		}
	}
}

func TestParseVLANID(t *testing.T) {
	tests := []struct {
		text  string
		want  int
		valid bool
	}{
		{"1", 1, true},
		{"4094", 4094, true},
		{" 10 ", 10, true},
		{"0", 0, false},
		{"4095", 0, false},
		{"", 0, false},
		{"ten", 0, false},
		{"10; reboot", 0, false},
	}
	for _, test := range tests {
		got, err := ParseVLANID(test.text)
		if (err == nil) != test.valid {
			t.Errorf("ParseVLANID(%q): err = %v, want valid=%v", test.text, err, test.valid)
			continue
		}
		if test.valid && got != test.want {
			t.Errorf("ParseVLANID(%q) = %d, want %d", test.text, got, test.want)
		}
	}
}

func TestNetdevNameValidation(t *testing.T) {
	for _, bad := range []string{
		"", "br 0", "br/0", "../etc", "br0; reboot", "-br0",
		"a-bridge-name-far-too-long",
	} {
		if err := checkNetdevName(bad); err == nil {
			t.Errorf("checkNetdevName accepted %q", bad)
		}
	}
	for _, good := range []string{"br0", "vlan10", "lan.10", "v_1"} {
		if err := checkNetdevName(good); err != nil {
			t.Errorf("checkNetdevName rejected %q: %v", good, err)
		}
	}
}

// TestCreateRefusesACollision covers every way a name can already be taken:
// another .netdev unit declares it, a link already carries it, or the unit file
// this plan would write is already there.
func TestCreateRefusesACollision(t *testing.T) {
	base := sampleModel()

	taken := base
	taken.NetdevFiles = []network.NetdevFile{
		ParseNetdevFile(NetdevPath("br0"),
			fileHeader+"[NetDev]\nName=br0\nKind=bridge\n"),
	}

	tests := []struct {
		name  string
		model network.Model
		spec  network.NetdevSpec
	}{
		{"a unit already declares the name", taken, network.NetdevSpec{
			Kind: network.NetdevBridge, Name: "br0", Members: []string{"enp1s0"}}},
		{"a link already carries the name", base, network.NetdevSpec{
			Kind: network.NetdevBridge, Name: "enp2s0", Members: []string{"enp1s0"}}},
		{"the member is not a link", base, network.NetdevSpec{
			Kind: network.NetdevBridge, Name: "br0", Members: []string{"enp9s0"}}},
		{"the member is unmanaged", base, network.NetdevSpec{
			Kind: network.NetdevBridge, Name: "br0", Members: []string{"wlan0"}}},
		{"a bridge with no members", base, network.NetdevSpec{
			Kind: network.NetdevBridge, Name: "br0"}},
		{"a member listed twice", base, network.NetdevSpec{
			Kind: network.NetdevBridge, Name: "br0",
			Members: []string{"enp1s0", "enp1s0"}}},
		{"a bridge that is its own member", base, network.NetdevSpec{
			Kind: network.NetdevBridge, Name: "enp1s0", Members: []string{"enp1s0"}}},
		{"a VLAN with no parent", base, network.NetdevSpec{
			Kind: network.NetdevVLAN, Name: "vlan10", VLANID: 10}},
		{"an unknown kind", base, network.NetdevSpec{
			Kind: "bond", Name: "bond0", Members: []string{"enp1s0"}}},
		{"a name that is not a name", base, network.NetdevSpec{
			Kind: network.NetdevBridge, Name: "br 0", Members: []string{"enp1s0"}}},
	}
	for _, test := range tests {
		files, _ := modelIO(test.model)
		if _, err := BuildCreateNetdev(test.model, test.spec, files); err == nil {
			t.Errorf("%s: BuildCreateNetdev built a plan, want a refusal", test.name)
		}
	}
}

// TestCreateVLANPlan is the whole promise of the feature: one plan, two files,
// one diff, and commands that install each file before the single reload.
func TestCreateVLANPlan(t *testing.T) {
	model := sampleModel()
	files, staged := modelIO(model)
	spec := network.NetdevSpec{
		Kind: network.NetdevVLAN, Name: "vlan10", Parent: "enp1s0", VLANID: 10,
	}
	plan, err := BuildCreateNetdev(model, spec, files)
	if err != nil {
		t.Fatalf("BuildCreateNetdev: %v", err)
	}

	if len(plan.Files) != 2 {
		t.Fatalf("plan touches %d files, want the unit and the parent", len(plan.Files))
	}
	unit := staged[NetdevPath("vlan10")]
	for _, want := range []string{
		"[NetDev]\n", "Name=vlan10\n", "Kind=vlan\n", "[VLAN]\n", "Id=10\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the .netdev unit is missing %q:\n%s", want, unit)
		}
	}
	parent := staged[ConfigDir+"/10-wired.network"]
	if !strings.Contains(parent, "VLAN=vlan10") {
		t.Errorf("the parent's .network file is missing the VLAN line:\n%s", parent)
	}
	// The parent keeps everything it had: a member line is an addition, not a
	// regeneration.
	if !strings.Contains(parent, "DHCP=ipv4") {
		t.Errorf("the parent's own settings were lost:\n%s", parent)
	}

	// One diff, covering both files.
	for _, want := range []string{
		"+++ " + NetdevPath("vlan10"),
		"+Kind=vlan",
		"+++ " + ConfigDir + "/10-wired.network",
		"+VLAN=vlan10",
	} {
		if !strings.Contains(plan.Diff, want) {
			t.Errorf("the multi-file diff is missing %q:\n%s", want, plan.Diff)
		}
	}

	// Install each file, then reload once — never a reload in the middle,
	// which is where networkd would see half a device.
	if len(plan.Commands) != 3 {
		t.Fatalf("plan has %d commands, want two installs and a reload:\n%v",
			len(plan.Commands), plan.Commands)
	}
	for i, cmd := range plan.Commands[:2] {
		if !strings.HasPrefix(cmd.String(), "install -m 644 ") {
			t.Errorf("command %d = %q, want an install", i, cmd.String())
		}
		if !cmd.Destructive {
			t.Errorf("command %d is not marked destructive", i)
		}
	}
	if got := plan.Commands[2].String(); got != "networkctl reload" {
		t.Errorf("last command = %q, want the reload", got)
	}
}

// TestCreateBridgePlan covers the many-member half: every member gets its own
// file, and a member with no .network file at all gets a minimal one.
func TestCreateBridgePlan(t *testing.T) {
	model := sampleModel()
	files, staged := modelIO(model)
	spec := network.NetdevSpec{
		Kind: network.NetdevBridge, Name: "br0",
		Members: []string{"enp1s0", "enp2s0"},
	}
	plan, err := BuildCreateNetdev(model, spec, files)
	if err != nil {
		t.Fatalf("BuildCreateNetdev: %v", err)
	}
	if len(plan.Files) != 3 {
		t.Fatalf("plan touches %d files, want the unit and two members", len(plan.Files))
	}
	unit := staged[NetdevPath("br0")]
	if !strings.Contains(unit, "Kind=bridge") || strings.Contains(unit, "[VLAN]") {
		t.Errorf("the bridge unit is wrong:\n%s", unit)
	}
	if !strings.Contains(staged[ConfigDir+"/10-wired.network"], "Bridge=br0") {
		t.Errorf("enp1s0 was not enslaved")
	}
	// enp2s0 has no file yet, so one is written for it, under the tool's own
	// name and with a [Match] that actually matches.
	fresh := staged[FileName("enp2s0")]
	for _, want := range []string{ownerMarker, "[Match]\nName=enp2s0\n", "Bridge=br0"} {
		if !strings.Contains(fresh, want) {
			t.Errorf("the new member file is missing %q:\n%s", want, fresh)
		}
	}
	if len(plan.Commands) != 4 {
		t.Errorf("plan has %d commands, want three installs and a reload", len(plan.Commands))
	}
}

// TestCreateSkipsAMemberThatAlreadySaysIt: a link whose file already carries
// the member line is not rewritten, so re-running the same plan does not
// reinstall an identical file.
func TestCreateSkipsAMemberThatAlreadySaysIt(t *testing.T) {
	model := sampleModel()
	model.ConfigFiles[0] = withLinks(ParseNetworkFile(ConfigDir+"/10-wired.network",
		"[Match]\nName=enp1s0\n\n[Network]\nDHCP=ipv4\nBridge=br0\n"), "enp1s0")
	files, _ := modelIO(model)

	plan, err := BuildCreateNetdev(model, network.NetdevSpec{
		Kind: network.NetdevBridge, Name: "br0", Members: []string{"enp1s0"},
	}, files)
	if err != nil {
		t.Fatalf("BuildCreateNetdev: %v", err)
	}
	if len(plan.Files) != 1 {
		t.Errorf("plan touches %d files, want only the unit", len(plan.Files))
	}
}

// TestRemovePlan is the mirror image: the unit is deleted, every member line
// that named it is stripped, and the reload still comes last.
func TestRemovePlan(t *testing.T) {
	model := sampleModel()
	model.ConfigFiles[0] = withLinks(ParseNetworkFile(ConfigDir+"/10-wired.network",
		"[Match]\nName=enp1s0\n\n[Network]\nDHCP=ipv4\nBridge=br0\n"), "enp1s0")
	model.NetdevFiles = []network.NetdevFile{
		ParseNetdevFile(NetdevPath("br0"),
			fileHeader+"[NetDev]\nName=br0\nKind=bridge\n"),
	}
	files, staged := modelIO(model)

	plan, err := BuildRemoveNetdev(model, "br0", files)
	if err != nil {
		t.Fatalf("BuildRemoveNetdev: %v", err)
	}
	if len(plan.Files) != 2 {
		t.Fatalf("plan touches %d files, want the unit and the member", len(plan.Files))
	}
	if !plan.Files[0].Remove {
		t.Errorf("the unit is not marked for removal")
	}
	member := staged[ConfigDir+"/10-wired.network"]
	if strings.Contains(member, "Bridge=br0") {
		t.Errorf("the member line survived the removal:\n%s", member)
	}
	if !strings.Contains(member, "DHCP=ipv4") {
		t.Errorf("the removal took the member's own settings with it:\n%s", member)
	}

	// The unit's side of the diff ends in /dev/null, the way a deletion reads.
	for _, want := range []string{
		"--- " + NetdevPath("br0"),
		"+++ /dev/null",
		"-Kind=bridge",
		"-Bridge=br0",
	} {
		if !strings.Contains(plan.Diff, want) {
			t.Errorf("the removal diff is missing %q:\n%s", want, plan.Diff)
		}
	}

	if len(plan.Commands) != 3 {
		t.Fatalf("plan has %d commands, want the rm, the install and the reload:\n%v",
			len(plan.Commands), plan.Commands)
	}
	if got := plan.Commands[0].String(); got != "rm -f -- "+NetdevPath("br0") {
		t.Errorf("first command = %q, want the unit's removal", got)
	}
	if got := plan.Commands[2].String(); got != "networkctl reload" {
		t.Errorf("last command = %q, want the reload", got)
	}
}

// TestRemoveRefusesAUnitTheToolDoesNotOwn: an administrator's own bridge is not
// tui-network's to delete, and neither is a name no unit declares.
func TestRemoveRefusesAUnitTheToolDoesNotOwn(t *testing.T) {
	model := sampleModel()
	model.NetdevFiles = []network.NetdevFile{
		// No tui-network banner: somebody wrote this by hand.
		ParseNetdevFile(NetdevPath("br0"), "[NetDev]\nName=br0\nKind=bridge\n"),
		// Owned, but a kind this tool never creates.
		ParseNetdevFile(NetdevPath("bond0"),
			fileHeader+"[NetDev]\nName=bond0\nKind=bond\n"),
		// Owned, but shipped from a directory the tool must not write to.
		ParseNetdevFile("/usr/lib/systemd/network/20-br1.netdev",
			fileHeader+"[NetDev]\nName=br1\nKind=bridge\n"),
	}
	files, _ := modelIO(model)

	for _, name := range []string{"br0", "bond0", "br1", "nosuch", "br 0"} {
		if _, err := BuildRemoveNetdev(model, name, files); err == nil {
			t.Errorf("BuildRemoveNetdev(%q) built a plan, want a refusal", name)
		}
	}
}

// TestCheckConfigPathAcceptsNetdev is the write-boundary guard: the destination
// is the one thing that becomes a path in an argv run as root, so .netdev is
// allowed alongside .network and nothing else moved.
func TestCheckConfigPathAcceptsNetdev(t *testing.T) {
	for _, good := range []string{
		ConfigDir + "/20-br0.netdev",
		ConfigDir + "/50-enp1s0.network",
	} {
		if err := checkConfigPath(good); err != nil {
			t.Errorf("checkConfigPath rejected %q: %v", good, err)
		}
	}
	for _, bad := range []string{
		"/etc/passwd",
		"/etc/systemd/network/../../passwd",
		ConfigDir + "/../20-br0.netdev",
		ConfigDir + "/sub/20-br0.netdev",
		ConfigDir + "/20-br0.netdev.bak",
		ConfigDir + "/20-br0.conf",
		ConfigDir + "/.netdev",
		ConfigDir + "/.network",
		ConfigDir + "/",
		ConfigDir + "/20 br0.netdev",
		"/run/systemd/network/20-br0.netdev",
		"/usr/lib/systemd/network/20-br0.netdev",
		"20-br0.netdev",
	} {
		if err := checkConfigPath(bad); err == nil {
			t.Errorf("checkConfigPath accepted %q", bad)
		}
	}
}

func TestBuildRemoveNetdevFileRefusesEverythingElse(t *testing.T) {
	cmd, err := BuildRemoveNetdevFile(NetdevPath("br0"))
	if err != nil {
		t.Fatalf("BuildRemoveNetdevFile: %v", err)
	}
	if got := cmd.String(); got != "rm -f -- "+ConfigDir+"/20-br0.netdev" {
		t.Errorf("argv %q", got)
	}
	if !cmd.Destructive {
		t.Errorf("deleting a unit is a destructive change")
	}
	// A .network file is never deleted, only rewritten.
	for _, bad := range []string{
		ConfigDir + "/50-enp1s0.network",
		"/etc/passwd",
		ConfigDir + "/20-br0.netdev; reboot",
	} {
		if _, err := BuildRemoveNetdevFile(bad); err == nil {
			t.Errorf("BuildRemoveNetdevFile accepted %q", bad)
		}
	}
}

func TestParseNetdevFile(t *testing.T) {
	owned := ParseNetdevFile(NetdevPath("vlan10"),
		fileHeader+"[NetDev]\nName=vlan10\nKind=VLAN\n\n[VLAN]\nId=10\n")
	if owned.Name != "vlan10" || owned.Kind != "vlan" || !owned.Owned {
		t.Errorf("parsed unit = %+v", owned)
	}
	foreign := ParseNetdevFile(NetdevPath("br0"), "[NetDev]\nName=br0\nKind=bridge\n")
	if foreign.Owned {
		t.Errorf("a file with no tui-network banner must not be owned")
	}
}

// TestWithSettingPlacesTheLine covers the line edit on its own: the setting
// lands in [Network] whatever the file looks like, and never twice.
func TestWithSettingPlacesTheLine(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			"a normal file",
			"[Match]\nName=e0\n\n[Network]\nDHCP=yes\n",
			"[Match]\nName=e0\n\n[Network]\nDHCP=yes\nBridge=br0\n",
		},
		{
			"[Network] is not the last section",
			"[Match]\nName=e0\n\n[Network]\nDHCP=yes\n\n[Route]\nMetric=1\n",
			"[Match]\nName=e0\n\n[Network]\nDHCP=yes\nBridge=br0\n\n[Route]\nMetric=1\n",
		},
		{
			"an empty [Network] section",
			"[Match]\nName=e0\n\n[Network]\n",
			"[Match]\nName=e0\n\n[Network]\nBridge=br0\n",
		},
		{
			"no [Network] section at all",
			"[Match]\nName=e0\n",
			"[Match]\nName=e0\n\n[Network]\nBridge=br0\n",
		},
		{
			"it is already there",
			"[Match]\nName=e0\n\n[Network]\nBridge=br0\n",
			"[Match]\nName=e0\n\n[Network]\nBridge=br0\n",
		},
	}
	for _, test := range tests {
		if got := withSetting(test.text, "e0", "Bridge", "br0"); got != test.want {
			t.Errorf("%s:\n got %q\nwant %q", test.name, got, test.want)
		}
	}

	// An empty file becomes the smallest one that means anything.
	fresh := withSetting("", "e0", "Bridge", "br0")
	for _, want := range []string{ownerMarker, "[Match]\nName=e0\n", "Bridge=br0\n"} {
		if !strings.Contains(fresh, want) {
			t.Errorf("the generated member file is missing %q:\n%s", want, fresh)
		}
	}
}

func TestWithoutSettingRemovesOnlyThatLine(t *testing.T) {
	text := "[Match]\nName=e0\n\n[Network]\nDHCP=yes\nBridge=br0\nBridge=br1\n"
	got := withoutSetting(text, "Bridge", "br0")
	want := "[Match]\nName=e0\n\n[Network]\nDHCP=yes\nBridge=br1\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// A setting that is not there leaves the file byte for byte alone, which is
	// what makes a no-op removal produce no command.
	if again := withoutSetting(got, "Bridge", "br0"); again != got {
		t.Errorf("removing an absent setting changed the file")
	}
}
