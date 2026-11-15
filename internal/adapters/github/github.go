// Package github is the GitHub Issues tracker adapter (#54), registered as
// "github" in tracker.yaml:
//
//	adapter: github
//	github:
//	  repo: owner/name        # required: the one repository it works on
//	  token_env: GITHUB_TOKEN # optional: the variable holding a token
//	  api_url: https://api.github.com # optional, for GitHub Enterprise
//
// It runs on the bridge with the bridge's own credentials, never a beam's
// (D6): the token in token_env when that is set and non-empty, else the
// output of `gh auth token --hostname <host>` (github.com for
// api.github.com, else api_url's host), read once when the bridge starts.
// An http api_url is only taken for a loopback host.
//
// Refs are "owner/name#<number>"; "#<number>" and "<number>" name an issue
// of the configured repo. Refs to any other repository are refused
// (tracker.ErrBadRef), so a request from a beam can only touch that repo.
// Parent uses GitHub's sub-issues API; BlockedBy its issue dependencies
// API, listing open blockers only, and nil where the API isn't available.
// A sub-issue or blocker in another repository keeps its own ref, and its
// BlockedBy is nil. Pull requests are left out of List and refused by Get,
// Comment, Close and Unlabel.
//
// Comment and Create take a tracker.Mark (#206). Its key goes at the end
// of the body as a hidden line, "<!-- bmus <key> -->", which GitHub keeps
// in the raw markdown and hides when it renders it. Before writing, the
// adapter lists what changed since the mark's Since (less an hour, for
// clock skew) and writes nothing when a comment or issue the token's own
// account wrote already ends with the marker. A write whose outcome is
// unknown (a timeout, a 5xx) is looked up again the same way, and once
// more a moment later; a marker found counts as done. A lookup that fails
// refuses the write: it never risks a second post. Item bodies come back
// without a trailing marker.
//
// The account is read once, from GET /user. A GitHub App installation
// token can't read it: markers are then taken from any author, and the
// adapter logs a warning saying so.
package github

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tscolari/bmus/internal/tracker"
)

// Name is the adapter's name in tracker.yaml.
const Name = "github"

// DefaultAPIURL is api_url's default.
const DefaultAPIURL = "https://api.github.com"

func init() {
	tracker.Register(Name, open)
}

