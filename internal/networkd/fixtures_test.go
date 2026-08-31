package networkd

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docV4 are the ranges a fixture IPv4 address is allowed to fall in: the two
// documentation ranges the whole family scrubs to (RFC 5737), plus the third
// (203.0.113.0/24) a route-get destination uses, loopback, link-local, and the
// unspecified/all-zeros address a "default" route or a mask can carry.
var docV4 = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("0.0.0.0/8"),
}

// docV6 are the ranges a fixture IPv6 address is allowed in: the documentation
// prefix (RFC 3849), link-local, loopback/unspecified and multicast.
var docV6 = []netip.Prefix{
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("ff00::/8"),
}

// TestFixturesCarryNoRealAddress is the scrub promise as a test. The fixtures
// are real command output captured from machines running systemd-networkd and
// iproute2, so a captured address is a real address until it is rewritten — and
// a fixture that shipped one would leak the machine it came from into a public
// repository. Every IP literal in testdata must be in a documentation or local
// range; anything else is a capture that was not scrubbed.
func TestFixturesCarryNoRealAddress(t *testing.T) {
	err := filepath.WalkDir("testdata", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// The fuzz corpus stores opaque inputs, not captures to scrub.
		if strings.Contains(path, string(filepath.Separator)+"fuzz"+string(filepath.Separator)) {
			return nil
		}
		raw, readErr := os.ReadFile(path) //nolint:gosec // walking the repository's own testdata
		if readErr != nil {
			return readErr
		}
		for _, token := range addressTokens(string(raw)) {
			addr, parseErr := netip.ParseAddr(token)
			if parseErr != nil {
				continue
			}
			if !allowed(addr) {
				t.Errorf("%s carries a non-documentation address %q — scrub it to "+
					"192.0.2.0/24 or 198.51.100.0/24", path, token)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking testdata: %v", err)
	}
}

// addressTokens splits a file into the substrings that could be an IP address:
// runs of hex digits, dots and colons. A MAC or a timestamp lands here too, but
// netip rejects those, so only real addresses reach the range check.
func addressTokens(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
			return false
		case r == '.' || r == ':':
			return false
		default:
			return true
		}
	})
	return fields
}

// allowed reports whether an address is in one of the documentation or local
// ranges a fixture may carry.
func allowed(addr netip.Addr) bool {
	ranges := docV4
	if addr.Is6() {
		ranges = docV6
	}
	for _, prefix := range ranges {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
