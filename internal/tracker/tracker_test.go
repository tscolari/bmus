package tracker_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/tscolari/bmus/internal/tracker"
	"github.com/tscolari/bmus/internal/tracker/trackertest"
)

// testAdapter is registered once for the registry tests: it decodes its
// section strictly into its fields.
type testAdapter struct {
	trackertest.Fake
	Repo string `yaml:"repo"`
}

func init() {
	tracker.Register("test", func(section *yaml.Node) (tracker.Tracker, error) {
		a := &testAdapter{}
		if section == nil {
			return nil, errors.New("needs a test section")
		}
		if err := section.Decode(a); err != nil {
			return nil, err
		}
		if a.Repo == "" {
			return nil, errors.New("repo is required")
		}
		return a, nil
	})
}

func TestOpen(t *testing.T) {
	t.Parallel()
	tr, err := tracker.Open([]byte("adapter: test\ntest:\n  repo: o/r\n---\n"))
	require.NoError(t, err)
	require.Equal(t, "o/r", tr.(*testAdapter).Repo)

	for _, tc := range []struct{ name, data, err string }{
		{"empty", "", "empty tracker file"},
		{"no adapter", "test:\n  repo: x\n", "adapter is required"},
		{"unknown adapter", "adapter: jira\n", `unknown adapter "jira"`},
		{"other section", "adapter: test\ngithub:\n  repo: x\n", `unknown key "github"`},
		{"adapter's error", "adapter: test\n", "test: needs a test section"},
		{"two documents", "adapter: test\n---\nadapter: test\n", "one YAML document"},
		{"not a string", "adapter: [a]\n", "non-empty string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tracker.Open([]byte(tc.data))
			require.ErrorContains(t, err, tc.err)
		})
	}
	require.Panics(t, func() { tracker.Register("test", nil) })
}

func TestLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tr, found, err := tracker.Load(filepath.Join(dir, tracker.File))
	require.NoError(t, err)
	require.False(t, found)
	require.Nil(t, tr)

	path := filepath.Join(dir, "t.yaml")
	require.NoError(t, os.WriteFile(path, []byte("adapter: nope\n"), 0o600))
	_, _, err = tracker.Load(path)
	require.ErrorContains(t, err, path+`: unknown adapter "nope"`)
}

func TestSourceRef(t *testing.T) {
	t.Parallel()
	ref, ok := tracker.SourceRef("tracker: o/r#12 ")
	require.True(t, ok)
	require.Equal(t, "o/r#12", ref)
	_, ok = tracker.SourceRef("rfc:12")
	require.False(t, ok)
}

