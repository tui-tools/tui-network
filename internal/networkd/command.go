package networkd

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tui-tools/tui-network/internal/network"
)

// The version-gated capabilities of the backend, named the way the manifest
// names them. A tool asks the compat set for these instead of comparing
// version numbers in the code.
const (
	// FeatureJSONStatus is `networkctl --json`, which arrived in systemd 249.
	// Without it the text output is parsed instead.
	FeatureJSONStatus = "json-status"
	// FeatureLinkUpDown is `networkctl up|down`, also new in 249.
	FeatureLinkUpDown = "link-up-down"
)

// ConfigDir is where systemd-networkd reads administrator configuration from,
// and the only directory tui-network writes to.
const ConfigDir = "/etc/systemd/network"

// ConfigDirs are the directories a .network file can legitimately come from,
// searched in the order networkd itself searches them. Only ConfigDir is
// writable; the others are read for display.
var ConfigDirs = []string{
	ConfigDir,
	"/run/systemd/network",
	"/usr/lib/systemd/network",
	"/lib/systemd/network",
}

// FileMode is the mode a written .network file gets: readable by everyone,
// writable only by root, which is what systemd ships its own files with.
const FileMode = "644"

// capabilities describes what the networkd backend supports. It is shared by
// the real and the fake backend, so --demo behaves exactly like a real run.
var capabilities = network.Capabilities{
	DHCPModes:           []string{"yes", "ipv4", "ipv6", "no"},
	SupportsUpDown:      true,
	SupportsRenew:       true,
	SupportsFlushCaches: true,
	SupportsRuntimeDNS:  true,
	SupportsFileEdit:    true,
	ConfigDir:           ConfigDir,
}

// Capabilities reports what the networkd backend supports.
func Capabilities() network.Capabilities { return capabilities }

