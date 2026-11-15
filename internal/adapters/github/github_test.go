package github

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/tracker"
)

const testToken = "test-token"

// botID is the token's account in the fake API, and the author of every
// issue and comment that names none.
const botID = 42

// fakeIssue is an issue the fake API holds.
type fakeIssue struct {
	ID        int64
	Number    int
	Title     string
	Body      string
	State     string
	Reason    string
	Labels    []string
	Milestone int // 0: none
	PR        bool
	Parent    int
	BlockedBy []int
	Comments  []fakeComment
	// Updated is when the issue last changed, for a listing's since.
	Updated time.Time
	// Author is the issue's author's id; 0 is botID.
	Author int64
	// NoSummary leaves issue_dependencies_summary out, as older GitHub
	// Enterprise servers do.
	NoSummary bool
	// Repo, if set, is the repository_url's owner/name: an issue of
	// another repository (or another spelling of o/r).
	Repo string
}

// fakeComment is a comment the fake API holds.
type fakeComment struct {
	Body string
	At   time.Time
	// Author is the comment's author's id; 0 is botID.
	Author int64
}

// user is the JSON of the account with id, botID for 0.
func user(id int64) map[string]any {
	id = cmp.Or(id, botID)
	return map[string]any{"id": id, "login": "user" + strconv.FormatInt(id, 10)}
}

// fakeAPI is an in-memory GitHub REST API for repo o/r: the endpoints the
// adapter calls, paged pageSize at a time.
type fakeAPI struct {
	t        *testing.T
	mu       sync.Mutex
	issues   []*fakeIssue
	ms       map[int]string // milestone number to title
	pageSize int
	noDeps   bool // the dependencies API answers 404
	// broken lists the "METHOD /path" requests answered 500.
	broken []string
	// appToken makes GET /user answer 403, as for a GitHub App
	// installation token.
	appToken bool
	// lag is how many more comment listings leave out the newest
	// comment, as GitHub may list a write a moment after taking it.
	lag int
	// dropLabels makes POST /issues ignore labels, as GitHub does for a
	// token without push access.
	dropLabels bool
	// repoLabels are the repository's labels: POST /issues takes a
	// requested label differing only in case as the repository spells it.
	repoLabels []string
	// label404 makes the next DELETE of a label answer 404: "gone"
	// removes the label first (someone else did), "stays" leaves it.
	label404 string
	// fileOnPage2 files a new issue just before GET /issues serves its
	// second page, shifting it by one.
	fileOnPage2 bool
	calls       []string
	srv         *httptest.Server
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, ms: map[int]string{1: "Phase 1", 2: "Phase 2"}, pageSize: 2}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) add(i fakeIssue) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	i.Number = len(f.issues) + 1
	i.ID = int64(1000 + i.Number)
	if i.State == "" {
		i.State = "open"
	}
	if i.Updated.IsZero() {
		i.Updated = time.Now()
	}
	f.issues = append(f.issues, &i)
	return i.Number
}

func (f *fakeAPI) issue(n int) *fakeIssue {
	if n < 1 || n > len(f.issues) {
		return nil
	}
	return f.issues[n-1]
}

func (f *fakeAPI) json(i *fakeIssue) map[string]any {
	labels := []map[string]any{}
	for _, l := range i.Labels {
		labels = append(labels, map[string]any{"name": l})
	}
	m := map[string]any{"id": i.ID, "number": i.Number, "title": i.Title, "body": i.Body, "state": i.State, "labels": labels,
		"repository_url": f.srv.URL + "/repos/" + cmp.Or(i.Repo, "o/r"), "milestone": nil, "user": user(i.Author)}
	if i.Body == "" {
		m["body"] = nil
	}
	if i.Milestone != 0 {
		m["milestone"] = map[string]any{"number": i.Milestone, "title": f.ms[i.Milestone]}
	}
	if i.PR {
		m["pull_request"] = map[string]any{"url": "x"}
	}
	if !i.NoSummary {
		open := 0
		for _, b := range i.BlockedBy {
			if f.issue(b).State == "open" {
				open++
			}
		}
		m["issue_dependencies_summary"] = map[string]any{"blocked_by": open, "total_blocked_by": len(i.BlockedBy)}
	}
	return m
}

func (f *fakeAPI) fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"message": msg})
}

// page writes items' page from r's page and per_page, with a Link to the
// next one.
func (f *fakeAPI) page(w http.ResponseWriter, r *http.Request, items []map[string]any) {
	per := f.pageSize
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	lo, hi := min((page-1)*per, len(items)), min(page*per, len(items))
	if hi < len(items) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, f.srv.URL, r.URL.Path, q.Encode()))
	}
	f.write(w, http.StatusOK, items[lo:hi])
}

