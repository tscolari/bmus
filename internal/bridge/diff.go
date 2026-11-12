package bridge

// The console's diff page (#104) reads a done order's work as one unified
// diff per repo, from the bridge's clone: git diff <base>...bmus/<order>,
// where the base is the commit the work started from.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tscolari/bmus/internal/store"
)

// consoleDiffCap caps each repo's diff the console's diff page gets.
const consoleDiffCap = 2 << 20

// fullCommitRE matches a full commit id: SHA-1 or SHA-256, in lower-case
// hex, as git prints them.
var fullCommitRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// OrderDiff is a done order's work as the console's diff page shows it:
// the order, its branch in the bridge's clone, each repo's diff, and Cap,
// the bytes each diff was capped at.
type OrderDiff struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Branch string     `json:"branch"`
	Cap    int        `json:"cap"`
	Repos  []RepoDiff `json:"repos"`
}

// RepoDiff is one repo's unified diff from Base, the commit the work
// started from, to the work. Truncated says Diff stops at the cap; it then
// ends at the last whole line under it.
type RepoDiff struct {
	Name      string `json:"name"`
	Base      string `json:"base"`
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated"`
}

// OrderDiff reads done order id's work in each of its repos, as `git diff
// <base>...bmus/<order>` in the bridge's clone, each capped at limit
// bytes. The order must exist and be done, and each repo be registered,
// with a full commit id as its base (baseCommits) and bmus/<order> in its
// clone; everything is checked before git runs. The diff takes no repo
// lock: it only reads, and a fetch into the clone may take minutes.
func (d *Daemon) OrderDiff(ctx context.Context, id string, limit int) (OrderDiff, error) {
	o, err := d.store.GetOrder(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return OrderDiff{}, fmt.Errorf("order %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return OrderDiff{}, err
	}
	if o.State != store.OrderDone {
		return OrderDiff{}, badRequest("order %s is %s; only a done order's work has a diff", id, o.State)
	}
	bases, err := d.baseCommits(ctx, id)
	if err != nil {
		return OrderDiff{}, err
	}
	branch := orderBranch(o.ID)
	for _, r := range o.Repos {
		if _, ok := d.repos[r.Name]; !ok {
			return OrderDiff{}, badRequest("repo %s is not registered on this bridge (repos: %s)", r.Name, d.repoNames())
		}
		if !fullCommitRE.MatchString(bases[r.Name]) {
			return OrderDiff{}, badRequest("order %s: repo %s has no recorded base commit", id, r.Name)
		}
		_, err := runGit(ctx, d.repoDir(r.Name), "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/heads/"+branch+"^{commit}")
		if ctx.Err() != nil {
			return OrderDiff{}, ctx.Err()
		}
		if err != nil {
			return OrderDiff{}, badRequest("repo %s: %s is missing from the bridge's clone", r.Name, branch)
		}
	}
	out := OrderDiff{ID: o.ID, Title: o.Title, Branch: branch, Cap: limit, Repos: []RepoDiff{}}
	for _, r := range o.Repos {
		base := bases[r.Name]
		diff, truncated, err := repoDiff(ctx, d.repoDir(r.Name), base, branch, limit)
		if err != nil {
			return OrderDiff{}, fmt.Errorf("repo %s: %w", r.Name, err)
		}
		out.Repos = append(out.Repos, RepoDiff{Name: r.Name, Base: base, Diff: diff, Truncated: truncated})
	}
	return out, nil
}

// repoDiff runs `git diff <base>...refs/heads/<branch>` in the clone
// gitDir and returns at most limit bytes of it. base must be a full commit
// id.
//
// The work's commits come from an agent, which may commit a
// .gitattributes naming a diff driver; the captain's config may define
// one by that name. --no-ext-diff and --no-textconv keep git from running
// any diff or textconv program, and -c diff.external= and -c
// core.attributesFile=/dev/null drop the external diff and the captain's
// own attributes file besides. Fixed prefixes keep "diff --git a/x b/x"
// headers whatever diff.noprefix or diff.mnemonicPrefix say.
//
// git reads each blob whole before it writes a line of its diff, so the
// output cap doesn't bound its memory. core.bigFileThreshold at
// consoleDiffCap makes a bigger file "Binary files ... differ", which
// spares the line diff's copies: git still reads the blob once to say so.
func repoDiff(ctx context.Context, gitDir, base, branch string, limit int) (string, bool, error) {
	if !fullCommitRE.MatchString(base) {
		return "", false, fmt.Errorf("base %q is not a full commit id", base)
	}
	out, truncated, err := runGitCapped(ctx, gitDir, limit,
		"-c", "core.attributesFile=/dev/null", "-c", "diff.external=", "-c", "core.bigFileThreshold="+strconv.Itoa(consoleDiffCap),
		"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--src-prefix=a/", "--dst-prefix=b/",
		"--end-of-options", base+"...refs/heads/"+branch, "--")
	if err != nil {
		return "", false, err
	}
	if truncated {
		if i := bytes.LastIndexByte(out, '\n'); i >= 0 {
			out = out[:i+1]
		}
	}
	return string(out), truncated, nil
}

// baseCommits returns order id's base commit in each repo, by name: the
// commit of its latest ship-repos entry that shipped the repo from its
// base_ref rather than a checkpoint. That is the commit the work started
// from, or one upstream moved to since, which a three-dot diff with the
// work still resolves to the work's own start.
func (d *Daemon) baseCommits(ctx context.Context, id string) (map[string]string, error) {
	entries, err := d.store.ListOrderLogKind(ctx, id, logOrder)
	if err != nil {
		return nil, err
	}
	bases := map[string]string{}
	for _, e := range entries {
		if !strings.Contains(e.Text, " shipped to the beam at ") {
			continue
		}
		var p struct {
			Repo    string `json:"repo"`
			BaseRef string `json:"base_ref"`
			Commit  string `json:"commit"`
			From    string `json:"from"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.Repo == "" || p.Commit == "" || p.From != p.BaseRef {
			continue
		}
		bases[p.Repo] = p.Commit
	}
	return bases, nil
}
