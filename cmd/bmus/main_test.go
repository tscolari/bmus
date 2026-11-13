package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/store"
)

func TestParseOrder(t *testing.T) {
	t.Parallel()
	args, err := parseOrder([]byte(`title: survey
division: science
assignee: sorell
repos:
  - name: app
    base_ref: main
budget_tokens: 1000
budget_turns: 40
model: model-x
body: |
  look around
`))
	require.NoError(t, err)
	require.Equal(t, bridge.OrderCreateArgs{Title: "survey", Division: "science", Assignee: "sorell",
		Repos: []bridge.OrderRepo{{Name: "app", BaseRef: "main"}}, BudgetTokens: 1000, BudgetTurns: 40, Body: "look around\n",
		Model: "model-x"}, args)

	// A tracker source stands in for the title (#54); any other doesn't.
	args, err = parseOrder([]byte("source: tracker:#12\ndivision: science\n"))
	require.NoError(t, err)
	require.Equal(t, bridge.OrderCreateArgs{Division: "science", Repos: []bridge.OrderRepo{}, Source: "tracker:#12"}, args)
	_, err = parseOrder([]byte("source: rfc-12\ndivision: science\n"))
	require.EqualError(t, err, "title is required (or a tracker source)")

	for name, doc := range map[string]string{
		"unknown field":  "title: t\nassignee: s\nurgency: 1\n",
		"no title":       "assignee: s\n",
		"no assignee":    "title: t\n",
		"bad priority":   "title: t\nassignee: s\npriority: high\n",
		"int priority":   "title: t\nassignee: s\npriority: 1\n",
		"bad type":       "title: t\nassignee: s\nbudget_tokens: lots\n",
		"negative":       "title: t\nassignee: s\nbudget_tokens: -1\n",
		"negative turns": "title: t\nassignee: s\nbudget_turns: -1\n",
		"two documents":  "title: t\nassignee: s\n---\ntitle: u\n",
		"empty":          "",
		"blank title":    "title: \"  \"\nassignee: s\n",
		"not a mapping":  "- a\n",
		"old base_ref":   "title: t\nassignee: s\nbase_ref: main\n",
		"repo field":     "title: t\nassignee: s\nrepos:\n  - name: a\n    ref: main\n",
		"repos not list": "title: t\nassignee: s\nrepos: app\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseOrder([]byte(doc))
			require.Error(t, err)
		})
	}
}

// priority names the store's levels (default routine), and a division
// alone makes a pool order (#96).
func TestParseOrderPriority(t *testing.T) {
	t.Parallel()
	for doc, want := range map[string]int{
		"title: t\nassignee: s\n":                    store.PriorityRoutine,
		"title: t\nassignee: s\npriority: routine\n": store.PriorityRoutine,
		"title: t\nassignee: s\npriority: yellow\n":  store.PriorityYellow,
		"title: t\ndivision: ops\npriority: red\n":   store.PriorityRed,
	} {
		args, err := parseOrder([]byte(doc))
		require.NoError(t, err, doc)
		require.Equal(t, want, args.Priority, doc)
	}
	args, err := parseOrder([]byte("title: t\ndivision: ops\n"))
	require.NoError(t, err)
	require.Empty(t, args.Assignee)
	require.Equal(t, "ops", args.Division)
	_, err = parseOrder([]byte("title: t\n"))
	require.EqualError(t, err, "assignee or division is required")
}

func TestParseOrderTrailingSeparator(t *testing.T) {
	t.Parallel()
	args, err := parseOrder([]byte("---\ntitle: t\nassignee: s\n---\n"))
	require.NoError(t, err)
	require.Equal(t, "t", args.Title)
}

func TestParseRepoFlags(t *testing.T) {
	repos, err := parseRepoFlags(nil)
	require.NoError(t, err)
	require.Empty(t, repos)

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "local"), 0o700))
	t.Chdir(dir)
	repos, err = parseRepoFlags([]string{"app=git@example.com:org/app.git", "lib=local", "web=https://example.com/w.git?x=a=b"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"app": "git@example.com:org/app.git",
		"lib": filepath.Join(dir, "local"), // an existing relative directory is made absolute
		"web": "https://example.com/w.git?x=a=b",
	}, repos)

	for _, flags := range [][]string{{"app"}, {"=u"}, {"app="}, {"a/b=u"}, {"in=u"}, {"app=u", "app=v"}, {"app=-oProxyCommand=x"}} {
		_, err := parseRepoFlags(flags)
		require.Error(t, err, "%v", flags)
	}
}

func TestResolveCombadge(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "combadge.mjs")
	require.NoError(t, os.WriteFile(bundle, []byte("//"), 0o600))

	got, err := resolveCombadge(bundle)
	require.NoError(t, err)
	require.Equal(t, bundle, got)

	_, err = resolveCombadge(filepath.Join(dir, "missing.mjs"))
	require.ErrorContains(t, err, "npm run build")
	_, err = resolveCombadge(dir)
	require.ErrorContains(t, err, "not a regular file")

	// The default falls back to ./combadge/dist/combadge.mjs.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "combadge", "dist"), 0o700))
	require.NoError(t, os.Rename(bundle, filepath.Join(dir, "combadge", "dist", "combadge.mjs")))
	t.Chdir(dir)
	got, err = resolveCombadge("")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "combadge", "dist", "combadge.mjs"), got)
	t.Chdir(t.TempDir())
	_, err = resolveCombadge("")
	require.ErrorContains(t, err, "--combadge")
}

// The order list's PROGRESS cell (#141): clamped to 0-100 and floored.
func TestProgressCell(t *testing.T) {
	t.Parallel()
	pct := func(v float64) *float64 { return &v }
	for in, want := range map[float64]string{0: "0%", 40: "40%", 99.6: "99%", 100: "100%", 140: "100%", -3: "0%"} {
		require.Equal(t, want, progressCell(pct(in)), in)
	}
	require.Equal(t, "-", progressCell(nil))
}

// crew list's BEAM cell (#106): a crew member beaming in without a beam
// shows provisioning; any other without one, "-".
func TestBeamCell(t *testing.T) {
	t.Parallel()
	require.Equal(t, "provisioning", beamCell(bridge.CrewView{Status: store.CrewBeamingIn}))
	require.Equal(t, "warm-orbit", beamCell(bridge.CrewView{Status: store.CrewBeamingIn, CurrentBeam: "warm-orbit"}))
	require.Equal(t, "-", beamCell(bridge.CrewView{Status: store.CrewOnDeck}))
	require.Equal(t, "warm-orbit", beamCell(bridge.CrewView{Status: store.CrewOnDeck, CurrentBeam: "warm-orbit"}))
}
