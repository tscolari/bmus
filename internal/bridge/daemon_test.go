package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
)

// running is a daemon started by startDaemon.
type running struct {
	d      *Daemon
	cancel context.CancelFunc
	done   <-chan error
}

// stop cancels the daemon and requires Run to return nil.
func (r running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func testConfig(home string) Config {
	return Config{
		Home:           home,
		Transporter:    &fakeProvider{}, // provisions fake beams; pumps attach to silent sessions
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		queuePollEvery: 20 * time.Millisecond,
		NoConsole:      true,
	}
}

// privateHome returns a new 0700 directory for a bridge home: t.TempDir's
// are 0755, which Run refuses.
func privateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, os.Chmod(home, 0o700))
	return home
}

// startDaemon runs the daemon in a goroutine and waits until it is up.
func startDaemon(t *testing.T, home string) running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan *Daemon, 1)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": testRepo(t).url}
	cfg.onReady = func(d *Daemon) { ready <- d }
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	select {
	case d := <-ready:
		return running{d: d, cancel: cancel, done: done}
	case err := <-done:
		cancel()
		t.Fatalf("Run returned before ready: %v", err)
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("daemon not ready")
	}
	return running{}
}

func insertCrew(t *testing.T, home string, c store.Crew) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(home, store.DBFile))
	require.NoError(t, err)
	defer st.Close()
	require.NoError(t, st.InsertCrew(t.Context(), c, testEntry))
}

// insertOrder inserts o into the store under home, as insertCrew does, and
// returns its id.
func insertOrder(t *testing.T, home string, o store.Order) string {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(home, store.DBFile))
	require.NoError(t, err)
	defer st.Close()
	id, err := st.InsertOrder(t.Context(), o, testEntry)
	require.NoError(t, err)
	return id
}

// workflows lists the workflows with the given id, regardless of status.
func workflows(t require.TestingT, d *Daemon, id string) []dbos.WorkflowStatus {
	wfs, err := dbos.ListWorkflows(d.dbos,
		dbos.WithFilterWorkflowIDs(id),
		dbos.WithFilterLoadInput(false),
		dbos.WithFilterLoadOutput(false))
	require.NoError(t, err)
	return wfs
}

// requirePending waits until exactly one workflow with id exists and is
// PENDING, and returns it. After a restart DBOS moves it through ENQUEUED
// (recovery puts it on its internal queue) before it runs again.
func requirePending(t *testing.T, d *Daemon, id string) dbos.WorkflowStatus {
	t.Helper()
	var wf dbos.WorkflowStatus
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		wfs := workflows(c, d, id)
		require.Len(c, wfs, 1)
		require.Equal(c, dbos.WorkflowStatusPending, wfs[0].Status)
		wf = wfs[0]
	}, 10*time.Second, 50*time.Millisecond)
	return wf
}

// dismiss sends the tour a dismissed signal and requires it to end with that.
func dismiss(t *testing.T, d *Daemon, tourID string) {
	t.Helper()
	require.NoError(t, dbos.Send(d.dbos, tourID, CrewSignal{Kind: SignalDismissed}, crewEventsTopic))
	h, err := dbos.RetrieveWorkflow[CrewTourResult](d.dbos, tourID)
	require.NoError(t, err)
	res, err := h.GetResult(dbos.WithHandleTimeout(10 * time.Second))
	require.NoError(t, err)
	require.Equal(t, CrewTourResult{Ended: SignalDismissed}, res)
}

