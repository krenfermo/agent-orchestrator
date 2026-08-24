package cli

import (
	"os"
	"strings"
	"testing"
)

// TestMain scrubs every ambient AO_* variable before the package's tests run.
//
// The CLI reads its own environment directly and on purpose -- `ao browser`
// refuses to run outside a session unless AO_SESSION_ID is set (browser.go),
// `ao spawn` resolves a project from AO_PROJECT_ID/AO_SESSION_ID (spawn.go),
// and the hook commands thread AO_RUNTIME_LAUNCH_ID onto every activity report
// (hooks.go). That is correct behavior and none of it is under test here; what
// is under test is what the CLI does for a GIVEN environment, which each test
// establishes with t.Setenv.
//
// Without this, "given" silently means "given, plus whatever the shell already
// had". Running the suite from inside a real AO session -- a developer with the
// desktop app open, or an AO worker session running `go test ./...` -- leaks a
// live AO_RUNTIME_LAUNCH_ID into every hook assertion that expects none, an
// AO_PROJECT_ID into every spawn test that expects project resolution to fail,
// and an AO_SESSION_ID into the browser test that expects the
// not-inside-a-session refusal. The tests were never wrong; they were reading
// an input nobody had set.
//
// Scrubbing here rather than in a helper is deliberate: it runs once, before
// any test, so a test's own t.Setenv still wins and is still restored
// afterwards. A helper could only clear variables at the point it is called,
// which would clobber the several tests that set AO_RUNTIME_LAUNCH_ID
// explicitly before calling it.
func TestMain(m *testing.M) {
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(name, "AO_") {
			_ = os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}
