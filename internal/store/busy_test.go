package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// holdWriteLock opens path apart from the store and takes SQLite's write
// lock on it with BEGIN IMMEDIATE. The returned func commits, releasing it.
func holdWriteLock(t *testing.T, path string) func() {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	_, err = conn.ExecContext(t.Context(), "BEGIN IMMEDIATE")
	require.NoError(t, err)
	return func() {
		_, err := conn.ExecContext(context.Background(), "COMMIT")
		require.NoError(t, err)
	}
}

// openBusy opens a store whose busy_timeout is 1ms, so a held write lock
// outlasts it at once.
func openBusy(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), DBFile)
	s, err := open(t.Context(), path, 1)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s, path
}

// A write whose BEGIN gets SQLITE_BUSY past busy_timeout retries, and
// succeeds once the lock holder commits (#204).
func TestInsertOrderRetriesBusy(t *testing.T) {
	t.Parallel()
	s, path := openBusy(t)
	busy := make(chan struct{}, 1)
	s.onBusy = func() {
		select {
		case busy <- struct{}{}:
		default:
		}
	}
	release := holdWriteLock(t, path)

	type result struct {
		id  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		id, err := s.InsertOrder(t.Context(), Order{Title: "under load"}, testEntry)
		done <- result{id, err}
	}()
	var r result
	select {
	case <-busy:
		release()
		r = <-done
	case r = <-done:
		release()
		require.Fail(t, "the write returned without retrying", "err = %v", r.err)
	}
	require.NoError(t, r.err)
	o, err := s.GetOrder(t.Context(), r.id)
	require.NoError(t, err)
	require.Equal(t, "under load", o.Title)
}

// The retry is bounded: past busyRetry, or once ctx ends, the write fails
// with SQLITE_BUSY, wrapping ctx.Err() in the second case, and creates
// nothing.
func TestInsertOrderBusyGivesUp(t *testing.T) {
	t.Parallel()
	t.Run("deadline", func(t *testing.T) {
		t.Parallel()
		s, path := openBusy(t)
		s.busyRetry = 0
		retried := false
		s.onBusy = func() { retried = true }
		release := holdWriteLock(t, path)
		_, err := s.InsertOrder(t.Context(), Order{Title: "too late"}, testEntry)
		require.True(t, isBusy(err), "err = %v", err)
		require.False(t, retried)
		release()
		all, err := s.ListOrders(t.Context())
		require.NoError(t, err)
		require.Empty(t, all)
	})
	t.Run("context", func(t *testing.T) {
		t.Parallel()
		s, path := openBusy(t)
		ctx, cancel := context.WithCancel(t.Context())
		s.onBusy = cancel
		release := holdWriteLock(t, path)
		_, err := s.InsertOrder(ctx, Order{Title: "cancelled"}, testEntry)
		require.True(t, isBusy(err), "err = %v", err)
		require.ErrorIs(t, err, context.Canceled)
		release()
		all, err := s.ListOrders(t.Context())
		require.NoError(t, err)
		require.Empty(t, all)
	})
}

// The store's writers queue for the writeSem slot, so they never wait on each other
// in SQLite's busy handler: with a 1ms busy_timeout and no retry,
// concurrent writes all succeed (#204).
func TestConcurrentWritesQueue(t *testing.T) {
	t.Parallel()
	s, _ := openBusy(t)
	s.busyRetry = 0
	const writers, each = 8, 20
	errs := make(chan error, writers*each)
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				_, err := s.InsertOrder(t.Context(), Order{Title: "queued"}, testEntry)
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	all, err := s.ListOrders(t.Context())
	require.NoError(t, err)
	require.Len(t, all, writers*each)
}

// A writer queued for the write slot leaves with ctx.Err() once its ctx
// ends, having written nothing.
func TestQueuedWriteHonoursContext(t *testing.T) {
	t.Parallel()
	s, _ := openBusy(t)
	s.writeSem <- struct{}{} // another writer holds the slot
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := s.InsertOrder(ctx, Order{Title: "queued"}, testEntry)
		done <- err
	}()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	<-s.writeSem
	all, err := s.ListOrders(t.Context())
	require.NoError(t, err)
	require.Empty(t, all)
}