func (f *fakeAPI) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if slices.Contains(f.broken, r.Method+" "+r.URL.Path) {
		f.fail(w, http.StatusInternalServerError, "Server Error")
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		f.fail(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") == "" {
		f.fail(w, http.StatusBadRequest, "bad headers")
		return
	}
	var in map[string]any
	if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPatch) {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			f.fail(w, http.StatusBadRequest, "bad body")
			return
		}
	}
	if r.Method == http.MethodGet && r.URL.Path == "/user" {
		if f.appToken {
			f.fail(w, http.StatusForbidden, "Resource not accessible by integration")
			return
		}
		f.write(w, http.StatusOK, user(botID))
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/repos/o/r")
	if !ok {
		f.fail(w, http.StatusNotFound, "Not Found")
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && path == "/milestones":
		var ms []map[string]any
		for n := 1; n <= len(f.ms); n++ {
			ms = append(ms, map[string]any{"number": n, "title": f.ms[n]})
		}
		f.page(w, r, ms)
	case r.Method == http.MethodGet && path == "/issues":
		q := r.URL.Query()
		if f.fileOnPage2 && q.Get("page") == "2" {
			f.fileOnPage2 = false
			n := len(f.issues) + 1
			f.issues = append(f.issues, &fakeIssue{Number: n, ID: int64(1000 + n), Title: "filed meanwhile", State: "open", Updated: time.Now()})
		}
		since := f.since(r)
		var out []map[string]any
		for _, i := range f.issues {
			if i.Updated.Before(since) {
				continue
			}
			if st := q.Get("state"); st != "all" && st != i.State {
				continue
			}
			if l := q.Get("labels"); l != "" && slices.ContainsFunc(strings.Split(l, ","), func(l string) bool { return !slices.Contains(i.Labels, l) }) {
				continue
			}
			if m := q.Get("milestone"); m != "" && m != strconv.Itoa(i.Milestone) {
				continue
			}
			out = append(out, f.json(i))
		}
		switch d := q.Get("direction"); {
		case d == "desc" && q.Get("sort") == "created":
			slices.Reverse(out)
		case d != "":
			f.fail(w, http.StatusUnprocessableEntity, "unexpected sort")
			return
		}
		f.page(w, r, out)
	case r.Method == http.MethodPost && path == "/issues":
		i := &fakeIssue{Title: in["title"].(string), Body: in["body"].(string), State: "open", Updated: time.Now()}
		for _, l := range asList(in["labels"]) {
			name := l.(string)
			if k := slices.IndexFunc(f.repoLabels, func(r string) bool { return strings.EqualFold(r, name) }); k >= 0 {
				name = f.repoLabels[k]
			}
			if !f.dropLabels {
				i.Labels = append(i.Labels, name)
			}
		}
		if m, ok := in["milestone"].(float64); ok {
			i.Milestone = int(m)
		}
		i.Number = len(f.issues) + 1
		i.ID = int64(1000 + i.Number)
		f.issues = append(f.issues, i)
		f.write(w, http.StatusCreated, f.json(i))
	case len(parts) >= 2 && parts[0] == "issues":
		n, _ := strconv.Atoi(parts[1])
		i := f.issue(n)
		if i == nil {
			f.fail(w, http.StatusNotFound, "Not Found")
			return
		}
		rest := strings.Join(parts[2:], "/")
		switch {
		case rest == "" && r.Method == http.MethodGet:
			f.write(w, http.StatusOK, f.json(i))
		case rest == "" && r.Method == http.MethodPatch:
			i.State, i.Reason = in["state"].(string), in["state_reason"].(string)
			f.write(w, http.StatusOK, f.json(i))
		case strings.HasPrefix(rest, "labels/") && r.Method == http.MethodDelete:
			// The name is one path segment, escaped: read it from the raw
			// path, where a "/" in it is still %2F.
			_, esc, _ := strings.Cut(r.URL.EscapedPath(), "/labels/")
			require.NotContains(f.t, esc, "/", "the label name is one path segment")
			name, err := url.PathUnescape(esc)
			require.NoError(f.t, err)
			k := slices.IndexFunc(i.Labels, func(l string) bool { return strings.EqualFold(l, name) })
			mode := f.label404
			f.label404 = ""
			if mode == "gone" && k >= 0 {
				i.Labels = slices.Delete(i.Labels, k, k+1)
			}
			if k < 0 || mode != "" {
				f.fail(w, http.StatusNotFound, "Label does not exist")
				return
			}
			i.Labels = slices.Delete(i.Labels, k, k+1)
			f.write(w, http.StatusOK, f.json(i)["labels"])
		case rest == "comments" && r.Method == http.MethodPost:
			i.Comments = append(i.Comments, fakeComment{Body: in["body"].(string), At: time.Now()})
			f.write(w, http.StatusCreated, map[string]any{"id": len(i.Comments)})
		case rest == "comments" && r.Method == http.MethodGet:
			since := f.since(r)
			cs := i.Comments
			if f.lag > 0 && len(cs) > 0 {
				f.lag--
				cs = cs[:len(cs)-1]
			}
			out := []map[string]any{}
			for k, c := range cs {
				if !c.At.Before(since) {
					at := c.At.UTC().Format(time.RFC3339)
					out = append(out, map[string]any{"id": k + 1, "body": c.Body, "created_at": at, "updated_at": at, "user": user(c.Author)})
				}
			}
			f.page(w, r, out)
		case rest == "sub_issues" && r.Method == http.MethodGet:
			var out []map[string]any
			for _, c := range f.issues {
				if c.Parent == n {
					out = append(out, f.json(c))
				}
			}
			f.page(w, r, out)
		case rest == "sub_issues" && r.Method == http.MethodPost:
			id := int64(in["sub_issue_id"].(float64))
			c := f.issue(int(id - 1000))
			if c == nil {
				f.fail(w, http.StatusUnprocessableEntity, "no such sub-issue")
				return
			}
			c.Parent = n
			f.write(w, http.StatusCreated, f.json(i))
		case rest == "dependencies/blocked_by" && f.noDeps:
			f.fail(w, http.StatusNotFound, "Not Found")
		case rest == "dependencies/blocked_by" && r.Method == http.MethodGet:
			var out []map[string]any
			for _, b := range i.BlockedBy {
				out = append(out, f.json(f.issue(b)))
			}
			f.page(w, r, out)
		case rest == "dependencies/blocked_by" && r.Method == http.MethodPost:
			id := int64(in["issue_id"].(float64))
			b := f.issue(int(id - 1000))
			if b == nil {
				f.fail(w, http.StatusUnprocessableEntity, "no such issue")
				return
			}
			i.BlockedBy = append(i.BlockedBy, b.Number)
			f.write(w, http.StatusCreated, f.json(i))
		default:
			f.fail(w, http.StatusNotFound, "Not Found")
		}
	default:
		f.fail(w, http.StatusNotFound, "Not Found")
	}
}

// since is r's since parameter, the zero time without one.
func (f *fakeAPI) since(r *http.Request) time.Time {
	s := r.URL.Query().Get("since")
	if s == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339, s)
	require.NoError(f.t, err, "since %q", s)
	return at
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func (f *fakeAPI) adapter(t *testing.T, token string) *GitHub {
	t.Helper()
	g, err := New(Config{Repo: "o/r", APIURL: f.srv.URL}, token, f.srv.Client())
	require.NoError(t, err)
	g.settle = 0
	return g
}

func refsOf(items []tracker.Item) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Ref)
	}
	return out
}

// List reads every page, leaves pull requests out, and returns each
// issue's open blockers: from the dependencies API, none when GitHub's
// summary counts none, and nil when the API isn't there.
func TestListWithBlockers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	epic := f.add(fakeIssue{Title: "Phase 1", Labels: []string{"epic"}, Milestone: 1})
	a := f.add(fakeIssue{Title: "a", Body: "do a", Labels: []string{"phase:1", "area:store"}, Milestone: 1, Parent: epic})
	done := f.add(fakeIssue{Title: "done", State: "closed", Labels: []string{"phase:1"}, Parent: epic})
	b := f.add(fakeIssue{Title: "b", Labels: []string{"phase:1", "area:bridge"}, Milestone: 1, Parent: epic,
		BlockedBy: []int{a, done}})
	f.add(fakeIssue{Title: "a pull request", Labels: []string{"phase:1"}, PR: true})
	c := f.add(fakeIssue{Title: "c", Labels: []string{"phase:1"}, Parent: epic, BlockedBy: []int{b}, NoSummary: true})
	g := f.adapter(t, testToken)

	items, err := g.List(ctx, tracker.Query{Labels: []string{"phase:1"}})
	require.NoError(t, err)
	require.Equal(t, []string{"o/r#6", "o/r#4", "o/r#2"}, refsOf(items), "newest first")
	require.Equal(t, tracker.Item{Ref: "o/r#2", Title: "a", Body: "do a", Labels: []string{"phase:1", "area:store"},
		Milestone: "Phase 1", State: "open", BlockedBy: []string{}}, items[2])
	require.Equal(t, []string{"o/r#2"}, items[1].BlockedBy, "the closed blocker no longer blocks")
	require.Equal(t, []string{"o/r#4"}, items[0].BlockedBy, "no summary: read from the dependencies API")
	require.Equal(t, "", items[0].Milestone)

	// Labels and milestone filter on GitHub's side.
	items, err = g.List(ctx, tracker.Query{Labels: []string{"phase:1", "area:bridge"}, Milestone: "Phase 1"})
	require.NoError(t, err)
	require.Equal(t, []string{"o/r#4"}, refsOf(items))
	items, err = g.List(ctx, tracker.Query{Milestone: "Phase 9"})
	require.ErrorIs(t, err, tracker.ErrNotFound, "no such milestone")
	require.ErrorContains(t, err, `milestone "Phase 9"`)
	require.Nil(t, items)

	// A parent's sub-issues, filtered here.
	items, err = g.List(ctx, tracker.Query{Parent: "#" + strconv.Itoa(epic), Labels: []string{"phase:1"}})
	require.NoError(t, err)
	require.Equal(t, []string{"o/r#2", "o/r#4", "o/r#6"}, refsOf(items))
	require.Equal(t, "o/r#1", items[0].Parent)
	items, err = g.List(ctx, tracker.Query{Parent: "o/r#1", State: tracker.StateClosed})
	require.NoError(t, err)
	require.Equal(t, []string{"o/r#3"}, refsOf(items))
	items, err = g.List(ctx, tracker.Query{Parent: "1", State: tracker.StateAll})
	require.NoError(t, err)
	require.Len(t, items, 4)

	it, err := g.Get(ctx, "o/r#"+strconv.Itoa(c))
	require.NoError(t, err)
	require.Equal(t, []string{"o/r#4"}, it.BlockedBy)

	// Without the dependencies API, BlockedBy is nil: no dependency data.
	f.mu.Lock()
	f.noDeps = true
	f.mu.Unlock()
	it, err = g.Get(ctx, "#"+strconv.Itoa(b))
	require.NoError(t, err)
	require.Nil(t, it.BlockedBy)
	it, err = g.Get(ctx, "#"+strconv.Itoa(a))
	require.NoError(t, err)
	require.Equal(t, []string{}, it.BlockedBy, "the summary says none")

	_, err = g.Get(ctx, "#5")
	require.ErrorIs(t, err, tracker.ErrNotFound, "a pull request")
	require.ErrorContains(t, err, "o/r#5 is a pull request")
	_, err = g.List(ctx, tracker.Query{State: "merged"})
	require.ErrorContains(t, err, `unknown state "merged"`)
}