func TestTourSurvivesRestart(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	insertCrew(t, home, store.Crew{ID: "gone", Name: "Gone", Status: store.CrewDismissed})
	tourID := tourWorkflowID("sorell", 0)
	require.Equal(t, "crew/sorell/tour/0", tourID)

	r := startDaemon(t, home)
	wf := requirePending(t, r.d, tourID)
	require.Equal(t, workflowVersion, wf.ApplicationVersion)
	require.Equal(t, crewTourWorkflowName, wf.Name)
	require.Equal(t, 1, wf.Attempts)
	require.Empty(t, workflows(t, r.d, tourWorkflowID("gone", 0)), "dismissed crew get no tour")
	r.stop(t)

	// Restart against the same bridge.db: Launch resumes the PENDING tour
	// (a second attempt, so it really ran again) and the re-attach finds it,
	// so there is still exactly one (next to its transporter workflow).
	r = startDaemon(t, home)
	wf = requirePending(t, r.d, tourID)
	require.Equal(t, 2, wf.Attempts)
	all, err := dbos.ListWorkflows(r.d.dbos, dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	require.NoError(t, err)
	var ids []string
	for _, w := range all {
		ids = append(ids, w.ID)
	}
	require.ElementsMatch(t, []string{tourID, beamWorkflowID("sorell", 0)}, ids)

	// The resumed tour is live: it receives signals and ends on dismissal.
	dismiss(t, r.d, tourID)
	r.stop(t)

	// A finished tour isn't run again.
	r = startDaemon(t, home)
	wfs := workflows(t, r.d, tourID)
	require.Len(t, wfs, 1)
	require.Equal(t, dbos.WorkflowStatusSuccess, wfs[0].Status)
	r.stop(t)
}

// TestTourRecoversPastAttemptLimit checks tours are registered without DBOS's
// recovery-attempt limit: every bridge restart is an attempt, and a tour lives
// as long as its crew member.
func TestTourRecoversPastAttemptLimit(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	tourID := tourWorkflowID("sorell", 0)

	r := startDaemon(t, home)
	requirePending(t, r.d, tourID)
	r.stop(t)

	db, err := sql.Open("sqlite", filepath.Join(home, store.DBFile)+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE workflow_status SET recovery_attempts = 500 WHERE workflow_uuid = ?`, tourID)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	r = startDaemon(t, home)
	wf := requirePending(t, r.d, tourID)
	require.Equal(t, 501, wf.Attempts)
	dismiss(t, r.d, tourID)
	r.stop(t)
}

func TestSpecialCharsInHome(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "we?ird%41 #home")
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	r := startDaemon(t, home)
	requirePending(t, r.d, tourWorkflowID("sorell", 0))
	r.stop(t)
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	require.Contains(t, names, store.DBFile)
	parent, err := os.ReadDir(filepath.Dir(home))
	require.NoError(t, err)
	require.Len(t, parent, 1, "nothing created at a truncated path")
}

func TestSecondDaemonIsLockedOut(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	r := startDaemon(t, home)
	defer r.stop(t)

	err := Run(t.Context(), testConfig(home))
	require.ErrorIs(t, err, store.ErrLocked)
}

func TestFailedStartReleasesLock(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	// A directory where bridge.db should be makes Run fail after locking.
	require.NoError(t, os.Mkdir(filepath.Join(home, store.DBFile), 0o700))
	err := Run(t.Context(), testConfig(home))
	require.Error(t, err)
	require.NotErrorIs(t, err, store.ErrLocked)

	lock, err := store.AcquireLock(filepath.Join(home, store.LockFile))
	require.NoError(t, err)
	require.NoError(t, lock.Close())
}

// Helper process for TestTourSurvivesSIGKILL: runs the daemon on
// $BMUS_TEST_DAEMON_HOME until killed. With BMUS_TEST_DISMISS=1 it instead
// dismisses sorell's tour once up, prints the outcome and exits.
func TestHelperDaemon(t *testing.T) {
	t.Parallel()
	home := os.Getenv("BMUS_TEST_DAEMON_HOME")
	if home == "" {
		t.Skip("helper process for TestTourSurvivesSIGKILL")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := testConfig(home)
	cfg.onReady = func(d *Daemon) {
		if os.Getenv("BMUS_TEST_DISMISS") != "1" {
			return
		}
		go func() {
			defer cancel()
			id := tourWorkflowID("sorell", 0)
			if err := dbos.Send(d.dbos, id, CrewSignal{Kind: SignalDismissed}, crewEventsTopic); err != nil {
				fmt.Println("send:", err)
				return
			}
			h, err := dbos.RetrieveWorkflow[CrewTourResult](d.dbos, id)
			if err != nil {
				fmt.Println("retrieve:", err)
				return
			}
			res, err := h.GetResult(dbos.WithHandleTimeout(10 * time.Second))
			fmt.Printf("RESULT %+v %v\n", res, err)
		}()
	}
	require.NoError(t, Run(ctx, cfg))
}

// tourRow reads the tour's status straight from bridge.db, since no daemon
// is up to ask while the test kills and restarts them.
func tourRow(t require.TestingT, dbPath, id string) (status string, attempts int) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	defer db.Close()
	err = db.QueryRow(`SELECT status, recovery_attempts FROM workflow_status WHERE workflow_uuid = ?`, id).Scan(&status, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0
	}
	require.NoError(t, err)
	return status, attempts
}

// helperOutput is a helper process's combined output file; it reads the file
// only when printed, so failure messages show what the helper wrote by then.
type helperOutput string

func (o helperOutput) String() string {
	b, _ := os.ReadFile(string(o))
	return string(b)
}

func helperDaemon(t *testing.T, home string, dismiss bool) (*exec.Cmd, helperOutput) {
	t.Helper()
	// The helper doesn't inherit go test's -timeout; bound it so a hung helper
	// fails the test instead of stalling it.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHelperDaemon$", "-test.v", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "BMUS_TEST_DAEMON_HOME="+home)
	if dismiss {
		cmd.Env = append(cmd.Env, "BMUS_TEST_DISMISS=1")
	}
	f, err := os.CreateTemp(t.TempDir(), "helper-out")
	require.NoError(t, err)
	defer f.Close()
	cmd.Stdout, cmd.Stderr = f, f
	require.NoError(t, cmd.Start())
	return cmd, helperOutput(f.Name())
}

// TestTourSurvivesSIGKILL is the Phase 1 "kill the bridge, restart it" exit
// criterion at the DBOS layer: a daemon killed with no chance to shut down
// leaves the tour PENDING, and the next daemon resumes it.
func TestTourSurvivesSIGKILL(t *testing.T) {
	t.Parallel()
	if os.Getenv("BMUS_TEST_DAEMON_HOME") != "" {
		t.Skip("running as helper")
	}
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	dbPath := filepath.Join(home, store.DBFile)
	tourID := tourWorkflowID("sorell", 0)

	cmd, out := helperDaemon(t, home, false)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		status, _ := tourRow(c, dbPath, tourID)
		require.Equal(c, "PENDING", status)
	}, 20*time.Second, 50*time.Millisecond, "helper output: %s", out)
	require.NoError(t, cmd.Process.Signal(syscall.SIGKILL))
	_ = cmd.Wait()
	status, attempts := tourRow(t, dbPath, tourID)
	require.Equal(t, "PENDING", status)
	require.Equal(t, 1, attempts)

	cmd, out = helperDaemon(t, home, true)
	require.NoError(t, cmd.Wait(), "helper output: %s", out)
	require.Contains(t, out.String(), "RESULT {Ended:dismissed} <nil>")
	status, attempts = tourRow(t, dbPath, tourID)
	require.Equal(t, "SUCCESS", status)
	require.Equal(t, 2, attempts, "resumed by recovery, not started afresh")
}
