package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/tracker"
)

// --tracker: none at the default path is no tracker; a missing named file
// and a bad one are errors; a github file opens the GitHub adapter with
// the token its token_env names. Not parallel: it sets the token's
// variable.
func TestLoadTrackerFlag(t *testing.T) {
	home := t.TempDir()
	tr, err := loadTrackerFlag(home, "")
	require.NoError(t, err)
	require.Nil(t, tr)

	_, err = loadTrackerFlag(home, filepath.Join(home, "missing.yaml"))
	require.ErrorContains(t, err, "--tracker: ")

	bad := filepath.Join(home, "bad.yaml")
	require.NoError(t, os.WriteFile(bad, []byte("adapter: jira\n"), 0o600))
	_, err = loadTrackerFlag(home, bad)
	require.ErrorContains(t, err, `tracker: `+bad+`: unknown adapter "jira" (registered: github)`)

	t.Setenv("BMUS_TEST_TRACKER_TOKEN", "t0ken")
	data := "adapter: github\ngithub:\n  repo: o/r\n  token_env: BMUS_TEST_TRACKER_TOKEN\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, tracker.File), []byte(data), 0o600))
	tr, err = loadTrackerFlag(home, "")
	require.NoError(t, err)
	require.Equal(t, "github", tr.Name())
}