// linkNameRe is the set of characters a network interface name may contain.
// Every command builder validates the link name against it, because the name
// is the one argument that comes from the machine and ends up in an argv.
var linkNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,14}$`)

// checkLink rejects a link name that is not a plausible interface name.
func checkLink(link string) error {
	if !linkNameRe.MatchString(link) {
		return fmt.Errorf("networkd: %q is not a valid interface name", link)
	}
	return nil
}

// The one-line descriptions of each link action, and whether it is the kind of
// change that can drop the connection the user is working over.
var linkActions = map[string]struct {
	description string
	destructive bool
}{
	network.ActionUp:          {"Bring %s up", false},
	network.ActionDown:        {"Take %s down", true},
	network.ActionReconfigure: {"Reconfigure %s from its .network file", true},
	network.ActionRenew:       {"Renew the dynamic lease on %s", false},
}

// BuildLinkAction turns one of the link verbs into a networkctl command.
func BuildLinkAction(action, link string) (network.Command, error) {
	if err := checkLink(link); err != nil {
		return network.Command{}, err
	}
	spec, ok := linkActions[action]
	if !ok {
		return network.Command{}, fmt.Errorf("networkd: unknown link action %q", action)
	}
	return network.Command{
		Argv:        []string{"networkctl", action, link},
		Description: fmt.Sprintf(spec.description, link),
		Destructive: spec.destructive,
	}, nil
}

// BuildReload re-reads the .network and .netdev files. It does not re-apply
// them to links that are already configured — that is what reconfigure does —
// so it is not marked destructive.
func BuildReload() (network.Command, error) {
	return network.Command{
		Argv:        []string{"networkctl", "reload"},
		Description: "Reload the networkd configuration files",
	}, nil
}

// BuildFlushCaches empties systemd-resolved's cache.
func BuildFlushCaches() (network.Command, error) {
	return network.Command{
		Argv:        []string{"resolvectl", "flush-caches"},
		Description: "Flush the resolver cache",
	}, nil
}

// serverRe accepts the DNS server forms resolvectl takes: a bare address, and
// systemd's address#servername form for DNS-over-TLS.
var serverRe = regexp.MustCompile(`^[0-9A-Fa-f:.\[\]]+(?:#[A-Za-z0-9.-]+)?$`)

// BuildSetDNS sets a link's DNS servers at runtime. The change lives until the
// link is reconfigured: it is the fast fix, and the .network file is the
// durable one, which is why the UI offers both.
func BuildSetDNS(link string, servers []string) (network.Command, error) {
	if err := checkLink(link); err != nil {
		return network.Command{}, err
	}
	for _, server := range servers {
		if !serverRe.MatchString(server) {
			return network.Command{}, fmt.Errorf(
				"networkd: %q is not a DNS server address", server)
		}
	}
	argv := append([]string{"resolvectl", "dns", link}, servers...)
	description := fmt.Sprintf("Set the DNS servers of %s to %s",
		link, strings.Join(servers, ", "))
	if len(servers) == 0 {
		description = "Clear the DNS servers of " + link
	}
	return network.Command{Argv: argv, Description: description, Destructive: true}, nil
}

// domainRe accepts a search domain, including the routing forms resolvectl
// understands: a leading "~" for a routing-only domain and "." for the
// catch-all route.
var domainRe = regexp.MustCompile(`^~?(\.|[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*)$`)

// BuildSetDomains sets a link's search domains at runtime.
func BuildSetDomains(link string, domains []string) (network.Command, error) {
	if err := checkLink(link); err != nil {
		return network.Command{}, err
	}
	for _, domain := range domains {
		if !domainRe.MatchString(domain) {
			return network.Command{}, fmt.Errorf(
				"networkd: %q is not a search domain", domain)
		}
	}
	argv := append([]string{"resolvectl", "domain", link}, domains...)
	description := fmt.Sprintf("Set the search domains of %s to %s",
		link, strings.Join(domains, ", "))
	if len(domains) == 0 {
		description = "Clear the search domains of " + link
	}
	return network.Command{Argv: argv, Description: description, Destructive: true}, nil
}

// BuildInstallFile copies a staged file into the networkd configuration
// directory. `install` is used rather than `cp` because it sets the mode in
// the same call, so there is no window where the file is on disk with the
// wrong permissions.
func BuildInstallFile(tempPath, destination string) (network.Command, error) {
	if err := checkConfigPath(destination); err != nil {
		return network.Command{}, err
	}
	return network.Command{
		Argv: []string{"install", "-m", FileMode, tempPath, destination},
		Description: fmt.Sprintf("Install %s as %s",
			tempPath, destination),
		Destructive: true,
	}, nil
}

// checkConfigPath refuses to write anywhere but the networkd configuration
// directory, and refuses a name systemd would not read.
func checkConfigPath(path string) error {
	if !strings.HasPrefix(path, ConfigDir+"/") {
		return fmt.Errorf("networkd: refusing to write outside %s", ConfigDir)
	}
	name := strings.TrimPrefix(path, ConfigDir+"/")
	if strings.ContainsAny(name, "/ \t") || name == "" {
		return fmt.Errorf("networkd: %q is not a file name", name)
	}
	if !strings.HasSuffix(name, ".network") {
		return fmt.Errorf("networkd: a networkd configuration file must end in .network")
	}
	return nil
}

// FileName is the file a link's configuration is written to when the link has
// none yet: a numeric prefix systemd sorts on, then the link name.
func FileName(link string) string {
	return fmt.Sprintf("%s/50-%s.network", ConfigDir, link)
}

// addressRe accepts an address in CIDR form, which is what a .network file's
// Address= expects.
var addressRe = regexp.MustCompile(`^[0-9A-Fa-f:.]+/[0-9]{1,3}$`)

// RenderFile turns a FileSpec into the text of a .network file.
//
// It writes the file tui-network would want to read back: one setting per
// line, and the sections in systemd's own order.
//
// existing is what the destination holds today, and it decides one thing: a
// file being created gets a header naming the tool that wrote it, and a file
// being edited does not — because the generated text replaces the file whole,
// and adding a banner to somebody else's file would bury the setting that
// actually changed under a diff of the entire thing.
func RenderFile(spec network.FileSpec, existing string) (string, error) {
	if err := checkConfigPath(spec.Path); err != nil {
		return "", err
	}
	if strings.TrimSpace(spec.MatchName) == "" {
		return "", fmt.Errorf("networkd: [Match] Name is required")
	}
	if err := checkLink(spec.MatchName); err != nil {
		return "", err
	}
	if !validDHCP(spec.DHCP) {
		return "", fmt.Errorf("networkd: DHCP must be one of %s",
			strings.Join(capabilities.DHCPModes, ", "))
	}
	static := spec.DHCP == "no" || spec.DHCP == "ipv6"
	if static && spec.Address == "" {
		return "", fmt.Errorf(
			"networkd: with DHCP=%s the link needs a static Address", spec.DHCP)
	}
	if spec.Address != "" && !addressRe.MatchString(spec.Address) {
		return "", fmt.Errorf(
			"networkd: Address must be in CIDR form, like 192.0.2.10/24")
	}
	if spec.Gateway != "" && !serverRe.MatchString(spec.Gateway) {
		return "", fmt.Errorf("networkd: %q is not a gateway address", spec.Gateway)
	}
	for _, server := range spec.DNS {
		if !serverRe.MatchString(server) {
			return "", fmt.Errorf("networkd: %q is not a DNS server address", server)
		}
	}
	for _, domain := range spec.Domains {
		if !domainRe.MatchString(domain) {
			return "", fmt.Errorf("networkd: %q is not a search domain", domain)
		}
	}

	var b strings.Builder
	if strings.TrimSpace(existing) == "" {
		b.WriteString("# Written by tui-network. Edit it here or by hand;\n")
		b.WriteString("# systemd-networkd re-reads it on `networkctl reload`.\n\n")
	}
	b.WriteString("[Match]\n")
	fmt.Fprintf(&b, "Name=%s\n", spec.MatchName)
	b.WriteString("\n[Network]\n")
	fmt.Fprintf(&b, "DHCP=%s\n", spec.DHCP)
	if spec.Address != "" {
		fmt.Fprintf(&b, "Address=%s\n", spec.Address)
	}
	if spec.Gateway != "" {
		fmt.Fprintf(&b, "Gateway=%s\n", spec.Gateway)
	}
	for _, server := range spec.DNS {
		fmt.Fprintf(&b, "DNS=%s\n", server)
	}
	if len(spec.Domains) > 0 {
		fmt.Fprintf(&b, "Domains=%s\n", strings.Join(spec.Domains, " "))
	}
	return b.String(), nil
}

// validDHCP reports whether a DHCP mode is one systemd accepts.
func validDHCP(mode string) bool {
	for _, known := range capabilities.DHCPModes {
		if mode == known {
			return true
		}
	}
	return false
}

// SpecFromFile seeds the guided form from a file that already exists, so
// editing starts from what is on disk instead of from an empty form.
func SpecFromFile(file network.ConfigFile, link string) network.FileSpec {
	spec := network.FileSpec{
		Path:      file.Path,
		MatchName: file.MatchName,
		DHCP:      "no",
	}
	if spec.Path == "" || !strings.HasPrefix(spec.Path, ConfigDir+"/") {
		// A file shipped by the distribution is never edited in place: the
		// form writes an administrator copy that overrides it.
		spec.Path = FileName(link)
	}
	if spec.MatchName == "" {
		spec.MatchName = link
	}
	if value, ok := file.Get("Network", "DHCP"); ok {
		spec.DHCP = normalizeDHCP(value)
	}
	if addresses := file.All("Network", "Address"); len(addresses) > 0 {
		spec.Address = addresses[0]
	}
	if gateway, ok := file.Get("Network", "Gateway"); ok {
		spec.Gateway = gateway
	}
	spec.DNS = file.All("Network", "DNS")
	for _, domains := range file.All("Network", "Domains") {
		spec.Domains = append(spec.Domains, strings.Fields(domains)...)
	}
	return spec
}

// normalizeDHCP maps the boolean spellings systemd accepts onto the four
// values the form offers.
func normalizeDHCP(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "yes", "true", "on", "both":
		return "yes"
	case "no", "false", "off", "none", "":
		return "no"
	case "ipv4", "v4":
		return "ipv4"
	case "ipv6", "v6":
		return "ipv6"
	default:
		return value
	}
}