// Config is tracker.yaml's github section.
type Config struct {
	Repo     string `yaml:"repo"`
	TokenEnv string `yaml:"token_env"`
	APIURL   string `yaml:"api_url"`
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Check validates c and fills in its default.
func (c *Config) Check() error {
	if !repoRe.MatchString(c.Repo) {
		return fmt.Errorf("repo %q: want owner/name", c.Repo)
	}
	if c.APIURL == "" {
		c.APIURL = DefaultAPIURL
	}
	u, err := url.Parse(c.APIURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("api_url %q: want an http(s) URL", c.APIURL)
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return fmt.Errorf("api_url %q: http is only for a loopback host; the token would go in the clear", c.APIURL)
	}
	c.APIURL = strings.TrimSuffix(c.APIURL, "/")
	return nil
}

// loopback reports whether host is localhost or a loopback address.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// GHHost is the host gh keeps the token for api_url under: github.com for
// api.github.com, else api_url's host (a GitHub Enterprise server).
func GHHost(apiURL string) string {
	u, err := url.Parse(apiURL)
	if err != nil || u.Host == "" || strings.EqualFold(u.Hostname(), "api.github.com") {
		return "github.com"
	}
	return u.Host
}

func open(section *yaml.Node) (tracker.Tracker, error) {
	if section == nil {
		return nil, errors.New("needs a github section with repo: owner/name")
	}
	var c Config
	if err := tracker.DecodeSection(section, &c); err != nil {
		return nil, err
	}
	if err := c.Check(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	host := GHHost(c.APIURL)
	token, err := ResolveToken(ctx, c.TokenEnv, os.Getenv, func(ctx context.Context) (string, error) { return ghAuthToken(ctx, host) })
	if err != nil {
		return nil, err
	}
	g, err := New(c, token, nil)
	if err != nil {
		return nil, err
	}
	// Read the account now, so a token that can't name it is reported at
	// startup. Any other failure is left for the first keyed write.
	if _, _, err := g.whoami(ctx); err != nil {
		slog.Warn("tracker: could not read the GitHub account yet; the first comment will try again", "err", err)
	}
	return g, nil
}

// ResolveToken is the bridge's token: the value of variable env when env
// is set and the variable isn't empty, else what gh returns (`gh auth
// token --hostname <GHHost>`).
func ResolveToken(ctx context.Context, env string, getenv func(string) string, gh func(context.Context) (string, error)) (string, error) {
	if env != "" {
		if v := strings.TrimSpace(getenv(env)); v != "" {
			return v, nil
		}
	}
	token, err := gh(ctx)
	if err == nil && token != "" {
		return token, nil
	}
	if err == nil {
		err = errors.New("it printed no token")
	}
	if env != "" {
		return "", fmt.Errorf("no token: $%s is empty, and gh auth token failed: %w", env, err)
	}
	return "", fmt.Errorf("no token: set token_env, or log gh in (gh auth token failed: %w)", err)
}

// ghAuthToken runs `gh auth token --hostname host`.
func ghAuthToken(ctx context.Context, host string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", host)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// GitHub is the adapter.
type GitHub struct {
	cfg    Config
	token  string
	client *http.Client
	// settle is how long a write whose outcome is unknown waits before
	// its second lookup (recheck).
	settle time.Duration

	// selfMu guards self and unchecked, whoami's answer once known.
	selfMu    sync.Mutex
	self      *account
	unchecked bool
}

// account is a GitHub user, as GET /user and a comment's user give it.
type account struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

var _ tracker.Tracker = (*GitHub)(nil)

// New is the adapter for c (checked here) with token. A nil client is a
// client with a 30s timeout. The adapter uses a copy of client whose
// redirect policy is its own (noWriteRedirects).
func New(c Config, token string, client *http.Client) (*GitHub, error) {
	if err := c.Check(); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("no token")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	cl := *client
	cl.CheckRedirect = noWriteRedirects(c.APIURL)
	return &GitHub{cfg: c, token: token, client: &cl, settle: 2 * time.Second}, nil
}

// whoami is the token's account, read once with GET /user. A 403 there is
// a GitHub App installation token, which has no user: checked is then
// false, markers are taken from any author, and a warning says so once.
// Any other failure is returned and not kept, so the next call tries
// again.
func (g *GitHub) whoami(ctx context.Context) (self account, checked bool, err error) {
	g.selfMu.Lock()
	defer g.selfMu.Unlock()
	switch {
	case g.self != nil:
		return *g.self, true, nil
	case g.unchecked:
		return account{}, false, nil
	}
	var a account
	_, err = g.do(ctx, http.MethodGet, "/user", nil, nil, &a)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusForbidden && !errors.Is(err, tracker.ErrRateLimited) {
		g.unchecked = true
		slog.Warn("tracker: the GitHub token has no user account (a GitHub App installation token?): "+
			"idempotency markers are taken from any author", "repo", g.cfg.Repo, "err", err)
		return account{}, false, nil
	}
	if err != nil {
		return account{}, false, err
	}
	if a.ID == 0 {
		return account{}, false, errors.New("github: GET /user: no account id")
	}
	g.self = &a
	return a, true, nil
}

// maxRedirects bounds the redirects a GET follows.
const maxRedirects = 5

// noWriteRedirects is the adapter's redirect policy. GitHub answers 301
// for a transferred issue or a renamed repository, and Go would turn a
// 301 on a POST or PATCH into a GET, or replay a 307 or 308 one, body and
// token included, to wherever it points: a close could land on another
// repository's issue, or report success having closed nothing. So only a
// GET follows a redirect, and only to a URL under apiURL; any other
// redirect comes back as the 3xx itself, which do reports as an error.
func noWriteRedirects(apiURL string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 || via[0].Method != http.MethodGet || len(via) > maxRedirects ||
			!strings.HasPrefix(req.URL.String(), apiURL+"/") {
			return http.ErrUseLastResponse
		}
		return nil
	}
}

// Name is "github".
func (g *GitHub) Name() string { return Name }

// ---- refs ----

var refRe = regexp.MustCompile(`^(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#?([1-9][0-9]{0,9})$`)

// number is ref's issue number in the configured repo.
func (g *GitHub) number(ref string) (int, error) {
	m := refRe.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil || (m[1] != "" && !strings.Contains(ref, "#")) {
		return 0, fmt.Errorf("%w: %q is not owner/name#<number>, #<number> or <number>", tracker.ErrBadRef, ref)
	}
	if m[1] != "" && !strings.EqualFold(m[1], g.cfg.Repo) {
		return 0, fmt.Errorf("%w: %q is not in %s, the repo the tracker is configured for", tracker.ErrBadRef, ref, g.cfg.Repo)
	}
	return strconv.Atoi(m[2])
}

func (g *GitHub) ref(n int) string { return g.cfg.Repo + "#" + strconv.Itoa(n) }

// Canon is ref as owner/name#<number>, in the configured repo.
func (g *GitHub) Canon(ref string) (string, error) {
	n, err := g.number(ref)
	if err != nil {
		return "", err
	}
	return g.ref(n), nil
}

// ---- the API ----

type issue struct {
	ID          int64           `json:"id"`
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        *string         `json:"body"`
	State       string          `json:"state"`
	Labels      []label         `json:"labels"`
	Milestone   *milestone      `json:"milestone"`
	PullRequest json.RawMessage `json:"pull_request,omitempty"`
	// RepositoryURL is https://api.github.com/repos/<owner>/<name>.
	RepositoryURL string `json:"repository_url"`
	// User is the issue's author.
	User *account `json:"user"`
	// Deps is GitHub's count of the issue's dependencies, when it sends
	// one: no blockers saves the dependencies call.
	Deps *struct {
		BlockedBy int `json:"blocked_by"`
	} `json:"issue_dependencies_summary"`
}

type label struct {
	Name string `json:"name"`
}

type milestone struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

// repoOf is the owner/name of i's repository, from its repository_url;
// "" when it has none.
func repoOf(i issue) string {
	_, repo, ok := strings.Cut(i.RepositoryURL, "/repos/")
	if !ok {
		return ""
	}
	return strings.Trim(repo, "/")
}

// local reports whether i is in the configured repo. An issue with no
// repository_url is taken as local: every endpoint the adapter calls
// for one is under the repo.
func (g *GitHub) local(i issue) bool {
	repo := repoOf(i)
	return repo == "" || strings.EqualFold(repo, g.cfg.Repo)
}

// refOf is i's ref: owner/name#N, with the configured repo's spelling when
// i is in it.
func (g *GitHub) refOf(i issue) string {
	if g.local(i) {
		return g.ref(i.Number)
	}
	return repoOf(i) + "#" + strconv.Itoa(i.Number)
}

// item is i as a tracker item, with no BlockedBy.
func (g *GitHub) item(i issue) tracker.Item {
	it := tracker.Item{Ref: g.refOf(i), Title: i.Title, State: i.State, Labels: []string{}}
	if i.Body != nil {
		it.Body = stripMarker(*i.Body)
	}
	for _, l := range i.Labels {
		it.Labels = append(it.Labels, l.Name)
	}
	if i.Milestone != nil {
		it.Milestone = i.Milestone.Title
	}
	return it
}

// apiError is a non-2xx answer.
type apiError struct {
	Method, Path string
	Status       int
	Message      string
	sentinel     error
}

func (e *apiError) Error() string {
	msg := fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func (e *apiError) Unwrap() error { return e.sentinel }

// do sends one request to path (under the API URL, or an absolute URL
// under it, a Link header's) with query and in as its JSON body, and
// decodes the answer into out. It returns the answer's headers.
func (g *GitHub) do(ctx context.Context, method, path string, query url.Values, in, out any) (http.Header, error) {
	u := path
	if !strings.HasPrefix(path, g.cfg.APIURL+"/") {
		u = g.cfg.APIURL + path
	}
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "bmus-bridge")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, strings.TrimPrefix(path, g.cfg.APIURL), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, strings.TrimPrefix(path, g.cfg.APIURL), err)
	}
	if resp.StatusCode/100 != 2 {
		e := &apiError{Method: method, Path: strings.TrimPrefix(path, g.cfg.APIURL), Status: resp.StatusCode}
		var msg struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &msg) == nil {
			e.Message = msg.Message
		}
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			e.sentinel = tracker.ErrNotFound
		case resp.StatusCode == http.StatusUnauthorized:
			e.sentinel = tracker.ErrAuth
		case resp.StatusCode == http.StatusTooManyRequests, rateLimited(resp, e.Message):
			e.sentinel = tracker.ErrRateLimited
		case resp.StatusCode == http.StatusForbidden:
			e.sentinel = tracker.ErrAuth
		}
		return resp.Header, e
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("github: %s %s: decode: %w", method, strings.TrimPrefix(path, g.cfg.APIURL), err)
		}
	}
	return resp.Header, nil
}