// Create files the issue, then links its parent and blockers.
func TestCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	epic := f.add(fakeIssue{Title: "epic"})
	blocker := f.add(fakeIssue{Title: "blocker"})
	closed := f.add(fakeIssue{Title: "closed", State: "closed"})
	g := f.adapter(t, testToken)

	it, err := g.Create(ctx, tracker.Item{Ref: "ignored", Title: "new", Body: "the body", Labels: []string{"bmus:unreviewed"},
		Milestone: "Phase 2", Parent: "#" + strconv.Itoa(epic), BlockedBy: []string{"o/r#" + strconv.Itoa(blocker), strconv.Itoa(closed)}}, tracker.Mark{})
	require.NoError(t, err)
	require.Equal(t, tracker.Item{Ref: "o/r#4", Title: "new", Body: "the body", Labels: []string{"bmus:unreviewed"},
		Milestone: "Phase 2", Parent: "o/r#1", State: "open", BlockedBy: []string{"o/r#2"}}, it)
	f.mu.Lock()
	n := f.issue(4)
	require.Equal(t, epic, n.Parent)
	require.Equal(t, []int{blocker, closed}, n.BlockedBy)
	require.Equal(t, 2, n.Milestone)
	f.mu.Unlock()

	got, err := g.Get(ctx, it.Ref)
	require.NoError(t, err)
	require.Equal(t, []string{"o/r#2"}, got.BlockedBy)
	items, err := g.List(ctx, tracker.Query{Parent: "o/r#1"})
	require.NoError(t, err)
	require.Equal(t, []string{"o/r#4"}, refsOf(items))

	_, err = g.Create(ctx, tracker.Item{Title: "x", Milestone: "Phase 9"}, tracker.Mark{})
	require.ErrorIs(t, err, tracker.ErrNotFound)
	require.ErrorContains(t, err, `milestone "Phase 9"`)
	_, err = g.Create(ctx, tracker.Item{Title: " "}, tracker.Mark{})
	require.ErrorContains(t, err, "needs a title")
	_, err = g.Create(ctx, tracker.Item{Title: "x", Parent: "other/repo#1"}, tracker.Mark{})
	require.ErrorIs(t, err, tracker.ErrBadRef)

	// A missing parent or blocker fails before anything is created.
	_, err = g.Create(ctx, tracker.Item{Title: "early", BlockedBy: []string{"#99"}}, tracker.Mark{})
	require.ErrorIs(t, err, tracker.ErrNotFound)
	require.ErrorContains(t, err, "blocker o/r#99")
	_, err = g.Create(ctx, tracker.Item{Title: "early", Parent: "#98"}, tracker.Mark{})
	require.ErrorIs(t, err, tracker.ErrNotFound)
	require.ErrorContains(t, err, "parent o/r#98")
	f.mu.Lock()
	require.Len(t, f.issues, 4, "nothing created")
	f.noDeps = true
	f.mu.Unlock()

	// A link that fails afterwards: the issue exists, and its ref comes
	// back with the error.
	it, err = g.Create(ctx, tracker.Item{Title: "late", BlockedBy: []string{"#2"}}, tracker.Mark{})
	require.ErrorIs(t, err, tracker.ErrNotFound)
	require.ErrorContains(t, err, "o/r#5 created, but not blocked by o/r#2")
	require.Equal(t, "o/r#5", it.Ref)
}

// GitHub drops labels silently for a token without push access. Create
// reports the ones missing, wrapping tracker.ErrLabels, with the issue's
// ref: it exists, linked as asked. A retry under the same key finds it,
// creates no twin, and reports the same lack.
func TestCreateMissingLabels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	blocker := f.add(fakeIssue{Title: "blocker"})
	g := f.adapter(t, testToken)
	f.mu.Lock()
	f.dropLabels = true
	f.mu.Unlock()

	m := tracker.Mark{Key: "finding ORD-0001/flaky", Since: time.Now()}
	in := tracker.Item{Title: "Flaky test", Labels: []string{"bmus:unreviewed", "area:store"}, BlockedBy: []string{"#1"}}
	for range 2 {
		it, err := g.Create(ctx, in, m)
		require.ErrorIs(t, err, tracker.ErrLabels)
		require.ErrorContains(t, err, "o/r#2 created without the labels bmus:unreviewed, area:store")
		require.Equal(t, "o/r#2", it.Ref)
		require.Equal(t, []string{}, it.Labels)
		require.Equal(t, []string{"o/r#1"}, it.BlockedBy)
	}
	require.Equal(t, 1, f.count("POST /repos/o/r/issues"), "the retry found the issue")
	require.Equal(t, 1, f.count("POST /repos/o/r/issues/2/dependencies/blocked_by"))
	f.mu.Lock()
	require.Len(t, f.issues, 2)
	require.Equal(t, []int{blocker}, f.issue(2).BlockedBy)
	f.dropLabels = false
	f.mu.Unlock()

	// A label the repository spells in another case is kept as GitHub
	// spells it: no error.
	f.mu.Lock()
	f.repoLabels = []string{"Area:Store"}
	f.mu.Unlock()
	it, err := g.Create(ctx, tracker.Item{Title: "kept", Labels: []string{"area:store"}}, tracker.Mark{})
	require.NoError(t, err)
	require.Equal(t, []string{"Area:Store"}, it.Labels)
}

