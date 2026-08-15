package generate_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// sharedModuleCache is the isolated Go module cache every end-to-end test
// resolves through. The fixture publishes modules at fixed versions through a
// fixture proxy, and a module cache is keyed by path and version alone: a
// developer's cache that already held one of those versions from an earlier
// fixture would serve that instead. Sharing one isolated cache across the package
// preserves correctness without repeatedly resolving the same tiny modules.
var sharedModuleCache string

// TestMain owns the shared module cache for the whole package.
//
// The removal is its own function rather than a defer because os.Exit does not
// run defers, and a module cache left behind is a directory the go command made
// read-only that nothing else will clean up.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "soapbox-generate")
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate tests: cache root:", err)
		os.Exit(1) //nolint:forbidigo // TestMain must report setup failure to the test process
	}
	sharedModuleCache = filepath.Join(root, "mod")

	code := m.Run()

	if err := removeAllForced(root); err != nil {
		fmt.Fprintln(os.Stderr, "generate tests: cache cleanup:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code) //nolint:forbidigo // TestMain must return the test result to the process
}

// moduleCache reports the shared module cache, failing a test that somehow ran
// without TestMain having prepared one.
func moduleCache(t *testing.T) string {
	t.Helper()
	if sharedModuleCache == "" {
		t.Fatal("module cache: TestMain did not prepare one")
	}
	return sharedModuleCache
}
