package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// ErrLocked is returned by AcquireLock when another process holds the lock.
var ErrLocked = errors.New("bridge lock is held by another process")

// Lock is a held process lock. Keep it reachable until Close: it owns the
// open file the flock lives on.
type Lock struct {
	f *os.File
}

// AcquireLock takes an exclusive, non-blocking flock on path (normally
// ~/.bmus/bridge.lock), creating the file if needed, and writes the current
// PID into it, so only one bridge daemon writes to the database at a time
// (D1). If the lock is already held it returns an error wrapping ErrLocked
// that names the holder's PID when known. The lock is released by Close or
// when the process exits.
func AcquireLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if b, rerr := os.ReadFile(path); rerr == nil {
				if pid, perr := strconv.Atoi(string(bytes.TrimSpace(b))); perr == nil {
					return nil, fmt.Errorf("%s (pid %d): %w", path, pid, ErrLocked)
				}
			}
			return nil, fmt.Errorf("%s: %w", path, ErrLocked)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	err = f.Truncate(0)
	if err == nil {
		_, err = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("write pid to %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Close releases the lock.
func (l *Lock) Close() error {
	return errors.Join(syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN), l.f.Close())
}
