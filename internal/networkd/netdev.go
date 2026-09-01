package networkd

// VLANs and bridges, as systemd-networkd builds them: a `.netdev` unit that
// declares the device, and a member line in the `.network` file of every link
// that feeds it. Neither half works alone — a .netdev with no member is a
// device nothing reaches, and a `Bridge=` line pointing at no unit is an error
// networkd logs and ignores — so both are rendered, diffed and installed as one
// confirmed plan.
//
// Everything here is pure: it takes the model the UI already read and a FileIO,
// and returns a WritePlan. The real backend hands it real files and the fake
// hands it its in-memory machine, which is what makes --demo build exactly the
// plan a real run would.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tui-tools/tui-network/internal/network"
)

// netdevPrefix is the numeric prefix a written .netdev unit gets. It sorts
// below the 50- the link files use, so `ls /etc/systemd/network` reads
// devices-then-links, which is also the order they come into being.
const netdevPrefix = "20-"

// ownerMarker is the line every file tui-network generates starts with. It is
// what tells a unit this tool wrote from one an administrator wrote by hand,
// and removal is offered only for the first kind.
const ownerMarker = "# Written by tui-network"

// fileHeader is that marker in full, the banner a generated file opens with.
const fileHeader = ownerMarker + ". Edit it here or by hand;\n" +
	"# systemd-networkd re-reads it on `networkctl reload`.\n\n"

// FileIO is how a plan reads what is on disk today and stages what it is about
// to install. The real backend reads real files and stages into a private
// temporary directory; the fake keeps both in memory. Nothing here writes: a
// staged file is a draft, and only the confirmed `install` puts it in /etc.
type FileIO struct {
	// Read returns a file's text, and an empty string when it does not exist.
	Read func(path string) (string, error)
	// Stage writes the pending content somewhere the install can copy from,
	// and returns that path.
	Stage func(path, content string) (string, error)
}

// NetdevPath is where a device's unit is written.
func NetdevPath(name string) string {
	return fmt.Sprintf("%s/%s%s%s", ConfigDir, netdevPrefix, name, NetdevSuffix)
}

// checkNetdevName rejects a name that is not a plausible device name. The name
// reaches a file name in /etc and a `Bridge=`/`VLAN=` value, so it is held to
// the same rule as an interface name — which is what it becomes.
func checkNetdevName(name string) error {
	if !linkNameRe.MatchString(name) {
		return fmt.Errorf("networkd: %q is not a valid device name", name)
	}
	return nil
}

// memberKey is the `.network` setting that attaches a link to a device: a VLAN
// is listed on its parent, a bridge is named on each of its members.
func memberKey(kind string) (string, error) {
	switch kind {
	case network.NetdevVLAN:
		return "VLAN", nil
	case network.NetdevBridge:
		return "Bridge", nil
	default:
		return "", fmt.Errorf("networkd: %q is not a device kind tui-network creates",
			kind)
	}
}

// RenderNetdev turns a spec into the text of its .netdev unit.
func RenderNetdev(spec network.NetdevSpec) (string, error) {
	if err := checkNetdevName(spec.Name); err != nil {
		return "", err
	}
	if _, err := memberKey(spec.Kind); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(fileHeader)
	b.WriteString("[NetDev]\n")
	fmt.Fprintf(&b, "Name=%s\n", spec.Name)
	fmt.Fprintf(&b, "Kind=%s\n", spec.Kind)

	if spec.Kind == network.NetdevVLAN {
		if spec.VLANID < network.MinVLANID || spec.VLANID > network.MaxVLANID {
			return "", fmt.Errorf("networkd: a VLAN id is between %d and %d, not %d",
				network.MinVLANID, network.MaxVLANID, spec.VLANID)
		}
		b.WriteString("\n[VLAN]\n")
		fmt.Fprintf(&b, "Id=%d\n", spec.VLANID)
	}
	return b.String(), nil
}

// checkNetdevSpec refuses a device the machine cannot give the user, before any
// file is rendered: a name that is already taken, a parent or member that is
// not a link networkd manages, an empty bridge, a VLAN id out of range.
func checkNetdevSpec(spec network.NetdevSpec, model network.Model) error {
	if err := checkNetdevName(spec.Name); err != nil {
		return err
	}
	if _, err := memberKey(spec.Kind); err != nil {
		return err
	}
	if _, taken := model.Netdev(spec.Name); taken {
		return fmt.Errorf("networkd: a .netdev unit already declares %q", spec.Name)
	}
	if _, taken := model.Link(spec.Name); taken {
		return fmt.Errorf("networkd: %s is already a link on this machine", spec.Name)
	}
	for _, unit := range model.NetdevFiles {
		if unit.Path == NetdevPath(spec.Name) {
			return fmt.Errorf("networkd: %s already exists", unit.Path)
		}
	}

	members := spec.MemberLinks()
	if len(members) == 0 {
		if spec.Kind == network.NetdevVLAN {
			return fmt.Errorf("networkd: a VLAN needs a parent link")
		}
		return fmt.Errorf("networkd: a bridge needs at least one member link")
	}
	seen := map[string]bool{}
	for _, member := range members {
		if seen[member] {
			return fmt.Errorf("networkd: %s is listed twice", member)
		}
		seen[member] = true
		if member == spec.Name {
			return fmt.Errorf("networkd: %s cannot be its own member", spec.Name)
		}
		link, ok := model.Link(member)
		if !ok {
			return fmt.Errorf("networkd: there is no link named %q", member)
		}
		if !link.Managed {
			reason := link.ReadOnlyReason
			if reason == "" {
				reason = "it is not managed by systemd-networkd"
			}
			return fmt.Errorf("networkd: %s is read-only: %s", member, reason)
		}
	}
	if spec.Kind == network.NetdevVLAN &&
		(spec.VLANID < network.MinVLANID || spec.VLANID > network.MaxVLANID) {
		return fmt.Errorf("networkd: a VLAN id is between %d and %d, not %d",
			network.MinVLANID, network.MaxVLANID, spec.VLANID)
	}
	return nil
}

