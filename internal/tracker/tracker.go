// Package tracker is the bridge's generic issue tracker interface (#54,
// spec/campaigns-plan-2026-11-03.md section 4.1). Beams hold no
// credentials (D6), so tracker access follows the git pattern: the bridge
// reads items into orders, and writes come out of beams as requests the
// bridge runs once Regulations allow them.
//
// No platform is hardcoded here. Adapters live outside the core packages
// (internal/adapters/...) and register themselves by name; tracker.yaml
// picks one. Having a tracker is optional: a bridge without one refuses
// tracker sources and requests, and works otherwise unchanged.
package tracker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// File is tracker.yaml's default name under the bridge home.
const File = "tracker.yaml"

// SourcePrefix starts an order source that names a tracker item:
// "tracker:<ref>".
const SourcePrefix = "tracker:"

// SourceRef is the tracker ref an order source names, and whether it
// names one at all.
func SourceRef(source string) (string, bool) {
	ref, ok := strings.CutPrefix(source, SourcePrefix)
	return strings.TrimSpace(ref), ok
}

// Item states.
const (
	StateOpen   = "open"
	StateClosed = "closed"
)

// Close reasons. An empty reason is ReasonCompleted.
const (
	ReasonCompleted  = "completed"
	ReasonNotPlanned = "not_planned"
)

// Item is a tracker item: an issue, a ticket.
//
// Ref is the adapter's own reference for the item, the one every method
// takes. BlockedBy lists the refs of the open items that block this one;
// it is nil when the platform keeps no dependency data, and empty when it
// does and nothing blocks the item. Parent is set by List when the query
// names one, and taken by Create.
type Item struct {
	Ref       string
	Title     string
	Body      string
	Labels    []string
	Milestone string
	Parent    string
	State     string
	BlockedBy []string
}

// Query filters List. Every set field must fit: all of Labels, the
// Milestone (by title), the Parent ref and the State (StateOpen,
// StateClosed, or "" for open; "all" for both).
type Query struct {
	Labels    []string
	Milestone string
	Parent    string
	State     string
}

// StateAll asks List for open and closed items.
const StateAll = "all"

// Tracker is an issue tracker the bridge reads and writes with its own
// credentials. Errors wrap ErrNotFound, ErrAuth, ErrRateLimited or
// ErrBadRef where they apply.
type Tracker interface {
	// Name is the adapter's name, as tracker.yaml gives it.
	Name() string
	// Canon is ref in the adapter's canonical form, without asking the
	// tracker: ErrBadRef for a ref it can't read or one outside what it
	// is configured for.
	Canon(ref string) (string, error)
	// Get reads one item.
	Get(ctx context.Context, ref string) (Item, error)
	// List reads the items q fits.
	List(ctx context.Context, q Query) ([]Item, error)
	// Create files a new item with its Title, Body, Labels, Milestone,
	// Parent and BlockedBy, and returns it with its Ref. Ref and State
	// are ignored. With a Mark, an item already filed under its key is
	// returned instead of a second one.
	Create(ctx context.Context, it Item, m Mark) (Item, error)
	// Comment adds a comment to item ref. With a Mark, a comment already
	// posted under its key counts as this one, and nothing is posted.
	Comment(ctx context.Context, ref, body string, m Mark) error
	// Close closes item ref, with reason ReasonCompleted ("" too) or
	// ReasonNotPlanned. Closing a closed item is no error.
	Close(ctx context.Context, ref, reason string) error
}

// Mark makes a write idempotent (#206): a write retried under the same
// Key, after a crash or a timeout whose outcome is unknown, finds the
// first one and does nothing. Adapters accept a key only on what the
// tracker's own account wrote. The bridge's key is the hail's id and the
// database's nonce (store.Nonce), so a fresh database never reuses one.
//
// Since is required with a key: a time before the first attempt (the
// bridge passes the hail's decided_at). Adapters look for the key only
// among what changed after it, give or take their own slack for clock
// skew. A zero Mark is no idempotency.
type Mark struct {
	Key   string
	Since time.Time
}

// keyRe is a key: words of letters, digits and ._:/- separated by single
// spaces. It holds nothing that could end an HTML comment or a line, so
// an adapter can embed it in a marker.
var keyRe = regexp.MustCompile(`^[A-Za-z0-9._:/-]+( [A-Za-z0-9._:/-]+)*$`)

// maxKey caps a key's length, in bytes.
const maxKey = 200

// Check refuses a key adapters can't embed: one not made of words of
// letters, digits and ._:/- separated by single spaces, or one longer than
// 200 bytes. It refuses a key with no Since too. A Mark with no key is no
// idempotency, and passes.
func (m Mark) Check() error {
	if m.Key == "" {
		return nil
	}
	if len(m.Key) > maxKey || !keyRe.MatchString(m.Key) {
		return fmt.Errorf("idempotency key %q: want words of letters, digits and ._:/- (at most %d bytes)", m.Key, maxKey)
	}
	if m.Since.IsZero() {
		return fmt.Errorf("idempotency key %q has no Since: a lookup needs its window", m.Key)
	}
	return nil
}

var (
	// ErrNotFound: the item (or the repository) doesn't exist, or the
	// bridge's credentials can't see it.
	ErrNotFound = errors.New("tracker: not found")
	// ErrAuth: the tracker refused the bridge's credentials.
	ErrAuth = errors.New("tracker: not authorized")
	// ErrBadRef: a ref the adapter can't read, or one outside what it is
	// configured for.
	ErrBadRef = errors.New("tracker: bad ref")
	// ErrRateLimited: the tracker refused the call for its rate limit.
	ErrRateLimited = errors.New("tracker: rate limited")
)

// CheckReason checks a close reason, "" meaning ReasonCompleted.
func CheckReason(reason string) bool {
	return reason == "" || reason == ReasonCompleted || reason == ReasonNotPlanned
}

// Fits reports whether it fits q's labels (case-insensitively, as GitHub
// matches them), milestone and state. Adapters
// that filter on their side may use it to filter what they couldn't (a
// parent's sub-items, say); Parent isn't checked.
func Fits(it Item, q Query) bool {
	for _, l := range q.Labels {
		if !slices.ContainsFunc(it.Labels, func(have string) bool { return strings.EqualFold(have, l) }) {
			return false
		}
	}
	if q.Milestone != "" && it.Milestone != q.Milestone {
		return false
	}
	switch q.State {
	case "", StateOpen:
		return it.State == StateOpen
	case StateAll:
		return true
	default:
		return it.State == q.State
	}
}