// rateLimited reports whether a 403 is GitHub's rate limit: the primary
// one (X-RateLimit-Remaining 0) or a secondary one (a Retry-After header,
// or a message naming it). Any other 403 is a permission error.
func rateLimited(resp *http.Response, msg string) bool {
	return resp.StatusCode == http.StatusForbidden && (resp.Header.Get("X-RateLimit-Remaining") == "0" ||
		resp.Header.Get("Retry-After") != "" || strings.Contains(strings.ToLower(msg), "rate limit"))
}

var nextRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// maxPages bounds a listing: 100 pages of 100.
const maxPages = 100

// getAll GETs every page of a list at path.
func getAll[T any](ctx context.Context, g *GitHub, path string, query url.Values) ([]T, error) {
	var all []T
	err := eachPage(ctx, g, path, query, func(page []T) bool {
		all = append(all, page...)
		return false
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// eachPage GETs the pages of a list at path in order, handing each to
// fn, until fn reports it has enough or there is no next page.
func eachPage[T any](ctx context.Context, g *GitHub, path string, query url.Values, fn func([]T) (enough bool)) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("per_page", "100")
	for range maxPages {
		var page []T
		h, err := g.do(ctx, http.MethodGet, path, query, nil, &page)
		if err != nil {
			return err
		}
		if fn(page) {
			return nil
		}
		m := nextRe.FindStringSubmatch(h.Get("Link"))
		// Only follow links under the API URL: the token goes with them.
		if m == nil || !strings.HasPrefix(m[1], g.cfg.APIURL+"/") {
			return nil
		}
		path, query = m[1], nil
	}
	return fmt.Errorf("github: %s: more than %d pages", path, maxPages)
}

func (g *GitHub) repoPath() string { return "/repos/" + g.cfg.Repo }

func (g *GitHub) getIssue(ctx context.Context, n int) (issue, error) {
	var i issue
	if _, err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d", g.repoPath(), n), nil, nil, &i); err != nil {
		return issue{}, err
	}
	if i.PullRequest != nil && string(i.PullRequest) != "null" {
		return issue{}, fmt.Errorf("%s is a pull request, not an issue: %w", g.ref(n), tracker.ErrNotFound)
	}
	// A redirect followed to another issue (a transfer) is not this one.
	if !g.local(i) || i.Number != n {
		return issue{}, fmt.Errorf("%s now answers as %s: %w", g.ref(n), g.refOf(i), tracker.ErrNotFound)
	}
	return i, nil
}

// blockedBy is the refs of i's open blockers: none when GitHub's summary
// counts none, nil when the dependencies API isn't there (404) or i is in
// another repository (a parent's sub-issue may be), whose endpoints the
// adapter doesn't call.
func (g *GitHub) blockedBy(ctx context.Context, i issue) ([]string, error) {
	if !g.local(i) {
		return nil, nil
	}
	if i.Deps != nil && i.Deps.BlockedBy == 0 {
		return []string{}, nil
	}
	blockers, err := getAll[issue](ctx, g, fmt.Sprintf("%s/issues/%d/dependencies/blocked_by", g.repoPath(), i.Number), nil)
	if errors.Is(err, tracker.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, b := range blockers {
		if b.State == tracker.StateOpen {
			out = append(out, g.refOf(b))
		}
	}
	return out, nil
}

// Get reads issue ref with its open blockers.
func (g *GitHub) Get(ctx context.Context, ref string) (tracker.Item, error) {
	n, err := g.number(ref)
	if err != nil {
		return tracker.Item{}, err
	}
	i, err := g.getIssue(ctx, n)
	if err != nil {
		return tracker.Item{}, err
	}
	it := g.item(i)
	if it.BlockedBy, err = g.blockedBy(ctx, i); err != nil {
		return tracker.Item{}, err
	}
	return it, nil
}

// milestoneNumber is the number of the milestone titled title, 0 when
// there is none.
func (g *GitHub) milestoneNumber(ctx context.Context, title string) (int, error) {
	ms, err := getAll[milestone](ctx, g, g.repoPath()+"/milestones", url.Values{"state": {"all"}})
	if err != nil {
		return 0, err
	}
	for _, m := range ms {
		if m.Title == title {
			return m.Number, nil
		}
	}
	return 0, nil
}

// List reads the issues q fits, each with its open blockers. A parent's
// sub-issues come from the sub-issues API and are filtered here; other
// queries filter on GitHub's side. Pull requests are left out. With a
// Limit, paging stops once that many issues fit, and only those have
// their blockers read.
func (g *GitHub) List(ctx context.Context, q tracker.Query) ([]tracker.Item, error) {
	switch q.State {
	case "", tracker.StateOpen, tracker.StateClosed, tracker.StateAll:
	default:
		return nil, fmt.Errorf("unknown state %q (want open, closed or all)", q.State)
	}
	if err := tracker.CheckLimit(q.Limit); err != nil {
		return nil, err
	}
	var (
		path   string
		query  url.Values
		parent string
	)
	if q.Parent != "" {
		n, err := g.number(q.Parent)
		if err != nil {
			return nil, err
		}
		parent = g.ref(n)
		path = fmt.Sprintf("%s/issues/%d/sub_issues", g.repoPath(), n)
	} else {
		query = url.Values{"state": {cmp.Or(q.State, tracker.StateOpen)}}
		if len(q.Labels) > 0 {
			query.Set("labels", strings.Join(q.Labels, ","))
		}
		if q.Milestone != "" {
			m, err := g.milestoneNumber(ctx, q.Milestone)
			if err != nil {
				return nil, err
			}
			if m == 0 {
				return nil, fmt.Errorf("milestone %q: %w", q.Milestone, tracker.ErrNotFound)
			}
			query.Set("milestone", strconv.Itoa(m))
		}
		path = g.repoPath() + "/issues"
	}
	var kept []issue
	err := eachPage(ctx, g, path, query, func(page []issue) bool {
		for _, i := range page {
			if i.PullRequest != nil && string(i.PullRequest) != "null" {
				continue
			}
			it := g.item(i)
			it.Parent = parent
			if !tracker.Fits(it, q) {
				continue
			}
			kept = append(kept, i)
			if len(kept) == q.Limit {
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	out := []tracker.Item{}
	for _, i := range kept {
		it := g.item(i)
		it.Parent = parent
		if it.BlockedBy, err = g.blockedBy(ctx, i); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// Create files an issue with its title, body, labels and milestone (by
// title, which must exist), then makes it a sub-issue of it.Parent and
// blocked by each of it.BlockedBy. The parent, the blockers and the
// milestone are read first, so a missing one fails before anything is
// created. When a later call fails, the issue already exists: the item
// comes back with its Ref and the error.
//
// Last, the issue must carry every label asked for: GitHub drops labels
// silently for a token without push access. One that lacks any comes back
// with its Ref and an error wrapping tracker.ErrLabels that names them,
// after its links are added.
//
// With a key, the body ends with its marker. An issue already filed under
// it (filed) is taken instead of a new one, and only its missing links
// are added. Its labels are checked the same way, so a retry reports the
// labels the first attempt lost.
func (g *GitHub) Create(ctx context.Context, it tracker.Item, m tracker.Mark) (tracker.Item, error) {
	if strings.TrimSpace(it.Title) == "" {
		return tracker.Item{}, errors.New("an issue needs a title")
	}
	if err := m.Check(); err != nil {
		return tracker.Item{}, err
	}
	var parent *issue
	if it.Parent != "" {
		n, err := g.number(it.Parent)
		if err != nil {
			return tracker.Item{}, err
		}
		p, err := g.getIssue(ctx, n)
		if err != nil {
			return tracker.Item{}, fmt.Errorf("parent %s: %w", g.ref(n), err)
		}
		parent = &p
	}
	blockers := make([]issue, len(it.BlockedBy))
	for k, b := range it.BlockedBy {
		n, err := g.number(b)
		if err != nil {
			return tracker.Item{}, err
		}
		if blockers[k], err = g.getIssue(ctx, n); err != nil {
			return tracker.Item{}, fmt.Errorf("blocker %s: %w", g.ref(n), err)
		}
	}
	in := map[string]any{"title": it.Title, "body": withMarker(it.Body, m.Key)}
	if len(it.Labels) > 0 {
		in["labels"] = it.Labels
	}
	if it.Milestone != "" {
		m, err := g.milestoneNumber(ctx, it.Milestone)
		if err != nil {
			return tracker.Item{}, err
		}
		if m == 0 {
			return tracker.Item{}, fmt.Errorf("milestone %q: %w", it.Milestone, tracker.ErrNotFound)
		}
		in["milestone"] = m
	}
	var (
		created issue
		found   bool
	)
	if m.Key != "" {
		var err error
		if created, found, err = g.filed(ctx, m); err != nil {
			return tracker.Item{}, fmt.Errorf("nothing created: could not look for an issue already filed under %s: %w", m.Key, err)
		}
	}
	if !found {
		_, err := g.do(ctx, http.MethodPost, g.repoPath()+"/issues", nil, in, &created)
		if err != nil && m.Key != "" && mayHaveLanded(err) {
			if err := g.recheck(ctx, err, "the issue may have been created", func() (ok bool, err error) {
				created, ok, err = g.filed(ctx, m)
				return ok, err
			}); err != nil {
				return tracker.Item{}, err
			}
			found = true
		} else if err != nil {
			return tracker.Item{}, err
		}
	}
	// link adds a link at path, unless an issue found under the key has
	// it already.
	link := func(path string, id int64, body map[string]any) error {
		if found {
			if ok, err := g.linked(ctx, path, id); err != nil || ok {
				return err
			}
		}
		_, err := g.do(ctx, http.MethodPost, path, nil, body, nil)
		return err
	}
	out := g.item(created)
	if parent != nil {
		out.Parent = g.ref(parent.Number)
		if err := link(fmt.Sprintf("%s/issues/%d/sub_issues", g.repoPath(), parent.Number), created.ID,
			map[string]any{"sub_issue_id": created.ID}); err != nil {
			return out, fmt.Errorf("%s created, but not as a sub-issue of %s: %w", out.Ref, out.Parent, err)
		}
	}
	out.BlockedBy = []string{}
	for _, b := range blockers {
		if err := link(fmt.Sprintf("%s/issues/%d/dependencies/blocked_by", g.repoPath(), created.Number), b.ID,
			map[string]any{"issue_id": b.ID}); err != nil {
			return out, fmt.Errorf("%s created, but not blocked by %s: %w", out.Ref, g.ref(b.Number), err)
		}
		if b.State == tracker.StateOpen {
			out.BlockedBy = append(out.BlockedBy, g.ref(b.Number))
		}
	}
	if missing := tracker.MissingLabels(it.Labels, out.Labels); len(missing) > 0 {
		return out, fmt.Errorf("%s created without the labels %s (GitHub drops labels for a token without push access): %w",
			out.Ref, strings.Join(missing, ", "), tracker.ErrLabels)
	}
	return out, nil
}

// ---- idempotency markers (#206) ----

// sinceSlack widens a Mark's lookup window, for clock skew between the
// bridge and GitHub.
const sinceSlack = time.Hour

// marker is the hidden line a write under key ends with.
func marker(key string) string { return "<!-- bmus " + key + " -->" }

// withMarker is body with key's marker appended after a blank line; body
// itself when key is empty.
func withMarker(body, key string) string {
	switch {
	case key == "":
		return body
	case body == "":
		return marker(key)
	}
	return body + "\n\n" + marker(key)
}

// hasMarker reports whether body ends with key's marker, trailing
// whitespace aside. Only the end counts: the text before it is a beam's,
// which may quote another hail's marker to pass a later write as done.
func hasMarker(body, key string) bool {
	return strings.HasSuffix(strings.TrimRight(body, " \t\r\n"), marker(key))
}

var markerRe = regexp.MustCompile(`(?:\r?\n\r?\n)?<!-- bmus [A-Za-z0-9._:/ -]+ -->[ \t\r\n]*$`)

// stripMarker is body without a trailing marker and the blank line before
// it: the text as its writer gave it.
func stripMarker(body string) string { return markerRe.ReplaceAllString(body, "") }

// sinceQuery is a listing's query for m: since m.Since less sinceSlack.
// Mark.Check refuses a key without a Since.
func sinceQuery(m tracker.Mark) url.Values {
	return url.Values{"since": {m.Since.Add(-sinceSlack).UTC().Format(time.RFC3339)}}
}

// mayHaveLanded reports whether a write that failed with err may have
// reached GitHub all the same: a transport error (a timeout, say), an
// answer that didn't decode, or a 5xx. Any other status was refused.
func mayHaveLanded(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status >= 500
	}
	return true
}

type comment struct {
	Body string   `json:"body"`
	User *account `json:"user"`
}

// mine reports whether the account whoami read wrote what by is the
// author of: always, when whoami couldn't read one.
func mine(self account, checked bool, by *account) bool {
	return !checked || (by != nil && by.ID == self.ID)
}

// commented reports whether issue n has a comment the token's account
// wrote (whoami) ending with m's marker, among those updated in m's
// window, every page of them. Anyone else's marker is ignored: hail ids
// are sequential, and an outsider could plant the next one's.
func (g *GitHub) commented(ctx context.Context, n int, m tracker.Mark) (bool, error) {
	self, checked, err := g.whoami(ctx)
	if err != nil {
		return false, err
	}
	cs, err := getAll[comment](ctx, g, fmt.Sprintf("%s/issues/%d/comments", g.repoPath(), n), sinceQuery(m))
	if err != nil {
		return false, err
	}
	for _, c := range cs {
		if hasMarker(c.Body, m.Key) && mine(self, checked, c.User) {
			return true, nil
		}
	}
	return false, nil
}

// recheck settles a write that failed with err, its outcome unknown:
// look reports whether it landed, read at once and, when that finds
// nothing, once more after g.settle, as GitHub may list a write a moment
// after taking it. Found is done (nil). Otherwise err comes back with
// maybe ("the comment may have been posted"), so whoever reads the
// refusal knows the write may be there.
func (g *GitHub) recheck(ctx context.Context, err error, maybe string, look func() (bool, error)) error {
	for k := range 2 {
		if k == 1 {
			t := time.NewTimer(g.settle)
			select {
			case <-ctx.Done():
				t.Stop()
				return fmt.Errorf("%w (%s: %v before looking for it again)", err, maybe, ctx.Err())
			case <-t.C:
			}
		}
		found, lerr := look()
		if lerr != nil {
			return fmt.Errorf("%w (%s: looking for it afterwards failed: %v)", err, maybe, lerr)
		}
		if found {
			return nil
		}
	}
	return fmt.Errorf("%w (%s: GitHub lists none under its key yet)", err, maybe)
}

// filed is the issue filed under m's key, if any: the repo's issues
// updated in m's window, open or closed, every page of them, that the
// token's account wrote and whose body ends with m's marker. Listing reads what GitHub has committed; the
// search API would add its own rate limit and an indexing lag that could
// hide an issue filed moments before a retry.
func (g *GitHub) filed(ctx context.Context, m tracker.Mark) (issue, bool, error) {
	self, checked, err := g.whoami(ctx)
	if err != nil {
		return issue{}, false, err
	}
	q := sinceQuery(m)
	q.Set("state", tracker.StateAll)
	issues, err := getAll[issue](ctx, g, g.repoPath()+"/issues", q)
	if err != nil {
		return issue{}, false, err
	}
	for _, i := range issues {
		if (i.PullRequest == nil || string(i.PullRequest) == "null") && g.local(i) && i.Body != nil && hasMarker(*i.Body, m.Key) &&
			mine(self, checked, i.User) {
			return i, true, nil
		}
	}
	return issue{}, false, nil
}

// linked reports whether the list of issues at path holds the one with
// id: a sub-issue already under its parent, a blocker already linked.
func (g *GitHub) linked(ctx context.Context, path string, id int64) (bool, error) {
	issues, err := getAll[issue](ctx, g, path, nil)
	if err != nil {
		return false, err
	}
	for _, i := range issues {
		if i.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// target is the number of issue ref, read first so a pull request (or a
// missing issue) is refused before anything is written.
func (g *GitHub) target(ctx context.Context, ref string) (int, error) {
	n, err := g.number(ref)
	if err != nil {
		return 0, err
	}
	if _, err := g.getIssue(ctx, n); err != nil {
		return 0, err
	}
	return n, nil
}

// Comment adds a comment to issue ref. A pull request is refused. With a
// key, the comment ends with its marker, and one already posted under it
// counts as this one (see the package doc).
func (g *GitHub) Comment(ctx context.Context, ref, body string, m tracker.Mark) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("a comment needs a body")
	}
	if err := m.Check(); err != nil {
		return err
	}
	n, err := g.target(ctx, ref)
	if err != nil {
		return err
	}
	if m.Key != "" {
		done, err := g.commented(ctx, n, m)
		if err != nil {
			return fmt.Errorf("nothing posted: could not look for a comment already posted on %s under %s: %w", g.ref(n), m.Key, err)
		}
		if done {
			return nil
		}
	}
	_, err = g.do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", g.repoPath(), n), nil,
		map[string]any{"body": withMarker(body, m.Key)}, nil)
	if err != nil && m.Key != "" && mayHaveLanded(err) {
		return g.recheck(ctx, err, "the comment may have been posted", func() (bool, error) { return g.commented(ctx, n, m) })
	}
	return err
}

// Unlabel removes label from issue ref. The issue is read first, so a pull
// request or a missing issue is refused; a 404 from the DELETE after that
// is GitHub saying the issue lacks the label, which is done.
func (g *GitHub) Unlabel(ctx context.Context, ref, label string) error {
	if strings.TrimSpace(label) == "" {
		return errors.New("unlabel: no label")
	}
	n, err := g.target(ctx, ref)
	if err != nil {
		return err
	}
	_, err = g.do(ctx, http.MethodDelete, fmt.Sprintf("%s/issues/%d/labels/%s", g.repoPath(), n, url.PathEscape(label)), nil, nil, nil)
	if errors.Is(err, tracker.ErrNotFound) {
		return nil
	}
	return err
}

// Close closes issue ref with GitHub's state_reason. A pull request is
// refused.
func (g *GitHub) Close(ctx context.Context, ref, reason string) error {
	if !tracker.CheckReason(reason) {
		return fmt.Errorf("unknown close reason %q (want %s or %s)", reason, tracker.ReasonCompleted, tracker.ReasonNotPlanned)
	}
	if reason == "" {
		reason = tracker.ReasonCompleted
	}
	n, err := g.target(ctx, ref)
	if err != nil {
		return err
	}
	_, err = g.do(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/%d", g.repoPath(), n), nil,
		map[string]any{"state": tracker.StateClosed, "state_reason": reason}, nil)
	return err
}