// Unlabel reads the issue first: a pull request or a missing issue is
// refused, and a label the issue lacks (in any case) is done with no
// DELETE. The DELETE names the label as the issue spells it, escaped as
// one path segment. A 404 from it reads the issue again: the label gone
// is done, the label still there is the error.
func TestUnlabel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	odd := []string{"a/b c", "100%", "x#y", "why?", "\u00e9tiquette \u2713"}
	n := f.add(fakeIssue{Title: "x", Labels: append([]string{"bmus:unreviewed", "area:store"}, odd...)})
	pr := f.add(fakeIssue{Title: "a pull request", PR: true, Labels: []string{"bmus:unreviewed"}})
	race := f.add(fakeIssue{Title: "y", Labels: []string{"bmus:unreviewed"}})
	g := f.adapter(t, testToken)

	require.NoError(t, g.Unlabel(ctx, "o/r#1", "BMUS:Unreviewed"))
	require.Equal(t, 1, f.count("DELETE /repos/o/r/issues/1/labels/bmus:unreviewed"), "the issue's spelling")
	for _, l := range odd {
		require.NoError(t, g.Unlabel(ctx, "#1", l), l)
		require.Equal(t, 1, f.count("DELETE /repos/o/r/issues/1/labels/"+l), "one escaped segment: %s", l)
	}
	f.mu.Lock()
	require.Equal(t, []string{"area:store"}, f.issue(n).Labels)
	f.mu.Unlock()

	// Absent: done, with no DELETE.
	require.NoError(t, g.Unlabel(ctx, "1", "bmus:unreviewed"))
	require.Equal(t, 1, f.count("DELETE /repos/o/r/issues/1/labels/bmus:unreviewed"))

	// A 404 from the DELETE: the issue is read again. Someone else
	// removed the label: done. The label still there: the error.
	f.mu.Lock()
	f.label404 = "stays"
	f.mu.Unlock()
	err := g.Unlabel(ctx, "#3", "bmus:unreviewed")
	require.ErrorIs(t, err, tracker.ErrNotFound)
	require.ErrorContains(t, err, "DELETE /repos/o/r/issues/3/labels/bmus:unreviewed: 404")
	require.Equal(t, 2, f.count("GET /repos/o/r/issues/3"))
	f.mu.Lock()
	require.Equal(t, []string{"bmus:unreviewed"}, f.issue(race).Labels)
	f.label404 = "gone"
	f.mu.Unlock()
	require.NoError(t, g.Unlabel(ctx, "#3", "bmus:unreviewed"))
	require.Equal(t, 4, f.count("GET /repos/o/r/issues/3"))
	f.mu.Lock()
	require.Empty(t, f.issue(race).Labels)
	f.mu.Unlock()

	// A pull request, a missing issue, a bad ref, no label, and "." or
	// "..", which a path segment can't name: refused, nothing deleted.
	err = g.Unlabel(ctx, "#"+strconv.Itoa(pr), "bmus:unreviewed")
	require.ErrorIs(t, err, tracker.ErrNotFound)
	require.ErrorContains(t, err, "is a pull request")
	f.mu.Lock()
	require.Equal(t, []string{"bmus:unreviewed"}, f.issue(pr).Labels)
	f.mu.Unlock()
	require.ErrorIs(t, g.Unlabel(ctx, "#9", "bmus:unreviewed"), tracker.ErrNotFound)
	require.ErrorIs(t, g.Unlabel(ctx, "other/x#1", "bmus:unreviewed"), tracker.ErrBadRef)
	require.ErrorContains(t, g.Unlabel(ctx, "#1", " "), "no label")
	require.ErrorContains(t, g.Unlabel(ctx, "#1", "."), "can't be named")
	require.ErrorContains(t, g.Unlabel(ctx, "#1", ".."), "can't be named")
	require.Zero(t, f.count("DELETE /repos/o/r/issues/2/labels/bmus:unreviewed"))
	require.Zero(t, f.count("DELETE /repos/o/r/issues/9/labels/bmus:unreviewed"))
}

// List reads newest first and stops paging once Limit issues fit,
// reading blockers for those alone. Pull requests and issues the query
// filters out don't count.
func TestListLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	for k := range 8 {
		f.add(fakeIssue{Title: "i" + strconv.Itoa(k), PR: k == 6, NoSummary: true})
	}
	g := f.adapter(t, testToken)

	// Pages of two, newest first: #8 and the pull request #7, then #6 and
	// #5, where three issues fit.
	items, err := g.List(ctx, tracker.Query{Limit: 3})
	require.NoError(t, err)
	require.Equal(t, []string{"o/r#8", "o/r#6", "o/r#5"}, refsOf(items))
	require.Equal(t, 2, f.count("GET /repos/o/r/issues"), "two pages read of four")
	for k := 1; k <= 8; k++ {
		call := fmt.Sprintf("GET /repos/o/r/issues/%d/dependencies/blocked_by", k)
		want := 0
		if k == 8 || k == 6 || k == 5 {
			want = 1
		}
		require.Equal(t, want, f.count(call), call)
	}

	// No limit reads everything; a limit past the end is everything too.
	items, err = g.List(ctx, tracker.Query{})
	require.NoError(t, err)
	require.Len(t, items, 7)
	items, err = g.List(ctx, tracker.Query{Limit: 50})
	require.NoError(t, err)
	require.Len(t, items, 7)

	// A parent's sub-issues, filtered here and in the parent's order,
	// stop at the limit too.
	f.mu.Lock()
	for _, i := range f.issues[2:] {
		i.Parent = 1
	}
	f.issues[2].State = "closed"
	f.mu.Unlock()
	items, err = g.List(ctx, tracker.Query{Parent: "#1", Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{"o/r#4", "o/r#5"}, refsOf(items))
	require.Equal(t, 2, f.count("GET /repos/o/r/issues/1/sub_issues"), "two pages read of three")

	_, err = g.List(ctx, tracker.Query{Limit: -1})
	require.ErrorContains(t, err, "limit -1")
}

// An issue filed while List pages shifts the next page by one, which
// repeats the last issue read: it is listed, and counted, once.
func TestListShiftedPage(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t)
	for k := range 4 {
		f.add(fakeIssue{Title: "i" + strconv.Itoa(k)})
	}
	f.fileOnPage2 = true
	g := f.adapter(t, testToken)
	items, err := g.List(context.Background(), tracker.Query{})
	require.NoError(t, err)
	require.Equal(t, []string{"o/r#4", "o/r#3", "o/r#2", "o/r#1"}, refsOf(items))
}

// A parent's sub-issue, or a blocker, in another repository keeps its own
// ref, and its BlockedBy is nil: the adapter calls no endpoint of that
// repository, and none of the configured one under its number. An issue
// whose repository_url spells the configured repo differently gets the
// configured spelling.
func TestForeignIssues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	epic := f.add(fakeIssue{Title: "epic"})
	foreign := f.add(fakeIssue{Title: "elsewhere", Parent: epic, Repo: "other/x", BlockedBy: []int{epic}})
	local := f.add(fakeIssue{Title: "here", Parent: epic, Repo: "O/R", BlockedBy: []int{foreign}})
	g := f.adapter(t, testToken)

	items, err := g.List(ctx, tracker.Query{Parent: "o/r#1"})
	require.NoError(t, err)
	require.Equal(t, []string{"other/x#" + strconv.Itoa(foreign), "o/r#" + strconv.Itoa(local)}, refsOf(items))
	require.Nil(t, items[0].BlockedBy, "no dependency data read for a foreign issue")
	require.Equal(t, []string{"other/x#2"}, items[1].BlockedBy, "a foreign blocker keeps its own ref")
	f.mu.Lock()
	require.NotContains(t, f.calls, "GET /repos/o/r/issues/2/dependencies/blocked_by")
	f.mu.Unlock()
}

// A Link to another host is not followed: the token would go with it.
func TestLinkToAnotherHost(t *testing.T) {
	t.Parallel()
	var elsewhere int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere++
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<`+other.URL+`/repos/o/r/issues?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`[{"id": 1001, "number": 1, "title": "t", "state": "open", "labels": [],
			"issue_dependencies_summary": {"blocked_by": 0}}]`))
	}))
	t.Cleanup(srv.Close)
	g, err := New(Config{Repo: "o/r", APIURL: srv.URL}, testToken, srv.Client())
	require.NoError(t, err)
	items, err := g.List(context.Background(), tracker.Query{})
	require.NoError(t, err)
	require.Equal(t, []string{"o/r#1"}, refsOf(items))
	require.Zero(t, elsewhere)
}

func TestCommentAndClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	n := f.add(fakeIssue{Title: "x"})
	g := f.adapter(t, testToken)

	require.NoError(t, g.Comment(ctx, "o/r#1", "looked at it", tracker.Mark{}))
	require.NoError(t, g.Close(ctx, "#1", ""))
	f.mu.Lock()
	require.Equal(t, "looked at it", f.issue(n).Comments[0].Body, "no key, no marker")
	require.Equal(t, "closed", f.issue(n).State)
	require.Equal(t, tracker.ReasonCompleted, f.issue(n).Reason)
	f.mu.Unlock()
	require.NoError(t, g.Close(ctx, "1", tracker.ReasonNotPlanned), "closing a closed issue")
	f.mu.Lock()
	require.Equal(t, tracker.ReasonNotPlanned, f.issue(n).Reason)
	f.mu.Unlock()

	require.ErrorContains(t, g.Close(ctx, "#1", "duplicate"), `unknown close reason "duplicate"`)
	require.ErrorContains(t, g.Comment(ctx, "#1", " ", tracker.Mark{}), "needs a body")
	require.ErrorIs(t, g.Comment(ctx, "#7", "x", tracker.Mark{}), tracker.ErrNotFound)

	// A pull request is refused before anything is written.
	pr := f.add(fakeIssue{Title: "a pull request", PR: true})
	require.ErrorContains(t, g.Comment(ctx, "#"+strconv.Itoa(pr), "x", tracker.Mark{}), "is a pull request")
	require.ErrorContains(t, g.Close(ctx, "#"+strconv.Itoa(pr), ""), "is a pull request")
	f.mu.Lock()
	require.Empty(t, f.issue(pr).Comments)
	require.Equal(t, "open", f.issue(pr).State)
	f.mu.Unlock()

	canon, err := g.Canon(" 1")
	require.NoError(t, err)
	require.Equal(t, "o/r#1", canon)
	_, err = g.Canon("other/x#1")
	require.ErrorIs(t, err, tracker.ErrBadRef)
}

// Errors map to the tracker's sentinels, carrying GitHub's message.
func TestErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	f.add(fakeIssue{Title: "x"})

	_, err := f.adapter(t, testToken).Get(ctx, "#2")
	require.ErrorIs(t, err, tracker.ErrNotFound)
	require.EqualError(t, err, "github: GET /repos/o/r/issues/2: 404 Not Found: Not Found")

	bad := f.adapter(t, "wrong")
	_, err = bad.Get(ctx, "#1")
	require.ErrorIs(t, err, tracker.ErrAuth)
	require.ErrorContains(t, err, "401 Unauthorized: Bad credentials")
	_, err = bad.List(ctx, tracker.Query{})
	require.ErrorIs(t, err, tracker.ErrAuth)
	require.ErrorIs(t, bad.Comment(ctx, "#1", "x", tracker.Mark{}), tracker.ErrAuth)
	require.ErrorIs(t, bad.Close(ctx, "#1", ""), tracker.ErrAuth)

	g := f.adapter(t, testToken)
	for _, ref := range []string{"other/repo#1", "o/r", "#0", "x#1", "o/r1", ""} {
		_, err := g.Get(ctx, ref)
		require.ErrorIs(t, err, tracker.ErrBadRef, ref)
	}
	_, err = g.Get(ctx, "O/R#1")
	require.NoError(t, err, "owner and name match case-insensitively")

	// A forbidden answer is an auth error, unless it is a rate limit: the
	// primary one (no requests remaining), a secondary one (Retry-After,
	// or a message naming it), or a 429.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, msg := http.StatusForbidden, "nope"
		switch r.URL.Path {
		case "/repos/o/r/issues/1":
			w.Header().Set("X-RateLimit-Remaining", "0")
		case "/repos/o/r/issues/2":
			w.Header().Set("Retry-After", "60")
		case "/repos/o/r/issues/3":
			msg = "You have exceeded a secondary rate limit."
		case "/repos/o/r/issues/4":
			status = http.StatusTooManyRequests
		}
		w.Header().Set("X-RateLimit-Remaining", cmp.Or(w.Header().Get("X-RateLimit-Remaining"), "4999"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message": "` + msg + `"}`))
	}))
	t.Cleanup(srv.Close)
	g, err = New(Config{Repo: "o/r", APIURL: srv.URL}, testToken, srv.Client())
	require.NoError(t, err)
	for _, ref := range []string{"#1", "#2", "#3", "#4"} {
		_, err = g.Get(ctx, ref)
		require.ErrorIs(t, err, tracker.ErrRateLimited, ref)
		require.False(t, errors.Is(err, tracker.ErrAuth), ref)
	}
	_, err = g.Get(ctx, "#5")
	require.ErrorIs(t, err, tracker.ErrAuth)
	require.ErrorContains(t, err, "403 Forbidden: nope")
}

func TestGHHost(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://api.github.com":              "github.com",
		"https://API.GitHub.com/":             "github.com",
		"https://ghe.example.com/api/v3":      "ghe.example.com",
		"https://ghe.example.com:8443/api/v3": "ghe.example.com:8443",
		"http://127.0.0.1:4000":               "127.0.0.1:4000",
	} {
		require.Equal(t, want, GHHost(in), in)
	}
}

func TestResolveToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := func(v string) func(string) string { return func(string) string { return v } }
	gh := func(tok string, err error) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return tok, err }
	}
	tok, err := ResolveToken(ctx, "GH_TOK", env(" from-env\n"), gh("", errors.New("never called")))
	require.NoError(t, err)
	require.Equal(t, "from-env", tok)
	tok, err = ResolveToken(ctx, "GH_TOK", env(""), gh("from-gh", nil))
	require.NoError(t, err)
	require.Equal(t, "from-gh", tok)
	tok, err = ResolveToken(ctx, "", env("ignored"), gh("from-gh", nil))
	require.NoError(t, err)
	require.Equal(t, "from-gh", tok)
	_, err = ResolveToken(ctx, "GH_TOK", env(""), gh("", errors.New("not logged in")))
	require.EqualError(t, err, "no token: $GH_TOK is empty, and gh auth token failed: not logged in")
	_, err = ResolveToken(ctx, "", env(""), gh("", nil))
	require.EqualError(t, err, "no token: set token_env, or log gh in (gh auth token failed: it printed no token)")
}

func TestOpen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ data, err string }{
		{"adapter: github\n", "needs a github section"},
		{"adapter: github\ngithub:\n  repo: nope\n", `repo "nope": want owner/name`},
		{"adapter: github\ngithub:\n  repo: o/r\n  api_url: ftp://x\n", `api_url "ftp://x": want an http(s) URL`},
		{"adapter: github\ngithub:\n  repo: o/r\n  token: x\n", "field token not found"},
		{"adapter: github\ngithub:\n  repo: o/r\n  api_url: http://ghe.example.com/api/v3\n", "http is only for a loopback host"},
	} {
		_, err := tracker.Open([]byte(tc.data))
		require.ErrorContains(t, err, "github: ", tc.data)
		require.ErrorContains(t, err, tc.err, tc.data)
	}
}