// diffContext is how many unchanged lines are shown around a change. Two is
// enough to place a `DNS=` line in its section without turning a ten line file
// into a wall of text in the confirm dialog.
const diffContext = 2

// Diff renders a unified diff between two versions of a file.
//
// It is a real line diff — a longest-common-subsequence walk — rather than
// "everything out, everything in", because the confirm dialog for a file edit
// has one job: show the setting that changed. A diff that repeats the whole
// file every time a comment moved buries exactly that.
//
// The files here are a dozen lines, so the quadratic table costs nothing.
func Diff(path, before, after string) string {
	if before == after {
		return ""
	}
	oldLines, newLines := splitLines(before), splitLines(after)
	ops := diffOps(oldLines, newLines)
	hunks := hunksOf(ops)
	if len(hunks) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n", labelFor(path, before))
	fmt.Fprintf(&b, "+++ %s\n", path)
	for _, hunk := range hunks {
		oldCount, newCount := 0, 0
		for _, op := range hunk.ops {
			if op.kind != '+' {
				oldCount++
			}
			if op.kind != '-' {
				newCount++
			}
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n",
			hunk.oldStart+1, oldCount, hunk.newStart+1, newCount)
		for _, op := range hunk.ops {
			fmt.Fprintf(&b, "%c%s\n", op.kind, op.text)
		}
	}
	return b.String()
}

