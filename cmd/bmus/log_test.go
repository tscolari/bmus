package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/bridge"
)

// fakeBridge answers log.tail on <home>/bridge.sock: no reply while down
// (a connection lost mid-request), unavailable while busy, else one entry
// after since_id 0 and none after it.
type fakeBridge struct {
	down, busy       atomic.Bool
	dropped, refused atomic.Int32
}

func (f *fakeBridge) serve(t *testing.T, home string) {
	ln, err := net.Listen("unix", filepath.Join(home, bridge.SocketFile))
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var req bridge.Request
				var args bridge.LogTailArgs
				if json.Unmarshal(line, &req) != nil || json.Unmarshal(req.Args, &args) != nil {
					return
				}
				if f.down.Load() {
					f.dropped.Add(1)
					return
				}
				resp := bridge.Response{OK: true}
				if f.busy.Load() {
					f.refused.Add(1)
					resp = bridge.Response{Error: "bridge busy", Code: bridge.CodeUnavailable}
				} else {
					res := bridge.LogTailResult{Entries: []bridge.LogEntryView{}, LastID: args.SinceID}
					if args.SinceID == 0 {
						res.Entries = append(res.Entries, bridge.LogEntryView{ID: 1, Kind: "order", Text: "after the rush"})
						res.LastID = 1
					}
					resp.Result, _ = json.Marshal(res)
				}
				b, _ := json.Marshal(resp)
				conn.Write(append(b, '\n'))
			}()
		}
	}()
}

// `log tail --follow` keeps running while the bridge answers unavailable,
// and resumes once it answers again (#40).
func TestFollowWaitsWhileBridgeBusy(t *testing.T) {
	t.Parallel()
	home, err := os.MkdirTemp("", "bmus")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	f := &fakeBridge{}
	f.busy.Store(true)
	f.serve(t, home)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out, errOut syncBuffer
	done := make(chan error, 1)
	go func() { done <- tailLog(ctx, &app{home: home}, &out, &errOut, 0, true, true) }()

	require.Eventually(t, func() bool { return f.refused.Load() >= 2 }, 10*time.Second, 20*time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("log tail --follow exited while the bridge was busy: %v", err)
	default:
	}
	require.Contains(t, errOut.String(), "bridge busy; retrying")
	require.Empty(t, out.String())

	f.busy.Store(false)
	require.Eventually(t, func() bool { return out.String() != "" }, 10*time.Second, 20*time.Millisecond)
	var e bridge.LogEntryView
	require.NoError(t, json.Unmarshal([]byte(out.String()), &e))
	require.Equal(t, "after the rush", e.Text)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("log tail --follow did not stop when interrupted")
	}
}

// Busy and down reset each other: down, busy, down notes the bridge is down
// twice.
func TestFollowBusyAndDownResetEachOther(t *testing.T) {
	t.Parallel()
	home, err := os.MkdirTemp("", "bmus")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	f := &fakeBridge{}
	f.serve(t, home)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out, errOut syncBuffer
	done := make(chan error, 1)
	go func() { done <- tailLog(ctx, &app{home: home}, &out, &errOut, 0, true, true) }()
	// Down only counts once the follower has reached the bridge.
	require.Eventually(t, func() bool { return out.String() != "" }, 10*time.Second, 20*time.Millisecond)

	const downNote, busyNote = "bridge not running; waiting for it", "bridge busy; retrying"
	f.down.Store(true)
	require.Eventually(t, func() bool { return f.dropped.Load() >= 1 }, 10*time.Second, 20*time.Millisecond)
	f.busy.Store(true)
	f.down.Store(false)
	require.Eventually(t, func() bool { return f.refused.Load() >= 1 }, 10*time.Second, 20*time.Millisecond)
	f.down.Store(true)
	require.Eventually(t, func() bool { return strings.Count(errOut.String(), downNote) == 2 },
		10*time.Second, 20*time.Millisecond, "stderr: %s", errOut.String())
	require.Equal(t, 1, strings.Count(errOut.String(), busyNote), errOut.String())

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("log tail --follow did not stop when interrupted")
	}
}