// Writes never follow a redirect: GitHub answers 301 for a transferred
// issue or a renamed repo, and a followed 307 on PATCH would close another
// repository's issue. The 3xx comes back as an error. A GET follows one
// only under api_url, and an issue that answers under another number or
// repository is not the one asked for.
func TestRedirects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var mu sync.Mutex
	var hits []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, "other "+r.Method+" "+r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(other.Close)
	issueJSON := func(repo string, n int) string {
		return fmt.Sprintf(`{"id": %d, "number": %d, "title": "t", "state": "open", "labels": [], "repository_url": "http://x/repos/%s"}`,
			1000+n, n, repo)
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.Method+" "+r.URL.Path)
		mu.Unlock()
		redirect := func(status int, to string) {
			w.Header().Set("Location", to)
			w.WriteHeader(status)
		}
		switch {
		case r.URL.Path == "/repos/other/secret/issues/7" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(issueJSON("other/secret", 7)))
		case strings.HasPrefix(r.URL.Path, "/repos/other/"):
			w.WriteHeader(http.StatusOK) // a write that must never arrive
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/8":
			redirect(http.StatusMovedPermanently, other.URL+"/repos/o/r/issues/8")
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/9":
			redirect(http.StatusMovedPermanently, srv.URL+"/repos/other/secret/issues/7")
		case r.Method == http.MethodGet:
			n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues/"))
			_, _ = w.Write([]byte(issueJSON("o/r", n)))
		case strings.HasSuffix(r.URL.Path, "/5") || strings.HasSuffix(r.URL.Path, "/5/comments"):
			redirect(http.StatusTemporaryRedirect, srv.URL+"/repos/other/secret/issues/7"+strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues/5"))
		default:
			redirect(http.StatusMovedPermanently, srv.URL+"/repos/other/secret/issues/7"+strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues/6"))
		}
	}))
	t.Cleanup(srv.Close)
	g, err := New(Config{Repo: "o/r", APIURL: srv.URL}, testToken, srv.Client())
	require.NoError(t, err)

	for _, ref := range []string{"#5", "#6"} {
		err := g.Close(ctx, ref, "")
		require.Error(t, err, ref)
		require.Regexp(t, `PATCH /repos/o/r/issues/[56]: 30[17] `, err.Error())
		err = g.Comment(ctx, ref, "hello", tracker.Mark{})
		require.Error(t, err, ref)
		require.Regexp(t, `POST /repos/o/r/issues/[56]/comments: 30[17] `, err.Error())
	}
	_, err = g.Get(ctx, "#8")
	require.ErrorContains(t, err, "GET /repos/o/r/issues/8: 301 ", "a redirect off api_url is not followed")
	_, err = g.Get(ctx, "#9")
	require.ErrorIs(t, err, tracker.ErrNotFound)
	require.ErrorContains(t, err, "o/r#9 now answers as other/secret#7")
	mu.Lock()
	defer mu.Unlock()
	for _, h := range hits {
		require.False(t, strings.HasPrefix(h, "other "), "the other host got %s", h)
		require.False(t, strings.HasPrefix(h, "PATCH /repos/other/") || strings.HasPrefix(h, "POST /repos/other/"), "a write followed a redirect: %s", h)
	}
}

// timeoutError is what an http.Client's transport reports when its
// timeout fires.
type timeoutError struct{}

func (timeoutError) Error() string   { return "Client.Timeout exceeded while awaiting headers" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// lostAnswer is a transport that fails the first n requests whose method
// is method and whose path ends with suffix with a timeout. With sent, the
// request reaches the server first, and only its answer is lost: GitHub
// took the write, but the client timed out waiting for it.
type lostAnswer struct {
	base           http.RoundTripper
	method, suffix string
	sent           bool
	mu             sync.Mutex
	n              int
}

func (l *lostAnswer) RoundTrip(r *http.Request) (*http.Response, error) {
	l.mu.Lock()
	lose := l.n > 0 && r.Method == l.method && strings.HasSuffix(r.URL.Path, l.suffix)
	if lose {
		l.n--
	}
	l.mu.Unlock()
	if !lose {
		return l.base.RoundTrip(r)
	}
	if l.sent {
		resp, err := l.base.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		_ = resp.Body.Close()
	}
	return nil, timeoutError{}
}

// lossy is an adapter on f whose transport loses answers as l does.
func (f *fakeAPI) lossy(t *testing.T, l *lostAnswer) *GitHub {
	t.Helper()
	l.base = f.srv.Client().Transport
	g, err := New(Config{Repo: "o/r", APIURL: f.srv.URL}, testToken, &http.Client{Transport: l})
	require.NoError(t, err)
	g.settle = 0
	return g
}

// count is how many of f's calls were call.
func (f *fakeAPI) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func (f *fakeAPI) comments(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.issue(n).Comments {
		out = append(out, c.Body)
	}
	return out
}

// A comment under a key ends with its marker (#206). GitHub takes the
// first POST, but the client times out: the lookup that follows finds the
// marker, and the comment counts as posted. A retry finds it too and posts
// nothing, so the fake holds exactly one comment.
func TestCommentTimeoutThenMarker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	n := f.add(fakeIssue{Title: "x"})
	post := "POST /repos/o/r/issues/1/comments"
	l := &lostAnswer{method: http.MethodPost, suffix: "/comments", sent: true, n: 1}
	g := f.lossy(t, l)
	m := tracker.Mark{Key: "H-0001", Since: time.Now()}

	require.NoError(t, g.Comment(ctx, "#1", "Fixed.", m), "the timeout, then the marker found")
	require.Equal(t, []string{"Fixed.\n\n<!-- bmus H-0001 -->"}, f.comments(n))
	require.NoError(t, g.Comment(ctx, "o/r#1", "Fixed.", m), "a retry")
	require.Equal(t, []string{"Fixed.\n\n<!-- bmus H-0001 -->"}, f.comments(n), "exactly one comment")
	require.Equal(t, 1, f.count(post))
	f.mu.Lock()
	require.Equal(t, []string{"GET /repos/o/r/issues/1", "GET /user", "GET /repos/o/r/issues/1/comments", post, "GET /repos/o/r/issues/1/comments",
		"GET /repos/o/r/issues/1", "GET /repos/o/r/issues/1/comments"}, f.calls)
	f.mu.Unlock()

	// A POST that timed out before GitHub took it: two lookups find
	// nothing, and the timeout is the error, saying the comment may be
	// there.
	*l = lostAnswer{base: l.base, method: http.MethodPost, suffix: "/comments", n: 1}
	before := f.count("GET /repos/o/r/issues/1/comments")
	err := g.Comment(ctx, "#1", "Second.", tracker.Mark{Key: "H-0002", Since: time.Now()})
	require.ErrorContains(t, err, "POST /repos/o/r/issues/1/comments: ")
	require.ErrorContains(t, err, "Client.Timeout exceeded")
	require.ErrorContains(t, err, "(the comment may have been posted: GitHub lists none under its key yet)")
	require.Equal(t, before+3, f.count("GET /repos/o/r/issues/1/comments"), "before, and twice after")
	require.Len(t, f.comments(n), 1)
	require.NoError(t, g.Comment(ctx, "#1", "Second.", tracker.Mark{Key: "H-0002", Since: time.Now()}), "the agent's retry")
	require.Len(t, f.comments(n), 2)

	// A 4xx was refused: no lookup after it.
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	require.ErrorIs(t, g.Comment(ctx, "#9", "x", m), tracker.ErrNotFound)
	require.Equal(t, 0, f.count("GET /repos/o/r/issues/9/comments"))
}

