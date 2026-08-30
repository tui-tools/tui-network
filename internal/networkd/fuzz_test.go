package networkd

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tui-tools/tui-network/internal/network"
)

// The parsers in this file's package are where output tui-network did not
// write becomes the model the UI shows and the commands the tool then offers
// to run: `networkctl` JSON and columns, `ip -j route`, `resolvectl dns`, and
// a `.network` file read off disk. A parser that invents a link name, or a
// setup state that disagrees with the managed flag, is how a tool ends up
// offering to reconfigure something it never really read.
//
// `go test` runs the seeds below on every commit; `go test -fuzz=FuzzParseX
// ./internal/networkd/` explores past them locally — see
// tui-kit/templates/FUZZING.md for the family rule.

// seed adds every named testdata file to the corpus, plus the shapes a real
// capture never has: nothing, a lone separator, a truncated line.
func seed(f *testing.F, names ...string) {
	f.Helper()
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // the name is a literal in the tests, and testdata is in the repository
		if err != nil {
			f.Fatalf("read fixture %s: %v", name, err)
		}
		f.Add(string(raw))
	}
	f.Add("")
	f.Add("\n\n\n")
	f.Add(":")
	f.Add("[]")
	f.Add("{}")
}

// checkAddress asserts what every caller of an address is allowed to assume:
// something it can print, a family it can branch on, and a scope that is never
// blank.
func checkAddress(t *testing.T, a network.Address) {
	t.Helper()
	if a.Address == "" {
		t.Fatalf("address with no textual form: %+v", a)
	}
	if strings.ContainsAny(a.Address, " \t\n") {
		t.Fatalf("address carries whitespace: %q", a.Address)
	}
	if a.Family != "" && a.Family != "ipv4" && a.Family != "ipv6" {
		t.Fatalf("address family is not a family: %q", a.Family)
	}
	if a.Scope == "" {
		t.Fatalf("address has no scope: %+v", a)
	}
	// String() is what the UI prints, so it has to survive any input.
	if a.String() == "" {
		t.Fatalf("address renders as nothing: %+v", a)
	}
}

// checkLink asserts the one relation the whole tool leans on: a link is
// editable only when networkd says it owns it, and a link the tool will not
// edit always carries the reason it will not.
func checkParsedLink(t *testing.T, l network.Link) {
	t.Helper()
	managed := l.Setup != "" && l.Setup != network.SetupUnmanaged
	if l.Managed != managed {
		t.Fatalf("managed flag disagrees with setup state %q: %+v", l.Setup, l)
	}
	if !l.Managed && l.ReadOnlyReason == "" {
		t.Fatalf("unmanaged link with no reason: %+v", l)
	}
	if strings.ContainsAny(l.Name, " \t\n") {
		t.Fatalf("link name carries whitespace: %q", l.Name)
	}
	for _, a := range l.Addresses {
		checkAddress(t, a)
	}
	for _, g := range l.Gateways {
		if g == "" || strings.ContainsAny(g, " \t\n") {
			t.Fatalf("gateway is not a bare address: %q", g)
		}
	}
	for _, s := range l.DNS {
		if s == "" || strings.ContainsAny(s, " \t\n") {
			t.Fatalf("DNS server is not a bare value: %q", s)
		}
	}
}

func FuzzParseListJSON(f *testing.F) {
	seed(f, "networkctl-list.json", "networkctl-list-systemd255.json",
		"networkctl-list-systemd261.json")
	f.Fuzz(func(t *testing.T, out string) {
		links, err := ParseListJSON(out)
		if err != nil {
			if len(links) != 0 {
				t.Fatalf("failed and still returned %d links", len(links))
			}
			return
		}
		for _, l := range links {
			checkParsedLink(t, l)
		}
	})
}

func FuzzParseStatusJSON(f *testing.F) {
	seed(f, "networkctl-status-lo.json", "networkctl-status-veth0.json",
		"networkctl-status-systemd255.json", "networkctl-status-systemd261.json")
	f.Fuzz(func(t *testing.T, out string) {
		link, err := ParseStatusJSON(out)
		if err != nil {
			if link.Name != "" || link.Index != 0 {
				t.Fatalf("failed and still returned a link: %+v", link)
			}
			return
		}
		checkParsedLink(t, link)
	})
}

