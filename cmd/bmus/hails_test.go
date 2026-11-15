package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/bridge"
)

// hails metrics --since takes Go durations, a leading day count, or an
// RFC 3339 time (#156).
func TestParseSince(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"1d12h":                now.Add(-36 * time.Hour),
		"36h":                  now.Add(-36 * time.Hour),
		"90m":                  now.Add(-90 * time.Minute),
		"0d1s":                 now.Add(-time.Second),
		"2026-10-01T00:00:00Z": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseSince(in, now)
		require.NoError(t, err, in)
		require.True(t, want.Equal(got), "%s: %v", in, got)
	}
	for _, in := range []string{"", "0", "0d", "-1h", "1d-1h", "d", "7 d", "1.5d", "week", "99999999999999999999d"} {
		_, err := parseSince(in, now)
		require.Error(t, err, in)
	}
}

// The table shows the counts, the rates with their fractions, and dashes
// for the rates and times of a period with nothing decided.
func TestPrintHailMetrics(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	share, rate := 0.75, 0.25
	median, p90 := int64(60000), int64(1200500)
	var b bytes.Buffer
	require.NoError(t, printHailMetrics(&b, bridge.HailsMetricsResult{Since: since, Total: 10, Decided: 8, Countermanded: 2,
		ByOriginalDecider: map[string]int{"captain": 2, "command": 3, "policy": 3},
		AutoResolvedShare: &share, CaptainOverrides: &rate, MedianTimeToAnswerMS: &median, P90TimeToAnswerMS: &p90}))
	require.Equal(t, `since                  2026-10-23T09:00:00Z
hails                  10
decided                8
  first by policy      3
  first by command     3
  first by captain     2
auto-resolved          75.0% (6 of 8)
captain overrides      25.0% (2 of 8)
time-to-answer median  1m0s
time-to-answer p90     20m0.5s
`, b.String())

	b.Reset()
	require.NoError(t, printHailMetrics(&b, bridge.HailsMetricsResult{Since: since, Total: 1,
		ByOriginalDecider: map[string]int{"captain": 0, "command": 0, "policy": 0}}))
	require.Equal(t, `since                  2026-10-23T09:00:00Z
hails                  1
decided                0
  first by policy      0
  first by command     0
  first by captain     0
auto-resolved          -
captain overrides      -
time-to-answer median  -
time-to-answer p90     -
`, b.String())
}

// A bridge-raised hail (#152) has no order or crew member: hails list
// prints - for both, beside a beam's hail.
func TestPrintHailsBridgeHail(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	require.NoError(t, printHails(&b, []bridge.HailView{
		{ID: "H-0001", DeliverTo: "findings/ORD-0001", Category: "tracker_review", Status: "open", Question: "Review 5 issues?",
			Options: []string{"keep", "discard"}},
		{ID: "H-0002", OrderID: "ORD-0002", CrewID: "sorell", Category: "clarifying_question", Status: "decided",
			Question: "Which?", Decision: "a"},
	}))
	require.Regexp(t, `(?m)^H-0001\s+-\s+-\s+tracker_review\s+open\s+Review 5 issues\?\s+0=keep 1=discard\s+-$`, b.String())
	require.Regexp(t, `(?m)^H-0002\s+ORD-0002\s+sorell\s+clarifying_question\s+decided\s+Which\?\s+-\s+a$`, b.String())
}

// oneLine collapses whitespace and turns control characters into spaces, so
// crew text in the log (a finding's title, say) can't move the terminal
// (#153).
func TestOneLine(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"plain":                     "plain",
		"  two\n lines\r\n":         "two lines",
		"tab\there":                 "tab here",
		"clear \x1b[2Jscreen":       "clear [2Jscreen",
		"bell\x07 del\x7f c1\u009b": "bell del c1",
		"nul\x00byte":               "nul byte",
	} {
		require.Equal(t, want, oneLine(in), "%q", in)
	}
}