// Only a marker at the end of a comment counts, since the text before it
// is the agent's; and only among the comments updated since the mark's
// Since, less an hour.
func TestCommentMarkerPlaceAndWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	n := f.add(fakeIssue{Title: "x", Comments: []fakeComment{
		{Body: "old\n\n<!-- bmus H-0001 -->", At: time.Now().Add(-3 * time.Hour)},
		{Body: "skewed\n\n<!-- bmus H-0002 -->", At: time.Now().Add(-30 * time.Minute)},
	}})
	g := f.adapter(t, testToken)

	require.NoError(t, g.Comment(ctx, "#1", "quoting <!-- bmus H-0009 --> here", tracker.Mark{Key: "H-0003", Since: time.Now()}))
	require.NoError(t, g.Comment(ctx, "#1", "new", tracker.Mark{Key: "H-0009", Since: time.Now()}), "a quoted marker is not one")
	require.NoError(t, g.Comment(ctx, "#1", "again", tracker.Mark{Key: "H-0001", Since: time.Now()}), "before the window")
	require.NoError(t, g.Comment(ctx, "#1", "skewed", tracker.Mark{Key: "H-0002", Since: time.Now()}), "inside the slack")
	require.Equal(t, []string{
		"old\n\n<!-- bmus H-0001 -->",
		"skewed\n\n<!-- bmus H-0002 -->",
		"quoting <!-- bmus H-0009 --> here\n\n<!-- bmus H-0003 -->",
		"new\n\n<!-- bmus H-0009 -->",
		"again\n\n<!-- bmus H-0001 -->",
	}, f.comments(n))

	require.ErrorContains(t, g.Comment(ctx, "#1", "x", tracker.Mark{Key: "a --> b"}), `idempotency key "a --> b"`)
	require.ErrorContains(t, g.Comment(ctx, "#1", "x", tracker.Mark{Key: "a\nb"}), "idempotency key")
	require.Len(t, f.comments(n), 5)
}

// The lookup reads every page of the comments: a marker on the last page
// is found.
func TestCommentLookupPages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	cs := []fakeComment{}
	for k := range 4 {
		cs = append(cs, fakeComment{Body: "c" + strconv.Itoa(k), At: time.Now()})
	}
	cs = append(cs, fakeComment{Body: "posted\n\n<!-- bmus H-0007 -->\n", At: time.Now()})
	n := f.add(fakeIssue{Title: "x", Comments: cs})
	g := f.adapter(t, testToken)

	require.NoError(t, g.Comment(ctx, "#1", "posted", tracker.Mark{Key: "H-0007", Since: time.Now()}))
	require.Len(t, f.comments(n), 5, "found on the third page of two")
	require.Equal(t, 3, f.count("GET /repos/o/r/issues/1/comments"))
	require.Equal(t, 0, f.count("POST /repos/o/r/issues/1/comments"))
}

// A lookup that fails refuses the write: a failed lookup must not turn
// into a second post or issue.
func TestLookupFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	n := f.add(fakeIssue{Title: "x"})
	f.broken = []string{"GET /repos/o/r/issues/1/comments", "GET /repos/o/r/issues"}
	g := f.adapter(t, testToken)

	err := g.Comment(ctx, "#1", "x", tracker.Mark{Key: "H-0001", Since: time.Now()})
	require.EqualError(t, err, "nothing posted: could not look for a comment already posted on o/r#1 under H-0001: "+
		"github: GET /repos/o/r/issues/1/comments: 500 Internal Server Error: Server Error")
	require.Empty(t, f.comments(n))
	_, err = g.Create(ctx, tracker.Item{Title: "t"}, tracker.Mark{Key: "finding ORD-0001/k", Since: time.Now()})
	require.EqualError(t, err, "nothing created: could not look for an issue already filed under finding ORD-0001/k: "+
		"github: GET /repos/o/r/issues: 500 Internal Server Error: Server Error")
	require.Equal(t, 0, f.count("POST /repos/o/r/issues/1/comments"))
	require.Equal(t, 0, f.count("POST /repos/o/r/issues"))

	// After a POST whose answer was lost, a lookup that fails too leaves
	// the POST's error, saying the comment may be there.
	f.mu.Lock()
	f.broken = nil
	f.mu.Unlock()
	var posted atomic.Bool
	base := f.srv.Client().Transport
	g, err = New(Config{Repo: "o/r", APIURL: f.srv.URL}, testToken, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost:
			resp, err := base.RoundTrip(r)
			if err != nil {
				return nil, err
			}
			_ = resp.Body.Close()
			posted.Store(true)
			return nil, timeoutError{}
		case posted.Load() && strings.HasSuffix(r.URL.Path, "/comments"):
			return nil, errors.New("connection reset")
		}
		return base.RoundTrip(r)
	})})
	require.NoError(t, err)
	err = g.Comment(ctx, "#1", "x", tracker.Mark{Key: "H-0001", Since: time.Now()})
	require.ErrorContains(t, err, "Client.Timeout exceeded")
	require.ErrorContains(t, err, "(the comment may have been posted: looking for it afterwards failed: github: GET /repos/o/r/issues/1/comments: ")
	require.ErrorContains(t, err, "connection reset)")
	require.Len(t, f.comments(n), 1, "one post, never a second")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Create under a key ends the body with its marker, and finds an issue
// already filed under it by listing the repo's issues since the mark's
// window, every page: GitHub took the first POST but the client timed
// out, and the issue found is taken, with its links. A retry creates
// nothing and links nothing twice. Bodies come back without the marker.
func TestCreateIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	epic := f.add(fakeIssue{Title: "epic"})
	blocker := f.add(fakeIssue{Title: "blocker"})
	f.add(fakeIssue{Title: "noise"})
	l := &lostAnswer{method: http.MethodPost, suffix: "/repos/o/r/issues", sent: true, n: 1}
	g := f.lossy(t, l)
	key := "finding ORD-0001/flaky"
	m := tracker.Mark{Key: key, Since: time.Now()}
	in := tracker.Item{Title: "Flaky test", Body: "It flakes.", Parent: "#1", BlockedBy: []string{"#2"}}

	want := tracker.Item{Ref: "o/r#4", Title: "Flaky test", Body: "It flakes.", Labels: []string{}, Parent: "o/r#1", State: "open",
		BlockedBy: []string{"o/r#2"}}
	it, err := g.Create(ctx, in, m)
	require.NoError(t, err, "the timeout, then the issue found")
	require.Equal(t, want, it)
	it, err = g.Create(ctx, in, m)
	require.NoError(t, err, "a retry")
	require.Equal(t, want, it)
	require.Equal(t, 1, f.count("POST /repos/o/r/issues"))
	require.Equal(t, 1, f.count("POST /repos/o/r/issues/1/sub_issues"))
	require.Equal(t, 1, f.count("POST /repos/o/r/issues/4/dependencies/blocked_by"))
	require.Equal(t, 2, f.count("GET /repos/o/r/issues/4/dependencies/blocked_by"), "the retry checked the link")
	f.mu.Lock()
	require.Len(t, f.issues, 4)
	require.Equal(t, "It flakes.\n\n<!-- bmus finding ORD-0001/flaky -->", f.issue(4).Body)
	require.Equal(t, epic, f.issue(4).Parent)
	require.Equal(t, []int{blocker}, f.issue(4).BlockedBy)
	f.mu.Unlock()
	got, err := g.Get(ctx, "#4")
	require.NoError(t, err)
	require.Equal(t, "It flakes.", got.Body, "the marker is stripped")

	// An issue filed under the key by a run that crashed before linking
	// it gets its links, and no twin.
	half := f.add(fakeIssue{Title: "half", Body: "<!-- bmus finding ORD-0001/half -->"})
	it, err = g.Create(ctx, tracker.Item{Title: "half", Parent: "#1"}, tracker.Mark{Key: "finding ORD-0001/half", Since: time.Now()})
	require.NoError(t, err)
	require.Equal(t, "o/r#"+strconv.Itoa(half), it.Ref)
	require.Equal(t, "", it.Body)
	f.mu.Lock()
	require.Len(t, f.issues, half)
	require.Equal(t, epic, f.issue(half).Parent)
	f.mu.Unlock()

	// An issue outside the window, or whose marker isn't at the end, is
	// not the one: Create files a new one.
	f.add(fakeIssue{Title: "old", Body: "<!-- bmus K-1 -->", Updated: time.Now().Add(-2 * time.Hour)})
	f.add(fakeIssue{Title: "quotes", Body: "<!-- bmus K-2 --> quoted"})
	it, err = g.Create(ctx, tracker.Item{Title: "k1"}, tracker.Mark{Key: "K-1", Since: time.Now()})
	require.NoError(t, err)
	require.Equal(t, "o/r#8", it.Ref)
	it, err = g.Create(ctx, tracker.Item{Title: "k2"}, tracker.Mark{Key: "K-2", Since: time.Now()})
	require.NoError(t, err)
	require.Equal(t, "o/r#9", it.Ref)
	f.mu.Lock()
	require.Equal(t, "<!-- bmus K-1 -->", f.issue(8).Body, "an empty body is the marker alone")
	f.mu.Unlock()
}