// List returns each item's open blockers (BlockedBy), nil without
// dependency data, and filters on labels, milestone, parent and state.
func TestFakeListBlockedBy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := &trackertest.Fake{}
	epic := f.Add(tracker.Item{Title: "epic", Labels: []string{"epic"}})
	a := f.Add(tracker.Item{Title: "a", Labels: []string{"phase:1"}, Parent: epic, Milestone: "Phase 1"})
	b := f.Add(tracker.Item{Title: "b", Labels: []string{"phase:1", "area:bridge"}, Parent: epic, Milestone: "Phase 1",
		BlockedBy: []string{a}})
	c := f.Add(tracker.Item{Title: "c", Labels: []string{"phase:1"}, Parent: epic, BlockedBy: []string{a, b}})

	items, err := f.List(ctx, tracker.Query{Parent: epic})
	require.NoError(t, err)
	require.Equal(t, []string{a, b, c}, refs(items))
	require.Equal(t, []string{}, items[0].BlockedBy, "dependency data, nothing blocks it")
	require.Equal(t, []string{a}, items[1].BlockedBy)
	require.Equal(t, []string{a, b}, items[2].BlockedBy)

	items, err = f.List(ctx, tracker.Query{Labels: []string{"phase:1", "area:bridge"}, Milestone: "Phase 1"})
	require.NoError(t, err)
	require.Equal(t, []string{b}, refs(items))
	items, err = f.List(ctx, tracker.Query{Labels: []string{"PHASE:1", "Area:Bridge"}})
	require.NoError(t, err)
	require.Equal(t, []string{b}, refs(items), "labels match case-insensitively")

	// A closed blocker no longer blocks; closed items leave the open list.
	require.NoError(t, f.Close(ctx, a, tracker.ReasonNotPlanned))
	require.Equal(t, tracker.ReasonNotPlanned, f.CloseReason(a))
	items, err = f.List(ctx, tracker.Query{Parent: epic})
	require.NoError(t, err)
	require.Equal(t, []string{b, c}, refs(items))
	require.Equal(t, []string{}, items[0].BlockedBy)
	require.Equal(t, []string{b}, items[1].BlockedBy)
	items, err = f.List(ctx, tracker.Query{State: tracker.StateClosed})
	require.NoError(t, err)
	require.Equal(t, []string{a}, refs(items))
	items, err = f.List(ctx, tracker.Query{State: tracker.StateAll})
	require.NoError(t, err)
	require.Len(t, items, 4)

	f.NoDeps = true
	items, err = f.List(ctx, tracker.Query{Parent: epic})
	require.NoError(t, err)
	require.Nil(t, items[1].BlockedBy, "no dependency data")

	_, err = f.Get(ctx, "#99")
	require.ErrorIs(t, err, tracker.ErrNotFound)
	_, err = f.Get(ctx, "o/r#1")
	require.ErrorIs(t, err, tracker.ErrBadRef)
	canon, err := f.Canon(" #4 ")
	require.NoError(t, err)
	require.Equal(t, "#4", canon)
	_, err = f.Canon("#0")
	require.ErrorIs(t, err, tracker.ErrBadRef)
}

func refs(items []tracker.Item) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Ref)
	}
	return out
}

// A key adapters embed in a marker holds nothing that could end an HTML
// comment or a line.
func TestMarkCheck(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"", "H-0001", "finding ORD-0001/flaky", "a.b:c_d"} {
		require.NoError(t, tracker.Mark{Key: key, Since: time.Now()}.Check(), key)
	}
	for _, key := range []string{" H-1", "H-1 ", "a  b", "a-->b", "a\nb", "a<b", "x'y", string(make([]byte, 201))} {
		require.Error(t, tracker.Mark{Key: key, Since: time.Now()}.Check(), key)
	}
	require.ErrorContains(t, tracker.Mark{Key: "H-1"}.Check(), "has no Since")
}

// The fake honours a mark's key: a second comment under it on the same
// item posts nothing, and a second create returns the first item.
func TestFakeMark(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := &trackertest.Fake{}
	a, b := f.Add(tracker.Item{Title: "a"}), f.Add(tracker.Item{Title: "b"})
	m := tracker.Mark{Key: "H-0001", Since: time.Now()}
	require.NoError(t, f.Comment(ctx, a, "x", m))
	require.NoError(t, f.Comment(ctx, a, "x", m))
	require.NoError(t, f.Comment(ctx, b, "x", m), "another item")
	require.NoError(t, f.Comment(ctx, a, "y", tracker.Mark{}))
	require.NoError(t, f.Comment(ctx, a, "y", tracker.Mark{}), "no key: posted twice")
	require.Equal(t, []trackertest.Comment{{Ref: a, Body: "x", Key: "H-0001"}, {Ref: b, Body: "x", Key: "H-0001"},
		{Ref: a, Body: "y"}, {Ref: a, Body: "y"}}, f.Comments())
	require.Error(t, f.Comment(ctx, a, "x", tracker.Mark{Key: "a-->b"}))

	k := tracker.Mark{Key: "finding ORD-0001/k", Since: time.Now()}
	first, err := f.Create(ctx, tracker.Item{Title: "t"}, k)
	require.NoError(t, err)
	again, err := f.Create(ctx, tracker.Item{Title: "t"}, k)
	require.NoError(t, err)
	require.Equal(t, first, again)
	items, err := f.List(ctx, tracker.Query{})
	require.NoError(t, err)
	require.Len(t, items, 3)
}

