package bridge

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/tscolari/bmus/internal/store"
)

// RosterFile is the roster's default name under the bridge home
// (`bmus bridge run --roster`).
const RosterFile = "roster.yaml"

// Transporter limits' defaults (spec/phase-2-plan-2026-10-19.md §5).
const (
	DefaultMaxBeams        = 18 // D8's "roster <= ~18", two short of the 20-beam tenant cap
	DefaultMaxProvisioning = 3
	DefaultRatePerMinute   = 5
)

// RosterConfig is the roster file: the divisions, the standing orders'
// default and the transporter limits (spec/phase-2-plan-2026-10-19.md §2).
type RosterConfig struct {
	Divisions map[string]DivisionConfig `yaml:"divisions"`
	// StandingOrders is only a default: Daemon.StandingOrders seeds the
	// live setting from it once, and from then on the setting wins.
	StandingOrders bool              `yaml:"standing_orders"`
	Transporter    TransporterLimits `yaml:"transporter"`
}

// DivisionConfig is a division's defaults for its crew members (#59): a
// crew member's own harness or model overrides them, and an order's model
// overrides both (resolveHarness, resolveModel). Harness is empty or one of
// Harnesses. Budget is the default budget of the division's orders: an
// order's own value overrides it (resolveBudget).
type DivisionConfig struct {
	Harness string `yaml:"harness"`
	Model   string `yaml:"model"`
	Budget  Budget `yaml:"budget"`
}

// Budget caps what an order may spend (spec/phase-4-plan-2026-11-12.md
// section 6): tokens (input, cache reads and writes included, and output)
// and main-loop turns. Zero means unlimited.
type Budget struct {
	Tokens int64 `yaml:"tokens" json:"tokens"`
	Turns  int64 `yaml:"turns" json:"turns"`
}

// BudgetSource says where each of a resolved budget's fields comes from:
// SourceOrder, SourceDivision or SourceDefault (none, unlimited).
type BudgetSource struct {
	Tokens string `json:"tokens"`
	Turns  string `json:"turns"`
}

// TransporterLimits pace beam creation and cap the beams the bridge holds.
// CheckRoster gives a zero field its default.
type TransporterLimits struct {
	MaxBeams        int `yaml:"max_beams"`        // default 18
	MaxProvisioning int `yaml:"max_provisioning"` // default 3
	RatePerMinute   int `yaml:"rate_per_minute"`  // default 5
}