func TestStripMarker(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"text\n\n<!-- bmus H-0001 -->":          "text",
		"text\r\n\r\n<!-- bmus H-0001 -->\n":    "text",
		"<!-- bmus finding ORD-0001/k -->":      "",
		"text <!-- bmus H-0001 --> more":        "text <!-- bmus H-0001 --> more",
		"<!-- bmus H-0001 -->\n\nthe real text": "<!-- bmus H-0001 -->\n\nthe real text",
		"text\n\n<!-- other -->":                "text\n\n<!-- other -->",
	} {
		require.Equal(t, want, stripMarker(in), in)
	}
	require.Equal(t, "a\n\n<!-- bmus k -->", withMarker("a", "k"))
	require.Equal(t, "a", withMarker("a", ""))
}

// After a lost answer, a first lookup that misses the comment (GitHub
// lists it a moment later) is followed by a second, after the settle
// wait, which finds it.
func TestRecheckAfterSettle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	n := f.add(fakeIssue{Title: "x"})
	g := f.lossy(t, &lostAnswer{method: http.MethodPost, suffix: "/comments", sent: true, n: 1})
	f.mu.Lock()
	f.lag = 1 // the first lookup after the post
	f.mu.Unlock()
	require.NoError(t, g.Comment(ctx, "#1", "x", tracker.Mark{Key: "H-0001", Since: time.Now()}))
	require.Len(t, f.comments(n), 1)
	require.Equal(t, 3, f.count("GET /repos/o/r/issues/1/comments"))

	// The settle wait ends with the context.
	g.settle = time.Hour
	cctx, cancel := context.WithCancel(ctx)
	looks := 0
	err := g.recheck(cctx, errors.New("lost"), "the comment may have been posted", func() (bool, error) {
		looks++
		cancel()
		return false, nil
	})
	require.EqualError(t, err, "lost (the comment may have been posted: context canceled before looking for it again)")
	require.Equal(t, 1, looks)
}

// Only the token's own account's markers count: an outsider who plants
// the next hail's marker doesn't stop its comment or issue, nor does the
// bridge's own marker under another database's nonce.
func TestMarkerAuthor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	n := f.add(fakeIssue{Title: "x", Comments: []fakeComment{
		{Body: "lol\n\n<!-- bmus H-0043 n1 -->", At: time.Now(), Author: 7},
		{Body: "mine\n\n<!-- bmus H-0044 n1 -->", At: time.Now()},
	}})
	f.add(fakeIssue{Title: "planted", Body: "<!-- bmus finding ORD-1/k n1 -->", Author: 7})
	g := f.adapter(t, testToken)
	now := time.Now()

	require.NoError(t, g.Comment(ctx, "#1", "Real status update", tracker.Mark{Key: "H-0043 n1", Since: now}), "a planted marker")
	require.NoError(t, g.Comment(ctx, "#1", "mine", tracker.Mark{Key: "H-0044 n1", Since: now}), "the bridge's own marker")
	require.NoError(t, g.Comment(ctx, "#1", "mine", tracker.Mark{Key: "H-0044 n2", Since: now}), "another database's nonce")
	require.Equal(t, []string{"lol\n\n<!-- bmus H-0043 n1 -->", "mine\n\n<!-- bmus H-0044 n1 -->",
		"Real status update\n\n<!-- bmus H-0043 n1 -->", "mine\n\n<!-- bmus H-0044 n2 -->"}, f.comments(n))
	it, err := g.Create(ctx, tracker.Item{Title: "finding"}, tracker.Mark{Key: "finding ORD-1/k n1", Since: now})
	require.NoError(t, err)
	require.Equal(t, "o/r#3", it.Ref, "a new issue, not the planted one")
	require.Equal(t, 1, f.count("GET /user"), "the account is read once")

	// A Mark with a key needs its Since.
	_, err = g.Create(ctx, tracker.Item{Title: "x"}, tracker.Mark{Key: "K-1"})
	require.ErrorContains(t, err, `idempotency key "K-1" has no Since`)
	require.ErrorContains(t, g.Comment(ctx, "#1", "x", tracker.Mark{Key: "K-1"}), "has no Since")
	f.mu.Lock()
	require.Len(t, f.issues, 3)
	f.mu.Unlock()
}

// GET /user fails for a GitHub App installation token: markers are then
// taken from any author. Any other failure refuses the write, and the
// next write reads the account again.
func TestWhoami(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeAPI(t)
	n := f.add(fakeIssue{Title: "x", Comments: []fakeComment{{Body: "<!-- bmus H-0001 n -->", At: time.Now(), Author: 7}}})
	f.broken = []string{"GET /user"}
	g := f.adapter(t, testToken)
	err := g.Comment(ctx, "#1", "x", tracker.Mark{Key: "H-0002 n", Since: time.Now()})
	require.ErrorContains(t, err, "nothing posted: could not look for a comment already posted on o/r#1 under H-0002 n: github: GET /user: 500 ")
	f.mu.Lock()
	f.broken = nil
	f.mu.Unlock()
	require.NoError(t, g.Comment(ctx, "#1", "x", tracker.Mark{Key: "H-0002 n", Since: time.Now()}))
	require.Len(t, f.comments(n), 2)

	f.mu.Lock()
	f.appToken = true
	f.mu.Unlock()
	app := f.adapter(t, testToken)
	self, checked, err := app.whoami(ctx)
	require.NoError(t, err)
	require.False(t, checked)
	require.Zero(t, self)
	require.NoError(t, app.Comment(ctx, "#1", "x", tracker.Mark{Key: "H-0001 n", Since: time.Now()}), "any author's marker")
	require.Len(t, f.comments(n), 2)
	require.Equal(t, 3, f.count("GET /user"), "failed, read, and once for the app token")
}