// FuzzParseRoutesJSON matters because the routes are read straight out of
// `ip -j route`: whatever family a route claims is what the UI files it under.
func FuzzParseRoutesJSON(f *testing.F) {
	seed(f, "ip-route.json")
	f.Fuzz(func(t *testing.T, out string) {
		routes, err := ParseRoutesJSON(out)
		if err != nil {
			if len(routes) != 0 {
				t.Fatalf("failed and still returned %d routes", len(routes))
			}
			return
		}
		for _, r := range routes {
			if r.Family != "" && r.Family != "ipv4" && r.Family != "ipv6" {
				t.Fatalf("route family is not a family: %q", r.Family)
			}
			// A family is claimed only when an address in the route says so.
			if r.Family != "" {
				if !hasFamily(r.Destination, r.Family) && !hasFamily(r.Gateway, r.Family) {
					t.Fatalf("route claims %q with no address of that family: %+v", r.Family, r)
				}
			}
		}
	})
}

// hasFamily reports whether the candidate is an address of the named family.
func hasFamily(candidate, family string) bool {
	host, _, found := strings.Cut(candidate, "/")
	if !found {
		host = candidate
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	if addr.Is4() {
		return family == "ipv4"
	}
	return family == "ipv6"
}

func FuzzParseListText(f *testing.F) {
	seed(f, "networkctl-list.txt")
	f.Add("IDX LINK TYPE OPERATIONAL SETUP\n  1 lo loopback carrier unmanaged\n")
	f.Add("1 lo\n")
	f.Fuzz(func(t *testing.T, out string) {
		for _, l := range ParseListText(out) {
			if l.Name == "" {
				t.Fatalf("link with no name: %+v", l)
			}
			checkParsedLink(t, l)
		}
	})
}

func FuzzParseStatusText(f *testing.F) {
	seed(f, "networkctl-status-veth0.txt")
	f.Add("● 4: veth0\n     Network File: /etc/systemd/network/10-veth0.network\n")
	f.Add("● 4: veth0\n            MTU: \n        Address: \n")
	f.Fuzz(func(t *testing.T, out string) {
		checkParsedLink(t, ParseStatusText(out))
	})
}

func FuzzParseResolvectlDNS(f *testing.F) {
	seed(f, "resolvectl-dns.txt", "resolvectl-domain.txt", "resolvectl-status.txt")
	f.Add("Global:\nLink 2 (eth0): 192.0.2.1\n")
	f.Fuzz(func(t *testing.T, out string) {
		perLink, global := ParseResolvectlDNS(out)
		if perLink == nil {
			t.Fatal("returned a nil map, which a caller cannot range over")
		}
		check := func(values []string, where string) {
			for _, v := range values {
				if v == "" || strings.ContainsAny(v, " \t\n") {
					t.Fatalf("%s value is not a bare word: %q", where, v)
				}
			}
		}
		check(global, "global")
		for name, values := range perLink {
			check(values, "link "+name)
		}
	})
}

// FuzzParseNetworkFile covers the one input that is a file rather than a
// command's output: the guided editor reads the settings back out of what this
// returns, so a setting has to say which section it came from and the raw text
// has to survive untouched.
func FuzzParseNetworkFile(f *testing.F) {
	f.Add("[Match]\nName=veth0\n\n[Network]\nDHCP=yes\n")
	f.Add("[Match]\n# a comment\n; another\nName = veth0 \n")
	f.Add("Name=veth0\n")
	f.Add("[]\n=\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		file := ParseNetworkFile("/etc/systemd/network/10-fuzz.network", raw)
		if file.Raw != raw {
			t.Fatal("the raw text the detail view shows is not the text that was read")
		}
		for _, s := range file.Settings {
			if strings.ContainsAny(s.Section, "\n") ||
				strings.ContainsAny(s.Key, "\n") ||
				strings.ContainsAny(s.Value, "\n") {
				t.Fatalf("setting spans lines: %+v", s)
			}
		}
		// MatchName is what the form edits, so it has to be the value the
		// file actually carries.
		want, ok := file.Get("Match", "Name")
		if !ok {
			want = ""
		}
		if file.MatchName != want {
			t.Fatalf("MatchName %q is not [Match] Name=%q", file.MatchName, want)
		}
	})
}