// divisionNameRE is what a division name must match.
var divisionNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// LoadRoster reads and checks (CheckRoster) the roster file at path. It
// decodes strictly, as `bmus order create` decodes orders: an unknown key is
// an error. A missing or empty file is a zero config, with the defaults.
func LoadRoster(path string) (RosterConfig, error) {
	var r RosterConfig
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return r, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil && !errors.Is(err, io.EOF) {
		return RosterConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	// Only empty documents may follow, e.g. after a trailing "---".
	for {
		var extra any
		err := dec.Decode(&extra)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || extra != nil {
			return RosterConfig{}, fmt.Errorf("%s: a roster file holds one YAML document", path)
		}
	}
	if err := CheckRoster(&r); err != nil {
		return RosterConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// CheckRoster validates r and fills in its defaults: each division name
// must match divisionNameRE, its harness be empty or one of Harnesses and
// its budget not negative,
// the limits must not be negative, and a zero limit takes its default.
func CheckRoster(r *RosterConfig) error {
	for _, name := range slices.Sorted(maps.Keys(r.Divisions)) {
		if !divisionNameRE.MatchString(name) {
			return fmt.Errorf("division name %q: use letters, digits, '_' and '-', starting with a letter", name)
		}
		if h := r.Divisions[name].Harness; h != "" && !slices.Contains(Harnesses, h) {
			return fmt.Errorf("division %s: unknown harness %q (want one of %s)", name, h, strings.Join(Harnesses, ", "))
		}
		if b := r.Divisions[name].Budget; b.Tokens < 0 || b.Turns < 0 {
			return fmt.Errorf("division %s: budget tokens and turns must not be negative", name)
		}
	}
	t := &r.Transporter
	for _, l := range []struct {
		name string
		v    *int
		def  int
	}{
		{"max_beams", &t.MaxBeams, DefaultMaxBeams},
		{"max_provisioning", &t.MaxProvisioning, DefaultMaxProvisioning},
		{"rate_per_minute", &t.RatePerMinute, DefaultRatePerMinute},
	} {
		if *l.v < 0 {
			return fmt.Errorf("transporter.%s must not be negative", l.name)
		}
		if *l.v == 0 {
			*l.v = l.def
		}
	}
	return nil
}

// checkDivision fails with a bad request when division isn't in the roster.
// A roster without divisions means divisions aren't configured, and any
// division passes; so does an empty one.
func (d *Daemon) checkDivision(division string) error {
	if division == "" || len(d.divisions) == 0 {
		return nil
	}
	if _, ok := d.divisions[division]; ok {
		return nil
	}
	return badRequest("unknown division %q (the roster has %s)", division,
		strings.Join(slices.Sorted(maps.Keys(d.divisions)), ", "))
}

// Where a crew member's harness or an order's model or budget comes from
// (CrewView.HarnessSource and ModelSource, resolveHarness, resolveModel,
// resolveBudget).
const (
	SourceOrder = "order" // the order's own model or budget
	SourceCrew  = "crew"  // the crew member's own setting
	// SourceDivision is the crew member's division's default; for a
	// budget, the order's division's, else its crew member's for a
	// directed order with none (resolveBudget).
	SourceDivision = "division"
	SourceDefault  = "default" // none: serve's default harness (sdk), the harness's default model, no budget
)

// resolveHarness is the harness crew member c's beams run (#59): c's own,
// else its division's default, else "" (serve's default, sdk), and its
// source. provision resolves it, so it is fixed for the tour; a roster
// changed at a restart applies from the next tour.
func (d *Daemon) resolveHarness(c store.Crew) (harness, source string) {
	if c.Harness != "" {
		return c.Harness, SourceCrew
	}
	if h := d.divisions[c.Division].Harness; h != "" {
		return h, SourceDivision
	}
	return "", SourceDefault
}

// resolveModel is the model order o runs on when crew member c takes it
// (#59): the order's own, else c's, else c's division's default, else ""
// (the harness's default), and its source. The string is opaque here: only
// the harness checks it.
func (d *Daemon) resolveModel(o store.Order, c store.Crew) (model, source string) {
	switch {
	case o.Model != "":
		return o.Model, SourceOrder
	case c.Model != "":
		return c.Model, SourceCrew
	}
	if m := d.divisions[c.Division].Model; m != "" {
		return m, SourceDivision
	}
	return "", SourceDefault
}

// resolveBudget is order o's budget when crew member c takes it (#118),
// each field on its own: the order's own value, else its division's
// default, else 0 (unlimited), with where each came from. The division is
// the order's; a directed order with none takes c's division. An order
// created without a budget, a split child among them, takes its
// division's. budget_extended is applied by budgetFor.
func (d *Daemon) resolveBudget(o store.Order, c store.Crew) (Budget, BudgetSource) {
	div := d.divisions[cmp.Or(o.Division, c.Division)].Budget
	pick := func(own, def int64) (int64, string) {
		switch {
		case own > 0:
			return own, SourceOrder
		case def > 0:
			return def, SourceDivision
		}
		return 0, SourceDefault
	}
	var b Budget
	var src BudgetSource
	b.Tokens, src.Tokens = pick(o.BudgetTokens, div.Tokens)
	b.Turns, src.Turns = pick(o.BudgetTurns, div.Turns)
	return b, src
}

// budgetFor is what an order command to beam carries about order o's
// budget b (#118): limit, each field of b times 1 + o.BudgetExtended (0
// stays 0, unlimited), and used, the usage recorded on the order's other
// beams. The harness's own count already holds this beam's earlier runs
// on the order (#117), and is over budget when used plus that count passes
// the limit (#119).
func budgetFor(o store.Order, b Budget, usage []store.Usage, beam string) (limit, used Budget) {
	for _, u := range usage {
		if u.Beam == beam {
			continue
		}
		used.Tokens += u.InputTokens + u.OutputTokens
		used.Turns += u.Turns
	}
	ext := 1 + max(o.BudgetExtended, 0)
	return Budget{Tokens: b.Tokens * ext, Turns: b.Turns * ext}, used
}

// checkHarness fails with a bad request when this bridge can't run crew
// member c's resolved harness (Config.Harnesses). AddCrew checks it before
// writing anything, and provision before making a beam: a roster changed at
// a restart, or a bridge started without the holodeck, can leave a crew
// member added earlier with a harness this bridge can't run.
func (d *Daemon) checkHarness(c store.Crew) error {
	h, source := d.resolveHarness(c)
	h = cmp.Or(h, "sdk")
	if slices.Contains(d.harnesses, h) {
		return nil
	}
	from := ""
	if source == SourceDivision {
		from = ", division " + c.Division + "'s default"
	}
	hint := ""
	if h == "holodeck" {
		hint = "; holodeck needs a bridge started with --transporter holodeck --holodeck-script"
	}
	return badRequest("this bridge can't run harness %q%s (it runs %s)%s", h, from, strings.Join(d.harnesses, ", "), hint)
}