// BuildCreateNetdev assembles the whole change: the .netdev unit, plus the
// member line in the .network file of every link the device gathers.
func BuildCreateNetdev(model network.Model, spec network.NetdevSpec,
	files FileIO) (network.WritePlan, error) {
	if err := checkNetdevSpec(spec, model); err != nil {
		return network.WritePlan{}, err
	}
	key, err := memberKey(spec.Kind)
	if err != nil {
		return network.WritePlan{}, err
	}
	content, err := RenderNetdev(spec)
	if err != nil {
		return network.WritePlan{}, err
	}

	unitPath := NetdevPath(spec.Name)
	before, err := files.Read(unitPath)
	if err != nil {
		return network.WritePlan{}, err
	}
	changes := []network.FileChange{
		{Path: unitPath, Before: before, Content: content},
	}

	for _, member := range spec.MemberLinks() {
		change, err := memberChange(model, member, key, spec.Name, files)
		if err != nil {
			return network.WritePlan{}, err
		}
		if change.Content == change.Before {
			// The link already says it: nothing to install for this member.
			continue
		}
		changes = append(changes, change)
	}
	return netdevPlan(changes, files)
}

// BuildRemoveNetdev is the mirror image of the creation: it deletes the unit
// and strips the member line from every .network file that named it, so the
// machine is left the way it was before the device existed.
func BuildRemoveNetdev(model network.Model, name string,
	files FileIO) (network.WritePlan, error) {
	if err := checkNetdevName(name); err != nil {
		return network.WritePlan{}, err
	}
	unit, ok := model.Netdev(name)
	if !ok {
		return network.WritePlan{}, fmt.Errorf(
			"networkd: no .netdev unit on this machine declares %q", name)
	}
	if !unit.Owned {
		return network.WritePlan{}, fmt.Errorf(
			"networkd: %s was not written by tui-network, so it is not removed here",
			unit.Path)
	}
	if !strings.HasPrefix(unit.Path, ConfigDir+"/") {
		return network.WritePlan{}, fmt.Errorf(
			"networkd: %s is outside %s", unit.Path, ConfigDir)
	}
	key, err := memberKey(unit.Kind)
	if err != nil {
		return network.WritePlan{}, err
	}

	changes := []network.FileChange{
		{Path: unit.Path, Before: unit.Raw, Remove: true},
	}
	for _, file := range model.ConfigFiles {
		// Only a file this tool could have written the line into is rewritten.
		// It never wrote one anywhere else, and a distribution file is not
		// ours to edit.
		if !strings.HasPrefix(file.Path, ConfigDir+"/") {
			continue
		}
		if !hasSetting(file.Raw, key, name) {
			continue
		}
		before, err := files.Read(file.Path)
		if err != nil {
			return network.WritePlan{}, err
		}
		content := withoutSetting(before, key, name)
		if content == before {
			continue
		}
		changes = append(changes, network.FileChange{
			Path: file.Path, Before: before, Content: content,
		})
	}
	return netdevPlan(changes, files)
}

// memberChange resolves which .network file carries a link's member line and
// applies edit to it.
//
// The destination is always under ConfigDir: a link whose file is one the
// distribution ships gets an administrator copy instead, the same rule the
// guided .network editor follows, so a package upgrade never reverts the change.
func memberChange(model network.Model, link, key, value string,
	files FileIO) (network.FileChange, error) {
	l, ok := model.Link(link)
	if !ok {
		return network.FileChange{}, fmt.Errorf("networkd: there is no link named %q", link)
	}
	path, source := FileName(link), ""
	if file, found := model.ConfigFor(l); found {
		source = file.Raw
		if strings.HasPrefix(file.Path, ConfigDir+"/") {
			path = file.Path
		}
	}
	before, err := files.Read(path)
	if err != nil {
		return network.FileChange{}, err
	}
	if before != "" {
		// The destination exists: edit what is really there rather than the
		// copy the model happens to hold.
		source = before
	}
	return network.FileChange{
		Path:    path,
		Before:  before,
		Content: withSetting(source, link, key, value),
	}, nil
}

