package transporter

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// startSession starts cmd with its stdin and stdout wired to the returned
// Session; both providers' Exec use it. cmd must have been built with
// exec.CommandContext(ctx, ...) so that cancelling ctx kills it. Up to the
// last stderrTail bytes of its stderr are kept for errors: wrap turns a start
// or exit error, plus that tail, into the error returned here or by Wait.
//
// Stdout is an *os.File so os/exec starts no copy goroutine for it: the output
// sits in the pipe until read, even after the process has exited, and
// cmd.WaitDelay can't truncate it. Cancelling ctx unblocks Stdout readers with
// ctx's error. The pipe and its hook on ctx are released once Stdout is read
// to EOF (or an error) or ctx ends.
func startSession(ctx context.Context, cmd *exec.Cmd, wrap func(err error, stderr *tailBuffer) error) (Session, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	cr := &ctxReader{ctx: ctx, f: r}
	s := &session{stdin: stdin, stdout: cr, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = w, &s.stderr
	err = cmd.Start()
	w.Close() // the child holds its own copy
	if err != nil {
		r.Close()
		return nil, wrap(err, &s.stderr)
	}
	// Unblock a pending Read when ctx ends; ctxReader maps its error and
	// deregisters this once the reader is done.
	cr.stop = context.AfterFunc(ctx, func() { r.Close() })
	go func() {
		defer close(s.done)
		if err := cmd.Wait(); err != nil {
			s.err = wrap(err, &s.stderr)
		}
	}()
	return s, nil
}

type session struct {
	stdin  io.WriteCloser
	stdout *ctxReader
	stderr tailBuffer
	done   chan struct{}
	err    error // set before done is closed
}

func (s *session) Write(p []byte) (int, error) { return s.stdin.Write(p) }
func (s *session) Close() error                { return s.stdin.Close() }
func (s *session) Stdout() io.Reader           { return s.stdout }
func (s *session) Wait() error                 { <-s.done; return s.err }

// ctxReader reads f until ctx is done, after which Reads return ctx.Err().
// The first terminal error (EOF, a read error or ctx's error) is sticky: it
// closes f, deregisters stop from ctx, and is returned by every later Read.
// Like most readers it is not safe for concurrent Reads.
type ctxReader struct {
	ctx  context.Context
	f    *os.File
	stop func() bool // from context.AfterFunc, which closes f when ctx ends
	err  error
}

func (r *ctxReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if err := r.ctx.Err(); err != nil {
		return 0, r.fail(err)
	}
	n, err := r.f.Read(p)
	if err != nil {
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		return n, r.fail(err)
	}
	return n, nil
}

func (r *ctxReader) fail(err error) error {
	r.err = err
	r.stop()
	r.f.Close()
	return err
}

// tailBuffer is an io.Writer that keeps only the last stderrTail bytes.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - stderrTail; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}