// op is one line of a diff: kept (' '), removed ('-') or added ('+').
type op struct {
	kind byte
	text string
	// oldIndex and newIndex are the line's position in each file, used to
	// number the hunk headers.
	oldIndex, newIndex int
}

// diffOps walks the longest common subsequence of the two line lists and
// returns the operations that turn the first into the second.
func diffOps(oldLines, newLines []string) []op {
	// lcs[i][j] is the length of the longest common subsequence of
	// oldLines[i:] and newLines[j:].
	lcs := make([][]int, len(oldLines)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(newLines)+1)
	}
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
				continue
			}
			lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
		}
	}

	var ops []op
	i, j := 0, 0
	for i < len(oldLines) && j < len(newLines) {
		switch {
		case oldLines[i] == newLines[j]:
			ops = append(ops, op{' ', oldLines[i], i, j})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{'-', oldLines[i], i, j})
			i++
		default:
			ops = append(ops, op{'+', newLines[j], i, j})
			j++
		}
	}
	for ; i < len(oldLines); i++ {
		ops = append(ops, op{'-', oldLines[i], i, j})
	}
	for ; j < len(newLines); j++ {
		ops = append(ops, op{'+', newLines[j], i, j})
	}
	return ops
}

// hunk is a run of changes with its surrounding context.
type hunk struct {
	oldStart, newStart int
	ops                []op
}

// hunksOf groups the operations into hunks, keeping diffContext unchanged
// lines around each change and merging changes that are close enough to share
// their context.
func hunksOf(ops []op) []hunk {
	keep := make([]bool, len(ops))
	for i, o := range ops {
		if o.kind == ' ' {
			continue
		}
		for j := max(i-diffContext, 0); j <= min(i+diffContext, len(ops)-1); j++ {
			keep[j] = true
		}
	}

	var hunks []hunk
	var current *hunk
	for i, o := range ops {
		if !keep[i] {
			current = nil
			continue
		}
		if current == nil {
			hunks = append(hunks, hunk{oldStart: o.oldIndex, newStart: o.newIndex})
			current = &hunks[len(hunks)-1]
		}
		current.ops = append(current.ops, o)
	}
	return hunks
}

// labelFor names the left side of the diff: the file, or /dev/null when it
// does not exist yet.
func labelFor(path, before string) string {
	if before == "" {
		return "/dev/null"
	}
	return path
}

// splitLines splits a file into lines, dropping the empty element a trailing
// newline produces.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	return lines
}
