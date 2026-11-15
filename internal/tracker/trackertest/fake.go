// Package trackertest is an in-memory tracker.Tracker for tests.
package trackertest

import (
	"context"
	"errors"
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
// creation order. It keeps each item's labels as Create gave them, less
// those Unlabel removed, and CloseReason reports how each closed item was
// closed. NoDeps makes it report no dependency data, and DropLabels makes
// Create drop labels as GitHub does for a token without push access. List
// takes items newest first, as the GitHub adapter does, or in creation
// order under a Parent (a parent's order), and stops at a Query's Limit.
// It honours a Mark's key (Since aside): a second Comment on a ref under
// a key already recorded there posts nothing, and a second Create under
// a key returns the item filed under it, adding the blocker links it
// lacks, as the GitHub adapter does. FailLinks makes Create fail a link. Its zero value is ready to use;
// it is safe for concurrent use.
type Fake struct {
	mu       sync.Mutex
	items    []tracker.Item
	reasons  map[string]string
	comments []Comment
	created  map[string]int // Create's key to the item's index
	// NoDeps makes List and Get report no dependency data (BlockedBy nil).
	NoDeps bool
	// DropLabels makes Create store an item without its labels and
	// return it with tracker.ErrLabels; a retry under the same Mark
	// returns the same.
	DropLabels bool
	// FailLinks is how many more Creates with blockers fail their last
	// blocker link: the item is filed with the others and returned with
	// an error, as the GitHub adapter returns a link that failed after
	// the issue was created. A retry under the same Mark adds the link.
	FailLinks int
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

// Add files it as Create would and returns its ref. A labels error
// (DropLabels) is ignored.
func (f *Fake) Add(it tracker.Item) string {
	got, err := f.Create(context.Background(), it, tracker.Mark{})
	if err != nil && !errors.Is(err, tracker.ErrLabels) {
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
	if err := tracker.CheckLimit(q.Limit); err != nil {
		return nil, err
	}
	var out []tracker.Item
	for k := range f.items {
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
		i := len(f.items) - 1 - k // newest first
		if q.Parent != "" {
			i = k
		}
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
		for _, b := range it.BlockedBy {
			if slices.Contains(f.items[i].BlockedBy, b) {
				continue
			}
			if _, err := f.index(b); err != nil {
				return f.view(i), err
			}
			f.items[i].BlockedBy = append(f.items[i].BlockedBy, b)
		}
		return f.view(i), f.labelsErr(i, it.Labels)
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
	want := it.Labels
	it.Labels = slices.Clone(it.Labels)
	if f.DropLabels {
		it.Labels = nil
	}
	it.BlockedBy = slices.Clone(it.BlockedBy)
	var linkErr error
	if n := len(it.BlockedBy); n > 0 && f.FailLinks > 0 {
		f.FailLinks--
		linkErr = fmt.Errorf("%s created, but linking blocker %s failed", it.Ref, it.BlockedBy[n-1])
		it.BlockedBy = it.BlockedBy[:n-1]
	}
	f.items = append(f.items, it)
	if m.Key != "" {
		if f.created == nil {
			f.created = map[string]int{}
		}
		f.created[m.Key] = len(f.items) - 1
	}
	if linkErr != nil {
		return f.view(len(f.items) - 1), linkErr
	}
	return f.view(len(f.items) - 1), f.labelsErr(len(f.items)-1, want)
}

// labelsErr is Create's error for items[i] lacking any of want: nil when
// it has them all.
func (f *Fake) labelsErr(i int, want []string) error {
	missing := tracker.MissingLabels(want, f.items[i].Labels)
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%s created without the labels %s: %w", f.items[i].Ref, strings.Join(missing, ", "), tracker.ErrLabels)
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

// Unlabel removes label from ref, matched case-insensitively. A label
// ref lacks is no error.
func (f *Fake) Unlabel(_ context.Context, ref, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	if strings.TrimSpace(label) == "" {
		return errors.New("unlabel: no label")
	}
	i, err := f.index(ref)
	if err != nil {
		return err
	}
	f.items[i].Labels = slices.DeleteFunc(f.items[i].Labels, func(l string) bool { return strings.EqualFold(l, label) })
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