// The fake keeps a created item's labels, removes one with Unlabel in any
// case (an absent one is no error), reports each closed item's reason,
// and lists newest first, stopping at a Query's Limit.
func TestFakeLabelsCloseAndLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := &trackertest.Fake{}
	a := f.Add(tracker.Item{Title: "a", Labels: []string{"bmus:unreviewed", "area:store"}})
	b := f.Add(tracker.Item{Title: "b", Labels: []string{"bmus:unreviewed"}})
	c := f.Add(tracker.Item{Title: "c", Labels: []string{"bmus:unreviewed"}})
	d := f.Add(tracker.Item{Title: "d"})

	it, err := f.Get(ctx, a)
	require.NoError(t, err)
	require.Equal(t, []string{"bmus:unreviewed", "area:store"}, it.Labels)
	require.NoError(t, f.Unlabel(ctx, a, "BMUS:Unreviewed"))
	require.NoError(t, f.Unlabel(ctx, a, "bmus:unreviewed"), "absent")
	it, err = f.Get(ctx, a)
	require.NoError(t, err)
	require.Equal(t, []string{"area:store"}, it.Labels)
	require.ErrorIs(t, f.Unlabel(ctx, "#9", "x"), tracker.ErrNotFound)
	require.ErrorIs(t, f.Unlabel(ctx, "9", "x"), tracker.ErrBadRef)
	require.ErrorContains(t, f.Unlabel(ctx, a, ""), "no label")

	require.NoError(t, f.Close(ctx, b, tracker.ReasonNotPlanned))
	require.NoError(t, f.Close(ctx, c, ""))
	require.Equal(t, tracker.ReasonNotPlanned, f.CloseReason(b))
	require.Equal(t, tracker.ReasonCompleted, f.CloseReason(c))
	require.Equal(t, "", f.CloseReason(a), "open")

	items, err := f.List(ctx, tracker.Query{State: tracker.StateAll, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{d, c}, refs(items), "newest first")
	items, err = f.List(ctx, tracker.Query{Limit: 1, Labels: []string{"bmus:unreviewed"}, State: tracker.StateAll})
	require.NoError(t, err)
	require.Equal(t, []string{c}, refs(items), "the limit counts what fits")
	items, err = f.List(ctx, tracker.Query{})
	require.NoError(t, err)
	require.Equal(t, []string{d, a}, refs(items), "no limit")
	_, err = f.List(ctx, tracker.Query{Limit: -1})
	require.ErrorContains(t, err, "limit -1")
}

func TestMissingLabels(t *testing.T) {
	t.Parallel()
	require.Nil(t, tracker.MissingLabels(nil, []string{"a"}))
	require.Nil(t, tracker.MissingLabels([]string{"Area:Store"}, []string{"area:store"}))
	require.Equal(t, []string{"b", "c"}, tracker.MissingLabels([]string{"a", "b", "c"}, []string{"A"}))
}

// With DropLabels, Create stores the item without its labels and returns
// it with ErrLabels; a retry under the same Mark returns the same item
// and error, and files nothing more.
func TestFakeDropLabels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := &trackertest.Fake{DropLabels: true}
	m := tracker.Mark{Key: "finding ORD-0001/k", Since: time.Now()}
	in := tracker.Item{Title: "t", Labels: []string{"bmus:unreviewed"}}
	for range 2 {
		it, err := f.Create(ctx, in, m)
		require.ErrorIs(t, err, tracker.ErrLabels)
		require.ErrorContains(t, err, "#1 created without the labels bmus:unreviewed")
		require.Equal(t, "#1", it.Ref)
		require.Empty(t, it.Labels)
	}
	items, err := f.List(ctx, tracker.Query{})
	require.NoError(t, err)
	require.Len(t, items, 1)
	_, err = f.Create(ctx, tracker.Item{Title: "no labels asked"}, tracker.Mark{})
	require.NoError(t, err)
	require.Equal(t, "#3", f.Add(in), "Add ignores the labels error")
}
