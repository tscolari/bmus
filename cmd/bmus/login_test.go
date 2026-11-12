package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #102's acceptance through the CLI: `bmus bridge run --transporter tsh`
// whose tsh (a fake on PATH) reports a login valid for 10 more minutes is
// yellow in `bmus status`, and restarted with the login expired it says so.
// With no crew, no tsh beams call is made.
func TestStatusShowsTshLogin(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and runs the bmus binary")
	}
	bin, err := buildBinary()
	require.NoError(t, err)
	base := os.TempDir()
	if len(base) > 20 {
		base = "/tmp"
	}
	home, err := os.MkdirTemp(base, "bmus")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	c := cli{bin: bin, home: home}

	fake := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = status ]; then . '` + fake + `/status.sh'; exit; fi
echo "$*" >> '` + fake + `/beams.log'; exit 1
`
	require.NoError(t, os.WriteFile(filepath.Join(fake, "tsh"), []byte(script), 0o755))
	bundle := filepath.Join(fake, "combadge.mjs") // never uploaded: there is no crew
	require.NoError(t, os.WriteFile(bundle, nil, 0o644))
	logFile := filepath.Join(t.TempDir(), "bridge.log")
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := os.ReadFile(logFile)
			t.Logf("bridge log:\n%s", out)
		}
	})

	statusFor := func(until time.Time, want string) {
		t.Helper()
		s := `echo '{"active":{"username":"captain","valid_until":"` + until.UTC().Format(time.RFC3339) + `"},"profiles":[]}'`
		if !until.After(time.Now()) {
			s += "; echo 'ERROR: Active profile expired.' >&2; exit 1"
		}
		require.NoError(t, os.WriteFile(filepath.Join(fake, "status.sh"), []byte(s+"\n"), 0o644))
		lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		defer lf.Close()
		cmd := c.cmd(context.Background(), "bridge", "run", "--transporter", "tsh", "--combadge", bundle)
		cmd.Env = append(cmd.Env, "PATH="+fake+string(os.PathListSeparator)+os.Getenv("PATH"))
		cmd.Stdout, cmd.Stderr = lf, lf
		require.NoError(t, cmd.Start())
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		defer func() {
			require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
			select {
			case err := <-exited:
				require.NoError(t, err, "bridge exit")
			case <-time.After(90 * time.Second):
				cmd.Process.Kill()
				t.Fatal("bridge did not exit after SIGTERM")
			}
		}()
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			require.Contains(ct, c.run(ct, "status"), want)
		}, 20*time.Second, 100*time.Millisecond)
	}

	statusFor(time.Now().Add(10*time.Minute), "yellow (tsh login expires in 10m)")
	statusFor(time.Now().Add(-time.Minute), "yellow (tsh login expired)")
	_, err = os.Stat(filepath.Join(fake, "beams.log"))
	require.ErrorIs(t, err, os.ErrNotExist, "a tsh beams call was made")
}