// netdevPlan turns the file changes into the reviewable plan: one diff over all
// of them, then an install (or a remove) per file and a single reload at the
// end — because reloading once, after every file is in place, is the only order
// in which networkd never sees half a device.
func netdevPlan(changes []network.FileChange, files FileIO) (network.WritePlan, error) {
	var diffs []string
	var commands []network.Command
	temp := ""
	for _, change := range changes {
		if diff := Diff(change.Path, change.Before, change.Content); diff != "" {
			diffs = append(diffs, diff)
		}
		if change.Remove {
			cmd, err := BuildRemoveNetdevFile(change.Path)
			if err != nil {
				return network.WritePlan{}, err
			}
			commands = append(commands, cmd)
			continue
		}
		staged, err := files.Stage(change.Path, change.Content)
		if err != nil {
			return network.WritePlan{}, err
		}
		if temp == "" {
			temp = staged
		}
		cmd, err := BuildInstallFile(staged, change.Path)
		if err != nil {
			return network.WritePlan{}, err
		}
		commands = append(commands, cmd)
	}
	if len(commands) == 0 {
		return network.WritePlan{}, fmt.Errorf("this machine already says exactly this")
	}
	reload, err := BuildReload()
	if err != nil {
		return network.WritePlan{}, err
	}
	commands = append(commands, reload)

	return network.WritePlan{
		Path:     changes[0].Path,
		Content:  changes[0].Content,
		Diff:     strings.Join(diffs, ""),
		TempPath: temp,
		Files:    changes,
		Commands: commands,
	}, nil
}

// ParseNetdevFile reads a systemd .netdev unit into what the tool needs to know
// about it: which device it declares, of which kind, and whether tui-network
// wrote it.
func ParseNetdevFile(path, raw string) network.NetdevFile {
	parsed := ParseNetworkFile(path, raw)
	unit := network.NetdevFile{
		Path:     path,
		Raw:      raw,
		Settings: parsed.Settings,
		Owned:    strings.Contains(raw, ownerMarker),
	}
	if name, ok := parsed.Get("NetDev", "Name"); ok {
		unit.Name = name
	}
	if kind, ok := parsed.Get("NetDev", "Kind"); ok {
		unit.Kind = strings.ToLower(kind)
	}
	return unit
}

// withSetting returns the text of a .network file with `key=value` present in
// its [Network] section.
//
// It is a line edit rather than a regeneration on purpose: the file being
// changed is the link's own, and it may hold a dozen settings the guided form
// does not model. Adding one line is the whole change, and the diff says so.
func withSetting(text, link, key, value string) string {
	line := key + "=" + value
	if hasSetting(text, key, value) {
		return text
	}
	if strings.TrimSpace(text) == "" {
		// The link has no file yet: write the smallest one that means anything.
		return fileHeader + "[Match]\nName=" + link + "\n\n[Network]\n" + line + "\n"
	}

	lines := splitLines(text)
	// header is the [Network] line; last is the final line belonging to that
	// section, which is where the new setting goes.
	header, last := -1, -1
	for i, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if isSectionHeader(trimmed) {
			if header >= 0 {
				break
			}
			if strings.EqualFold(trimmed, "[Network]") {
				header, last = i, i
			}
			continue
		}
		if header >= 0 && trimmed != "" && !isComment(trimmed) {
			last = i
		}
	}
	if header < 0 {
		return strings.TrimRight(text, "\n") + "\n\n[Network]\n" + line + "\n"
	}
	out := append([]string{}, lines[:last+1]...)
	out = append(out, line)
	out = append(out, lines[last+1:]...)
	return strings.Join(out, "\n") + "\n"
}

// withoutSetting returns the text with every `key=value` line dropped.
func withoutSetting(text, key, value string) string {
	lines := splitLines(text)
	out := make([]string, 0, len(lines))
	for _, raw := range lines {
		if matchesSetting(raw, key, value) {
			continue
		}
		out = append(out, raw)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// hasSetting reports whether the text already carries `key=value`.
func hasSetting(text, key, value string) bool {
	for _, raw := range splitLines(text) {
		if matchesSetting(raw, key, value) {
			return true
		}
	}
	return false
}

// matchesSetting reports whether one line is exactly `key=value`, ignoring
// spacing and the case of the key, the way systemd reads it.
func matchesSetting(raw, key, value string) bool {
	k, v, found := strings.Cut(strings.TrimSpace(raw), "=")
	return found && strings.EqualFold(strings.TrimSpace(k), key) &&
		strings.TrimSpace(v) == value
}

// isSectionHeader reports whether a trimmed line opens a section.
func isSectionHeader(trimmed string) bool {
	return strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")
}

// isComment reports whether a trimmed line is a comment, in either of the two
// spellings systemd accepts.
func isComment(trimmed string) bool {
	return strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";")
}

// ParseVLANID reads a VLAN id typed into the form, refusing anything outside
// the 802.1Q range with the same message the renderer would give.
func ParseVLANID(text string) (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || id < network.MinVLANID || id > network.MaxVLANID {
		return 0, fmt.Errorf("networkd: a VLAN id is between %d and %d, not %q",
			network.MinVLANID, network.MaxVLANID, text)
	}
	return id, nil
}
