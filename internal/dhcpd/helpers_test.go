package dhcpd

import (
	"os"
	"path/filepath"
	"testing"
)

// mustReadFixture reads a testdata file or fails the test. The fixtures are
// constructed, not captured, and every address in them is in a documentation
// range — see fixtures_test.go, which enforces that.
func mustReadFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // the name is a literal in the tests
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(raw)
}

// mustReadFixtureF is mustReadFixture for a fuzz seed.
func mustReadFixtureF(f *testing.F, name string) string {
	f.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // the name is a literal in the tests
	if err != nil {
		f.Fatalf("read fixture %s: %v", name, err)
	}
	return string(raw)
}
