package bridge

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Harnesses must be exactly the names combadge serve accepts (#64): serve's
// unknown-harness error lists them.
func TestHarnessesMatchServe(t *testing.T) {
	t.Parallel()
	bundle := combadgeBundle(t)
	out, err := exec.Command("node", bundle, "serve", "--socket", "unused.sock", "--crew", "x",
		"--outbox", t.TempDir()+"/outbox.ndjson", "--harness", "nope").CombinedOutput()
	require.Error(t, err, "serve accepted --harness nope: %s", out)
	m := regexp.MustCompile(`unknown harness nope \(available: ([^)]*)\)`).FindSubmatch(out)
	require.NotNil(t, m, "no available list in serve's output: %s", out)
	require.ElementsMatch(t, strings.Split(string(m[1]), ", "), Harnesses)
	for _, h := range DefaultHarnesses {
		require.Contains(t, Harnesses, h)
	}
}
