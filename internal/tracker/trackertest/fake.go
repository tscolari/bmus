// Package trackertest is an in-memory tracker.Tracker for tests.
package trackertest

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/tscolari/bmus/internal/tracker"
)

// Comment is a comment the fake recorded, with the idempotency key it
// was posted under ("" for none).
type Comment struct {
	Ref, Body, Key string
}

// Fake is an in-memory tracker. Refs are "#<n>", numbered from 1 in
// creation order. Items with Deps unset (nil) report no dependency data.
// It honours a Mark's key (Since aside): a second Comment on a ref under
// a key already recorded there posts nothing, and a second Create under
// a key returns the item filed under it. Its zero value is ready to use;
// it is safe for concurrent use.
type Fake struct {
	mu       sync.Mutex
	items    []tracker.Item
	reasons  map[string]string
	comments []Comment
	created  map[string]int // Create's key to the item's index
	// NoDeps makes List and Get report no dependency data (BlockedBy nil).
	NoDeps bool
	// Err, if set, is returned by every call.
	Err error
}

var _ tracker.Tracker = (*Fake)(nil)

// Name is "fake".
func (f *Fake) Name() string { return "fake" }

// Canon is ref trimmed, which must be #<n>.
func (f *Fake) Canon(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if n, err := strconv.Atoi(strings.TrimPrefix(ref, "#")); err != nil || n < 1 || !strings.HasPrefix(ref, "#") {
		return "", fmt.Errorf("%w: %q is not #<n>", tracker.ErrBadRef, ref)
	}
	return ref, nil
}

// Add files it as Create would and returns its ref.
func (f *Fake) Add(it tracker.Item) string {
	got, err := f.Create(context.Background(), it, tracker.Mark{})
	if err != nil {
		panic(err)
	}
	return got.Ref
}

func (f *Fake) index(ref string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(ref, "#"))
	if err != nil || !strings.HasPrefix(ref, "#") {
		return 0, fmt.Errorf("%w: %q is not #<n>", tracker.ErrBadRef, ref)
	}
	if n < 1 || n > len(f.items) {
		return 0, fmt.Errorf("item %s: %w", ref, tracker.ErrNotFound)
	}
	return n - 1, nil
}

// view is items[i] with its open blockers resolved.
func (f *Fake) view(i int) tracker.Item {
	it := f.items[i]
	it.Labels = slices.Clone(it.Labels)
	if f.NoDeps {
		it.BlockedBy = nil
		return it
	}
	open := []string{}
	for _, b := range it.BlockedBy {
		if j, err := f.index(b); err == nil && f.items[j].State == tracker.StateOpen {
			open = append(open, b)
		}
	}
	it.BlockedBy = open
	return it
}

func (f *Fake) Get(_ context.Context, ref string) (tracker.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return tracker.Item{}, f.Err
	}
	i, err := f.index(ref)
	if err != nil {
		return tracker.Item{}, err
	}
	return f.view(i), nil
}

func (f *Fake) List(_ context.Context, q tracker.Query) ([]tracker.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	var out []tracker.Item
	for i := range f.items {
		it := f.view(i)
		if (q.Parent == "" || it.Parent == q.Parent) && tracker.Fits(it, q) {
			out = append(out, it)
		}
	}
	return out, nil
}

func (f *Fake) Create(_ context.Context, it tracker.Item, m tracker.Mark) (tracker.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return tracker.Item{}, f.Err
	}
	if err := m.Check(); err != nil {
		return tracker.Item{}, err
	}
	if i, ok := f.created[m.Key]; ok && m.Key != "" {
		return f.view(i), nil
	}
	for _, r := range append(slices.Clone(it.BlockedBy), it.Parent) {
		if r == "" {
			continue
		}
		if _, err := f.index(r); err != nil {
			return tracker.Item{}, err
		}
	}
	it.Ref = "#" + strconv.Itoa(len(f.items)+1)
	it.State = tracker.StateOpen
	it.Labels = slices.Clone(it.Labels)
	it.BlockedBy = slices.Clone(it.BlockedBy)
	f.items = append(f.items, it)
	if m.Key != "" {
		if f.created == nil {
			f.created = map[string]int{}
		}
		f.created[m.Key] = len(f.items) - 1
	}
	return f.view(len(f.items) - 1), nil
}

func (f *Fake) Comment(_ context.Context, ref, body string, m tracker.Mark) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	if err := m.Check(); err != nil {
		return err
	}
	if _, err := f.index(ref); err != nil {
		return err
	}
	if m.Key != "" && slices.ContainsFunc(f.comments, func(c Comment) bool { return c.Ref == ref && c.Key == m.Key }) {
		return nil
	}
	f.comments = append(f.comments, Comment{Ref: ref, Body: body, Key: m.Key})
	return nil
}

func (f *Fake) Close(_ context.Context, ref, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	i, err := f.index(ref)
	if err != nil {
		return err
	}
	if !tracker.CheckReason(reason) {
		return fmt.Errorf("unknown close reason %q", reason)
	}
	if reason == "" {
		reason = tracker.ReasonCompleted
	}
	f.items[i].State = tracker.StateClosed
	if f.reasons == nil {
		f.reasons = map[string]string{}
	}
	f.reasons[ref] = reason
	return nil
}

// Comments returns the comments recorded so far.
func (f *Fake) Comments() []Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.comments)
}

// CloseReason is the reason ref was closed with, "" while open.
func (f *Fake) CloseReason(ref string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reasons[ref]
}
