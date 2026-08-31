package dhcpd

import (
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The documentation ranges a fixture is allowed to use: RFC 5737 for IPv4,
// RFC 3849 for IPv6, RFC 7042 for MACs. A fixture that carries anything else is
// either captured from a real host without scrubbing, or invented from a real
// address — both of which this test exists to stop reaching the repository.
var (
	ipv4DocPrefixes = []string{"192.0.2.", "198.51.100.", "203.0.113."}
	macDocPrefix    = "00:00:5e:00:53:"
)

// tokenSplitters break text into candidate tokens for each kind of value: only
// the characters that can appear inside one are kept together.
var (
	ipv4Chars = regexp.MustCompile(`[^0-9.]+`)
	hexChars  = regexp.MustCompile(`[^0-9a-fA-F:]+`)
	ipv4Token = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
	macToken  = regexp.MustCompile(`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`)
)

// TestFixturesUseDocumentationValues is the scrub promise as a test: every
// address and MAC in every testdata file is a documentation value. It scans the
// bytes rather than parsing, so it also covers a fixture a parser happens to
// ignore.
func TestFixturesUseDocumentationValues(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("testdata", entry.Name())) //nolint:gosec // testdata is in the repository
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		checkFixtureValues(t, entry.Name(), string(raw))
	}
}

func checkFixtureValues(t *testing.T, name, text string) {
	t.Helper()

	// IPv4 addresses (and netmasks) must be documentation values.
	for _, token := range ipv4Chars.Split(text, -1) {
		if !ipv4Token.MatchString(token) {
			continue
		}
		addr, err := netip.ParseAddr(token)
		if err != nil || !addr.Is4() {
			continue
		}
		if !allowedIPv4(token) {
			t.Errorf("%s: IPv4 %q is not a documentation address (RFC 5737)", name, token)
		}
	}

	for _, token := range hexChars.Split(text, -1) {
		switch {
		case macToken.MatchString(token):
			if !strings.HasPrefix(strings.ToLower(token), macDocPrefix) {
				t.Errorf("%s: MAC %q is not a documentation address (RFC 7042 00:00:5e:00:53:xx)",
					name, token)
			}
		case strings.Contains(token, "::"):
			// A real IPv6 address in a fixture always carries "::"; a DUID or
			// client id (colon-separated hex without "::") is an identifier, not
			// an address, and is not checked here.
			if addr, err := netip.ParseAddr(token); err == nil && addr.Is6() {
				if !allowedIPv6(token) {
					t.Errorf("%s: IPv6 %q is not a documentation address (RFC 3849 2001:db8::/32)",
						name, token)
				}
			}
		}
	}
}

// allowedIPv4 reports whether an IPv4 literal is a documentation address, a
// netmask, or the unspecified address.
func allowedIPv4(token string) bool {
	if token == "0.0.0.0" || netmaskRe.MatchString(token) {
		return true
	}
	for _, prefix := range ipv4DocPrefixes {
		if strings.HasPrefix(token, prefix) {
			return true
		}
	}
	return false
}

// allowedIPv6 reports whether an IPv6 literal is a documentation address, the
// loopback, or a link-local address.
func allowedIPv6(token string) bool {
	lower := strings.ToLower(token)
	return strings.HasPrefix(lower, "2001:db8") ||
		strings.HasPrefix(lower, "fe80:") || lower == "::1" || lower == "::"
}
