// Package regs is the Regulations engine (spec/phase-3-plan-2026-10-29.md
// §2): the rules in regs.yaml that decide which hails are auto-allowed,
// which go to Command and which only the captain may answer. It is plain
// functions over a Hail the caller builds; it reads no store.
package regs

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"
)

// File is regs.yaml's default name under the bridge home
// (`bmus bridge run --regs`, `bmus regs init`).
const File = "regs.yaml"

// Default is the shipped regs.yaml, which `bmus regs init` writes.
//
//go:embed default.yaml
var Default []byte

// Routes Classify returns (hails.route).
const (
	RouteAuto    = "auto"    // an auto_allow rule allows it: decided_by policy
	RouteCommand = "command" // Command answers it, subject to CheckAnswer
	RouteCaptain = "captain" // a captain_only rule matched, or the hail is malformed
)

// CategoryToolApproval is the only category Classify ever auto-allows
// (comms.CategoryToolApproval).
const CategoryToolApproval = "tool_approval"

// Escalation.OnTimeout values.
const (
	OnTimeoutHold          = "hold"
	OnTimeoutBelay         = "belay"
	OnTimeoutDefaultOption = "default_option"
)

// DecisionAnswer and DecisionEscalate are Reply.Decision's values
// (comms.ConsultReplyBody.Decision).
const (
	DecisionAnswer   = "answer"
	DecisionEscalate = "escalate"
)

// Defaults CheckRegs gives a missing command_may_answer field. An explicit
// 0 stays 0.
const (
	DefaultMinConfidence     = 0.75
	DefaultCountermandWindow = 5 * time.Minute
)

// MinEscalation is the shortest non-zero captain_reminder_every and
// captain_timeout CheckRegs takes: each reminder is a log entry.
const MinEscalation = time.Second

// Alert levels a rule's Alert names, as orders.priority values
// (store.PriorityRoutine, PriorityYellow, PriorityRed).
var alertLevels = map[string]int{"routine": 0, "yellow": 1, "red": 2}

// Config is regs.yaml. Use it only after CheckRegs, which compiles its
// regexes and fills in defaults: Classify routes every hail to the captain
// on an unchecked one.
type Config struct {
	ClassifyRules    []ClassifyRule   `yaml:"classify"`
	CaptainOnly      []MatchRule      `yaml:"captain_only"`
	AutoAllow        []MatchRule      `yaml:"auto_allow"`
	CommandMayAnswer CommandMayAnswer `yaml:"command_may_answer"`
	Escalation       Escalation       `yaml:"escalation"`

	checked bool // CheckRegs passed
}

// CommandMayAnswer bounds Command's answers. Both fields are set after
// CheckRegs.
type CommandMayAnswer struct {
	MinConfidence *float64 `yaml:"min_confidence"` // 0-1; missing: DefaultMinConfidence
	// CountermandWindow is how long the captain may override Command's
	// answer; missing: DefaultCountermandWindow, 0: no window.
	CountermandWindow *time.Duration `yaml:"countermand_window"`
}

// Escalation is what happens to a hail the captain leaves open.
type Escalation struct {
	CaptainReminderEvery time.Duration `yaml:"captain_reminder_every"` // 0 never reminds
	OnTimeout            string        `yaml:"on_timeout"`             // hold (the default), belay or default_option
	CaptainTimeout       time.Duration `yaml:"captain_timeout"`        // 0 never times out
	DefaultOption        *int          `yaml:"default_option"`         // required by default_option
}

// ClassifyRule tags a hail whose Tool, Match, Command, Opaque and ANSIC
// fit (Hail.Tool; the substring regex Match over the hail's command; the
// regex Command over each simple command in it, its command word's
// basename then its arguments, dequoted (shellCommands); Opaque: the
// command runs something whose name can't be read, opaqueCommand; and
// ANSIC: the command holds an ANSI-C string where bash decodes one,
// parseShell). Unset fields fit any hail. Command, Opaque and ANSIC read
// only Hail.Command, and never a question or answer.
type ClassifyRule struct {
	Tag     string `yaml:"tag"`
	Tool    string `yaml:"tool"`
	Match   string `yaml:"match"`
	Command string `yaml:"command"`
	Opaque  bool   `yaml:"opaque"`
	ANSIC   bool   `yaml:"ansi_c"`
	// OpaqueArgs, with Opaque, is a regex over a command word's basename:
	// such a command's arguments count too (see opaqueCommand).
	OpaqueArgs string `yaml:"opaque_args"`
	// OpaqueSplitArgs, with Opaque, is a regex over a command word's
	// basename: an unquoted $(...) or `...` in such a command's arguments
	// counts too, since its output splits into words (rm $(echo -rf)).
	OpaqueSplitArgs string `yaml:"opaque_split_args"`

	re, cmdRE, argsRE, splitRE *regexp.Regexp
}

// noCommandTools are tools whose input has no command, so a Command or
// Opaque rule scoped to one could never fit.
var noCommandTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true,
	"Read": true, "Glob": true, "Grep": true, "WebFetch": true, "WebSearch": true, "TodoWrite": true}

// MatchRule is a captain_only or auto_allow rule. It fits a hail when every
// field set fits: Category, Tag (a classify tag), Tool, Match (a regex over
// the command: a substring match in captain_only, a whole match in
// auto_allow) and Alert (the order's priority is at least this level). A
// captain_only rule with Option (a regex over the hail's question and
// Command's picked option label or answer) is for CheckAnswer only;
// Classify skips it.
type MatchRule struct {
	Category string `yaml:"category"`
	Tag      string `yaml:"tag"`
	Tool     string `yaml:"tool"`
	Match    string `yaml:"match"`
	Option   string `yaml:"option"`
	Alert    string `yaml:"alert"`

	re, optRE *regexp.Regexp
	level     int
}

// Hail is what Classify and CheckAnswer read of a hail: store.Hail's
// category, question and options, what its context carries (SetToolContext)
// and its order's priority.
type Hail struct {
	Category string
	Question string
	Options  []string
	Tool     string
	// Command is the tool input's command; the only thing auto_allow
	// matches.
	Command string
	// Values are a non-Bash tool's input string values, which classify and
	// captain_only read after Command.
	Values    []string
	Malformed bool // the tool_approval context didn't decode, or named no tool
	Priority  int  // orders.priority
}

// Reply is what CheckAnswer reads of Command's comms.ConsultReplyBody.
type Reply struct {
	Decision   string
	Option     *int
	Answer     string
	Confidence float64
	RiskFlags  []string
}

// SetToolContext fills h's Tool, Command, Values and Malformed from a
// tool_approval hail's context, which combadge's sdk harness writes as
// {"tool": <name>, "input": <the tool's input>}. Command is input.command
// when that is a string, whatever the tool. For any tool but Bash, Values
// are every string in the input, decoded, keys in sorted order: the
// command's arguments and a Write's content get read too. A context that
// isn't such an object, names no tool or has a command that isn't a string
// sets Malformed.
func (h *Hail) SetToolContext(context json.RawMessage) {
	var c struct {
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	err := json.Unmarshal(context, &c)
	h.Tool, h.Command, h.Values = c.Tool, "", nil
	h.Malformed = err != nil || c.Tool == "" || c.Input == nil
	if h.Malformed {
		return
	}
	if v, has := c.Input["command"]; has {
		s, isStr := v.(string)
		if !isStr {
			h.Malformed = true
			return
		}
		h.Command = s
	}
	if c.Tool != "Bash" {
		h.Values = stringValues(nil, c.Input)
	}
}

// stringValues appends the strings in v, a decoded JSON value, to out.
func stringValues(out []string, v any) []string {
	switch v := v.(type) {
	case string:
		out = append(out, v)
	case []any:
		for _, e := range v {
			out = stringValues(out, e)
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			out = stringValues(out, v[k])
		}
	}
	return out
}

// LoadRegs reads and checks (CheckRegs) the regs file at path. It decodes
// strictly, as LoadRoster does: an unknown key is an error. A missing file
// is a zero Config and found false; an empty one is found.
func LoadRegs(path string) (cfg Config, found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, false, fmt.Errorf("%s: %w", path, err)
	}
	// Only empty documents may follow, e.g. after a trailing "---".
	for {
		var extra any
		err := dec.Decode(&extra)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || extra != nil {
			return Config{}, false, fmt.Errorf("%s: a regs file holds one YAML document", path)
		}
	}
	if err := CheckRegs(&cfg); err != nil {
		return Config{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, true, nil
}

// CheckRegs validates c, compiles its regexes and fills in its defaults. An
// error names the rule, e.g. "captain_only[2]".
func CheckRegs(c *Config) error {
	c.checked = false
	tags := map[string]bool{}
	for i := range c.ClassifyRules {
		r := &c.ClassifyRules[i]
		name := fmt.Sprintf("classify[%d]", i)
		if r.Tag == "" {
			return fmt.Errorf("%s: tag is required", name)
		}
		if r.Tool == "" && r.Match == "" && r.Command == "" && !r.Opaque && !r.ANSIC {
			return fmt.Errorf("%s: set tool, match, command, opaque or ansi_c", name)
		}
		if (r.Command != "" || r.Opaque || r.ANSIC) && noCommandTools[r.Tool] {
			return fmt.Errorf("%s: tool %s has no command for command, opaque or ansi_c to read", name, r.Tool)
		}
		var err error
		if r.re, err = compile(name, "match", r.Match, false); err != nil {
			return err
		}
		if r.cmdRE, err = compile(name, "command", r.Command, false); err != nil {
			return err
		}
		if (r.OpaqueArgs != "" || r.OpaqueSplitArgs != "") && !r.Opaque {
			return fmt.Errorf("%s: opaque_args and opaque_split_args need opaque: true", name)
		}
		if r.splitRE, err = compile(name, "opaque_split_args", r.OpaqueSplitArgs, false); err != nil {
			return err
		}
		if r.argsRE, err = compile(name, "opaque_args", r.OpaqueArgs, false); err != nil {
			return err
		}
		tags[r.Tag] = true
	}
	for _, list := range []struct {
		name  string
		rules []MatchRule
	}{{"captain_only", c.CaptainOnly}, {"auto_allow", c.AutoAllow}} {
		auto := list.name == "auto_allow"
		for i := range list.rules {
			r := &list.rules[i]
			name := fmt.Sprintf("%s[%d]", list.name, i)
			if r.Category == "" && r.Tag == "" && r.Tool == "" && r.Match == "" && r.Option == "" && r.Alert == "" {
				return fmt.Errorf("%s: an empty rule fits every hail; set at least one field", name)
			}
			if auto && r.Option != "" {
				return fmt.Errorf("%s: option is for captain_only rules", name)
			}
			if auto && r.Category != "" && r.Category != CategoryToolApproval {
				return fmt.Errorf("%s: category %q: only %s hails are auto-allowed", name, r.Category, CategoryToolApproval)
			}
			if auto && r.Tool == "" && r.Match == "" && r.Tag == "" {
				return fmt.Errorf("%s: set tool, match or tag; category or alert alone would allow every tool", name)
			}
			if r.Tag != "" && !tags[r.Tag] {
				return fmt.Errorf("%s: no classify rule sets tag %q", name, r.Tag)
			}
			r.level = 0
			if r.Alert != "" {
				l, ok := alertLevels[r.Alert]
				if !ok {
					return fmt.Errorf("%s: alert %q: want routine, yellow or red", name, r.Alert)
				}
				r.level = l
			}
			var err error
			if r.re, err = compile(name, "match", r.Match, auto); err != nil {
				return err
			}
			if r.optRE, err = compile(name, "option", r.Option, false); err != nil {
				return err
			}
		}
	}
	m := &c.CommandMayAnswer
	if m.MinConfidence == nil {
		m.MinConfidence = new(float64)
		*m.MinConfidence = DefaultMinConfidence
	}
	if v := *m.MinConfidence; !(v >= 0 && v <= 1) {
		return fmt.Errorf("command_may_answer.min_confidence: want 0 to 1, got %v", v)
	}
	if m.CountermandWindow == nil {
		m.CountermandWindow = new(time.Duration)
		*m.CountermandWindow = DefaultCountermandWindow
	}
	if *m.CountermandWindow < 0 {
		return errors.New("command_may_answer.countermand_window must not be negative")
	}
	e := &c.Escalation
	if e.CaptainReminderEvery < 0 || e.CaptainTimeout < 0 {
		return errors.New("escalation: durations must not be negative")
	}
	for _, d := range []struct {
		name string
		d    time.Duration
	}{{"captain_reminder_every", e.CaptainReminderEvery}, {"captain_timeout", e.CaptainTimeout}} {
		if d.d > 0 && d.d < MinEscalation {
			return fmt.Errorf("escalation.%s %v: want 0 (never) or at least %v", d.name, d.d, MinEscalation)
		}
	}
	switch e.OnTimeout {
	case "":
		e.OnTimeout = OnTimeoutHold
	case OnTimeoutHold, OnTimeoutBelay:
	case OnTimeoutDefaultOption:
		if e.DefaultOption == nil || *e.DefaultOption < 0 {
			return errors.New("escalation: on_timeout default_option needs a default_option of 0 or more")
		}
	default:
		return fmt.Errorf("escalation.on_timeout %q: want hold, belay or default_option", e.OnTimeout)
	}
	c.checked = true
	return nil
}

// compile compiles a rule's regex, nil for an empty one. whole anchors it to
// match the whole string, once expr compiles on its own: `a)|(.*` must not
// break out of the anchors.
func compile(rule, field, expr string, whole bool) (*regexp.Regexp, error) {
	if expr == "" {
		return nil, nil
	}
	re, err := regexp.Compile(expr)
	if err == nil && whole {
		re, err = regexp.Compile(`^(?:` + expr + `)$`)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %s %q: %w", rule, field, expr, err)
	}
	return re, nil
}

// unsafeForAuto are what make a command never auto-allowed: shell command
// separators, substitutions and redirections.
var unsafeForAuto = []string{";", "&", "|", "`", "$(", "<", ">"}

// autoSafe reports whether cmd may be auto-allowed at all: only printable
// ASCII, spaces and tabs (no newline, \v, \f or Unicode space), and none of
// unsafeForAuto.
func autoSafe(cmd string) bool {
	for i := 0; i < len(cmd); i++ {
		if b := cmd[i]; b != '\t' && (b < ' ' || b > '~') {
			return false
		}
	}
	return !slices.ContainsFunc(unsafeForAuto, func(s string) bool { return strings.Contains(cmd, s) })
}

// collapse trims s and collapses its runs of whitespace to one space.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// normalize is fold, then collapse.
func normalize(s string) (_ string, ok bool) {
	s, ok = fold(s)
	return collapse(s), ok
}

// fold drops format characters (zero-width spaces, joiners, bidi marks),
// applies NFKC (fullwidth and compatibility forms fold to ASCII) and maps
// the confusables (Cyrillic U+0435 to e). NFKC can grow text up to 18
// times (U+FDFA); ok is false, and the work stops, once the result passes
// maxText.
func fold(s string) (_ string, ok bool) {
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	var it norm.Iter
	it.InitString(norm.NFKC, s)
	out := make([]byte, 0, len(s))
	for !it.Done() {
		if out = append(out, it.Next()...); len(out) > maxText {
			return "", false
		}
	}
	return strings.Map(func(r rune) rune {
		if a, has := confusables[r]; has {
			return a
		}
		return r
	}, string(out)), true
}

// confusables are the Cyrillic, Greek and Latin letters NFKC leaves alone
// that look like an ASCII letter, mapped to it: "d\u0435lete" (Cyrillic
// U+0435) reads as delete. Only lookalikes in common fonts are listed.
var confusables = map[rune]rune{
	// Cyrillic.
	'\u0430': 'a', '\u0435': 'e', '\u043e': 'o', '\u0440': 'p', '\u0441': 'c', '\u0443': 'y',
	'\u0445': 'x', '\u0456': 'i', '\u0458': 'j', '\u0455': 's', '\u0501': 'd', '\u051b': 'q',
	'\u051d': 'w', '\u04bb': 'h', '\u04cf': 'l', '\u04af': 'y', '\u043a': 'k',
	'\u0410': 'A', '\u0412': 'B', '\u0415': 'E', '\u041a': 'K', '\u041c': 'M', '\u041d': 'H',
	'\u041e': 'O', '\u0420': 'P', '\u0421': 'C', '\u0422': 'T', '\u0425': 'X', '\u0406': 'I',
	'\u0408': 'J', '\u0405': 'S', '\u04ae': 'Y', '\u04c0': 'I',
	// Greek.
	'\u03b1': 'a', '\u03b9': 'i', '\u03ba': 'k', '\u03bd': 'v', '\u03bf': 'o', '\u03c1': 'p',
	'\u03c5': 'u', '\u03c7': 'x',
	'\u0391': 'A', '\u0392': 'B', '\u0395': 'E', '\u0396': 'Z', '\u0397': 'H', '\u0399': 'I',
	'\u039a': 'K', '\u039c': 'M', '\u039d': 'N', '\u039f': 'O', '\u03a1': 'P', '\u03a4': 'T',
	'\u03a5': 'Y', '\u03a7': 'X',
	// Latin.
	'\u0131': 'i', '\u0237': 'j', '\u0261': 'g',
}

// stripExpansions removes the shell expansions in s that may expand to
// nothing, or to something else than they show: $(...), ${...} and $[...]
// whole, nested ones included (g$(echo $(true))it), backtick pairs, an
// empty $"" or ANSI-C quote ($ then two single quotes), and the special
// parameters $@ $* $0-$9 $# $? $! $_ $-. An unclosed one stays.
func stripExpansions(s string) string {
	pairs := bracketPairs(s)
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				i += j + 2
				continue
			}
		case c == '$' && i+1 < len(s):
			switch n := s[i+1]; {
			case n == '(' || n == '{' || n == '[':
				if j, has := pairs[i+1]; has {
					i = j + 1
					continue
				}
			case strings.HasPrefix(s[i+1:], `""`) || strings.HasPrefix(s[i+1:], "''"):
				i += 3
				continue
			case strings.IndexByte("@*0123456789#?!_-", n) >= 0:
				i += 2
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// bracketPairs maps the index of each (, { and [ in s that is closed to
// the index of its closing bracket, counting nested pairs of the same kind.
func bracketPairs(s string) map[int]int {
	pairs := map[int]int{}
	var stacks [3][]int
	for i := 0; i < len(s); i++ {
		if k := strings.IndexByte("({[", s[i]); k >= 0 {
			stacks[k] = append(stacks[k], i)
		} else if k := strings.IndexByte(")}]", s[i]); k >= 0 && len(stacks[k]) > 0 {
			pairs[stacks[k][len(stacks[k])-1]] = i
			stacks[k] = stacks[k][:len(stacks[k])-1]
		}
	}
	return pairs
}

// assignment is a shell variable assignment, export x=... and the like
// included: its name, = or +=, and its value up to unquoted space or
// shell syntax.
var assignment = regexp.MustCompile(`(?:^|[\s;&|(])(?:(?:export|declare|typeset|local|readonly)\s+(?:-\S+\s+)*)?([A-Za-z_]\w*)(\+?=)((?:'[^']*'|"[^"]*"|[^\s;&|()<>'"])*)`)

// paramOps are the ${name<op>word} operators resolve reads, longest first.
var paramOps = []string{":-", ":=", ":+", ":?", "-", "=", "+", "?"}

// resolve substitutes in s the variables s itself assigns (x=git; $x push
// reads as git; git push), last assignment winning, and turns
// ${x:-word}, ${x:=word} and ${x:+word} into word unless x is assigned a
// non-empty value: g${x:-i}t reads as git. Other expansions stay, for
// stripExpansions. Values naming variables resolve too, a few levels deep.
// ok is false once the result passes maxText.
func resolve(s string) (_ string, ok bool) {
	vars := map[string]string{}
	for _, m := range loopNames.FindAllStringSubmatch(s, -1) {
		if m[2] != "" && !unknownList.MatchString(m[3]) {
			vars[m[1]] = collapse(dequote.Replace(m[3])) // every value, a space apart
		}
	}
	for _, m := range assignment.FindAllStringSubmatch(s, -1) {
		v := dequote.Replace(m[3])
		if m[2] == "+=" {
			v = vars[m[1]] + v
		}
		vars[m[1]] = v
	}
	for range 4 {
		next, ok := expandVars(s, vars)
		if !ok {
			return "", false
		}
		if next == s {
			break
		}
		s = next
	}
	return s, true
}

// paramName is the name a ${...} or $... expansion starts with.
var paramName = regexp.MustCompile(`^(?:[A-Za-z_]\w*|[0-9]+|[@*#?$!-])`)

// expandVars is one pass of resolve.
func expandVars(s string, vars map[string]string) (_ string, ok bool) {
	pairs := bracketPairs(s)
	var b strings.Builder
	for i := 0; i < len(s); {
		if b.Len() > maxText {
			return "", false
		}
		if s[i] != '$' || i+1 == len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		if s[i+1] == '{' {
			if j, has := pairs[i+1]; has {
				if v, ok := param(s[i+2:j], vars); ok {
					b.WriteString(v)
					i = j + 1
					continue
				}
			}
		} else if name := paramName.FindString(s[i+1:]); name != "" {
			if v, set := vars[name]; set {
				b.WriteString(v)
				i += 1 + len(name)
				continue
			}
		}
		b.WriteByte('$')
		i++
	}
	return b.String(), b.Len() <= maxText
}

// param resolves the body of ${...} with vars, ok false when it can't.
func param(body string, vars map[string]string) (_ string, ok bool) {
	name := paramName.FindString(body)
	if name == "" {
		return "", false
	}
	val, set := vars[name]
	rest := body[len(name):]
	if rest == "" {
		return val, set
	}
	for _, op := range paramOps {
		word, has := strings.CutPrefix(rest, op)
		if !has {
			continue
		}
		colon := op[0] == ':'
		// Set, and not empty when the operator has a colon.
		present := set && (!colon || val != "")
		switch op[len(op)-1] {
		case '-', '=':
			if present {
				return val, true
			}
			return word, true
		case '+':
			// Unassigned here, x may still be set in the environment.
			if present || !set {
				return word, true
			}
			return "", true
		default: // ?
			return val, set
		}
	}
	return val, set
}

// dequote removes the shell's quotes and backslashes.
var dequote = strings.NewReplacer(`'`, "", `"`, "", `\`, "")

// maxText caps the hail text Classify reads (Command, Values, Question
// and Options) and each answer text CheckAnswer reads, before and after
// normalize; past it, they fail closed.
const maxText = 1 << 20

// tooLarge is the rationale for text past maxText.
const tooLarge = "too large to classify"

// subjects are what classify and captain_only regexes see of s, once a
// backslash-newline (a line continuation) is removed: it normalized; that
// with the shell's quotes and backslashes (' " \) removed, so g"i"t and
// gi\t read as git; the normalized text with stripExpansions applied, then
// de-quoted; and the same after resolve, so x=sh; git pu$x reads as git
// push. Text holding ANSI-C strings gets the three de-quoted views again
// for each of two decodings (decodeANSIC, decodeANSICAnywhere), folded.
// ok is false when normalize, fold or resolve gave up.
func subjects(s string) (_ []string, ok bool) {
	n, ok := normalize(strings.ReplaceAll(s, "\\\n", ""))
	if !ok {
		return nil, false
	}
	out := []string{n}
	bases := []string{n}
	if strings.Contains(n, "$'") {
		for _, decode := range []func(string) string{decodeANSIC, decodeANSICAnywhere} {
			d, ok := fold(decode(n))
			if !ok {
				return nil, false
			}
			bases = append(bases, d)
		}
	}
	for _, b := range bases {
		r, ok := resolve(b)
		if !ok {
			return nil, false
		}
		for _, v := range []string{b, stripExpansions(b), stripExpansions(r)} {
			if v = collapse(dequote.Replace(v)); !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out, true
}

// decodeANSIC turns each ANSI-C string in s ($ then a single-quoted body)
// into a single-quoted string holding its body decoded, as bash reads
// it: \xHH, \NNN (octal), \uHHHH, \UHHHHHHHH, \cX, \n, \t and the
// other letter escapes, and \ before any other character. A decoded NUL
// ends the string, as in bash. Plain '...' and "..." spans and escaped
// characters are copied as they are, so a $ then a quote inside them
// starts nothing. git $ then '\x70ush' reads as git 'push'.
func decodeANSIC(s string) string {
	var b strings.Builder
	cut := false // a NUL ended the body: drop the rest of it
	quote := func(c byte) {
		switch {
		case cut:
		case c == 0:
			cut = true
		case c == '\'':
			b.WriteString(`'\''`)
		default:
			b.WriteByte(c)
		}
	}
	// span copies s[i:] up to and including the next unescaped end
	// (escapes is whether \ escapes there), and returns the index past it.
	span := func(i int, end byte, escapes bool) int {
		j := i + 1
		for j < len(s) && s[j] != end {
			if escapes && s[j] == '\\' {
				j++
			}
			j++
		}
		j = min(len(s), j+1)
		b.WriteString(s[i:j])
		return j
	}
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '\\':
			j := min(len(s), i+2)
			b.WriteString(s[i:j])
			i = j
			continue
		case c == '\'':
			i = span(i, '\'', false)
			continue
		case c == '"':
			i = span(i, '"', true)
			continue
		case !strings.HasPrefix(s[i:], "$'"):
			b.WriteByte(c)
			i++
			continue
		}
		b.WriteByte('\'')
		cut = false
		for i += 2; i < len(s) && s[i] != '\''; {
			if s[i] != '\\' || i+1 == len(s) {
				quote(s[i])
				i++
				continue
			}
			e := s[i+1]
			i += 2
			digits := func(max int, base string) (v int, n int) {
				for n < max && i+n < len(s) && strings.IndexByte(base, lower(s[i+n])) >= 0 {
					v = v*len(base) + strings.IndexByte(base, lower(s[i+n]))
					n++
				}
				return v, n
			}
			const hex, oct = "0123456789abcdef", "01234567"
			switch {
			case e == 'x' || e == 'u' || e == 'U':
				max := 2
				if e == 'u' {
					max = 4
				} else if e == 'U' {
					max = 8
				}
				v, n := digits(max, hex)
				switch {
				case n == 0:
					quote('\\')
					quote(e)
				case e == 'x':
					quote(byte(v))
				case v == 0:
					quote(0)
				default:
					for _, c := range []byte(string(rune(v))) {
						quote(c)
					}
				}
				i += n
			case e >= '0' && e <= '7':
				i--
				v, n := digits(3, oct)
				quote(byte(v))
				i += n
			case e == 'c' && i < len(s) && s[i] == '\\' && i+1 < len(s) && s[i+1] == '\\':
				quote('\\' & 0x1f) // \c\\ is one escape
				i += 2
			case e == 'c' && i < len(s) && s[i] != '\'' && s[i] != '\\':
				quote(s[i] & 0x1f)
				i++
			case e == 'c':
				quote('\\')
				quote('c')
			default:
				if c, has := ansiEscapes[e]; has {
					quote(c)
				} else {
					quote(e)
				}
			}
		}
		b.WriteByte('\'')
		i++
	}
	return b.String()
}

// decodeANSICAnywhere is decodeANSIC without its quote tracking: every $
// then a single quote starts an ANSI-C string, wherever it is. Bash
// decodes them inside "$(...)", "`...`" and "${x:-...}", and treats no
// quote as one in a comment or heredoc body, where decodeANSIC's spans
// go wrong. Each reading is a view of its own: a view only adds matches.
func decodeANSICAnywhere(s string) string {
	var b strings.Builder
	quote := func(c byte) {
		if c == '\'' {
			b.WriteString(`'\''`)
		} else {
			b.WriteByte(c)
		}
	}
	for i := 0; i < len(s); {
		if !strings.HasPrefix(s[i:], "$'") {
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteByte('\'')
		for i += 2; i < len(s) && s[i] != '\''; {
			if s[i] != '\\' || i+1 == len(s) {
				quote(s[i])
				i++
				continue
			}
			e := s[i+1]
			i += 2
			digits := func(max int, base string) (v int, n int) {
				for n < max && i+n < len(s) && strings.IndexByte(base, lower(s[i+n])) >= 0 {
					v = v*len(base) + strings.IndexByte(base, lower(s[i+n]))
					n++
				}
				return v, n
			}
			const hex, oct = "0123456789abcdef", "01234567"
			switch {
			case e == 'x' || e == 'u' || e == 'U':
				max := 2
				if e == 'u' {
					max = 4
				} else if e == 'U' {
					max = 8
				}
				v, n := digits(max, hex)
				switch {
				case n == 0:
					b.WriteByte('\\')
					quote(e)
				case e == 'x':
					quote(byte(v))
				default:
					for _, c := range []byte(string(rune(v))) {
						quote(c)
					}
				}
				i += n
			case e >= '0' && e <= '7':
				i--
				v, n := digits(3, oct)
				quote(byte(v))
				i += n
			case e == 'c' && i < len(s):
				quote(s[i] & 0x1f)
				i++
			default:
				if c, has := ansiEscapes[e]; has {
					quote(c)
				} else {
					quote(e)
				}
			}
		}
		b.WriteByte('\'')
		i++
	}
	return b.String()
}

// ansiEscapes are the letter escapes of an ANSI-C string.
var ansiEscapes = map[byte]byte{'a': '\a', 'b': '\b', 'e': 0x1b, 'E': 0x1b, 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t', 'v': '\v'}

// lower is c in lower case, for an ASCII letter.
func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// hailText is what classify and captain_only read of h: its command, then
// its input's other strings, a blank line apart, so a string ending in a
// backslash can't join the next one as a line continuation.
func hailText(h Hail) string { return strings.Join(append([]string{h.Command}, h.Values...), "\n\n") }

// Classify routes h:
//   - RouteCaptain for a tool_approval hail that is Malformed or names no
//     tool, for hail text over maxText, for a hail a captain_only rule
//     (other than an Option rule) fits, and for a question a captain_only
//     rule fits as CheckAnswer reads it (textRule);
//   - RouteAuto for a tool_approval hail an auto_allow rule fits, if its
//     command is autoSafe;
//   - else RouteCommand.
//
// tag is the first classify rule's tag that fits, "" for none; rule names
// the rule that routed h, "" for RouteCommand. captain_only Tag rules see
// every tag that fits, not only the first, so classify's order can't hide
// one.
func (c Config) Classify(h Hail) (route, tag, rule string) {
	route, tag, rule, _, _ = c.classify(h)
	return route, tag, rule
}

// classify is Classify, also returning h's subjects and tags.
func (c Config) classify(h Hail) (route, tag, rule string, subj, tags []string) {
	if !c.checked {
		return RouteCaptain, "", "regs not checked", nil, nil
	}
	if h.Category == CategoryToolApproval && (h.Malformed || h.Tool == "") {
		return RouteCaptain, "", "malformed tool_approval context", nil, nil
	}
	size := len(h.Command) + len(h.Question)
	for _, v := range slices.Concat(h.Values, h.Options) {
		size += len(v)
	}
	if size > maxText {
		return RouteCaptain, "", tooLarge, nil, nil
	}
	subj, ok := subjects(hailText(h))
	if !ok {
		return RouteCaptain, "", tooLarge, nil, nil
	}
	tags = c.tags(h.Tool, h.Command, subj, modeHail)
	if len(tags) > 0 {
		tag = tags[0]
	}
	for i, r := range c.CaptainOnly {
		if r.Option == "" && r.fits(h, subj, tags) {
			return RouteCaptain, tag, fmt.Sprintf("captain_only[%d]: %s", i, r), subj, tags
		}
	}
	if why, hit := c.textRule(h, subj, tags, h.Question); hit {
		return RouteCaptain, tag, "captain-only question: " + why, subj, tags
	}
	if h.Category == CategoryToolApproval && autoSafe(h.Command) {
		// auto_allow reads the command as written: its tags come from the
		// collapsed command and the raw values, never from a folded,
		// de-quoted, stripped or resolved form, which could only widen it.
		// A Tag rule needs its tag first in both passes: a command or
		// opaque rule listed first, which the auto pass leaves out, still
		// keeps a later match rule's tag from allowing.
		cmd := []string{collapse(h.Command)}
		autoTags := c.tags(h.Tool, h.Command, append(cmd, h.Values...), modeAuto)
		var first []string
		if len(tags) > 0 && len(autoTags) > 0 && tags[0] == autoTags[0] {
			first = autoTags[:1]
		}
		for i, r := range c.AutoAllow {
			if r.fits(h, cmd, first) {
				return RouteAuto, tag, fmt.Sprintf("auto_allow[%d]: %s", i, r), subj, tags
			}
		}
	}
	return RouteCommand, tag, "", subj, tags
}

// textRule names the first captain_only rule that fits text, a question,
// picked option label or answer of h (whose subjects and tags are subj and
// tags), or tooLarge for text past maxText:
//   - an Option rule fitting h whose regex matches text, normalized;
//   - a Tag rule fitting h with text's tags, from every classify rule
//     whatever its tool;
//   - a Match rule fitting h with text in place of the command.
func (c Config) textRule(h Hail, subj, tags []string, text string) (why string, hit bool) {
	if text == "" {
		return "", false
	}
	if len(text) > maxText {
		return tooLarge, true
	}
	tsubj, ok := subjects(text)
	if !ok {
		return tooLarge, true
	}
	ttags := c.tags("", "", tsubj, modeText)
	for i, r := range c.CaptainOnly {
		fit := false
		switch {
		case r.optRE != nil:
			fit = r.fits(h, subj, tags) && r.optRE.MatchString(tsubj[0])
		case r.Tag != "" || r.re != nil:
			fit = r.fits(h, tsubj, ttags)
		}
		if fit {
			return fmt.Sprintf("captain_only[%d]: %s", i, r), true
		}
	}
	return "", false
}

// CheckAnswer re-applies captain_only to Command's reply on h. It rejects:
//   - a hail Classify routes to the captain;
//   - an escalation, or any decision but an answer;
//   - any risk flag;
//   - a confidence below min_confidence, or outside 0-1;
//   - a missing or out-of-range answer;
//   - a question, picked option label or answer over maxText, before or
//     after normalize;
//   - a question, picked option label or answer a captain_only rule fits
//     (textRule), so picking "rm -rf the dir" is no way around the hail's
//     own route.
func (c Config) CheckAnswer(h Hail, r Reply) (ok bool, reason string) {
	route, _, rule, subj, tags := c.classify(h)
	if route == RouteCaptain {
		return false, "captain-only hail: " + rule
	}
	switch r.Decision {
	case DecisionAnswer:
	case DecisionEscalate:
		return false, "Command escalated"
	default:
		return false, fmt.Sprintf("decision %q is not %q", r.Decision, DecisionAnswer)
	}
	if len(r.RiskFlags) > 0 {
		return false, "Command flagged risks: " + strings.Join(r.RiskFlags, ", ")
	}
	floor := *c.CommandMayAnswer.MinConfidence
	if math.IsNaN(r.Confidence) || r.Confidence < floor || r.Confidence > 1 {
		return false, fmt.Sprintf("confidence %v is below %v or out of range", r.Confidence, floor)
	}
	texts := []string{h.Question}
	if r.Option != nil {
		if *r.Option < 0 || *r.Option >= len(h.Options) {
			return false, fmt.Sprintf("option %d is not one of the hail's %d options", *r.Option, len(h.Options))
		}
		texts = append(texts, h.Options[*r.Option])
	}
	if r.Answer != "" {
		texts = append(texts, r.Answer)
	}
	if len(texts) == 1 {
		return false, "no option or answer"
	}
	for _, t := range texts {
		if why, hit := c.textRule(h, subj, tags, t); hit {
			return false, fmt.Sprintf("captain-only %.80q: %s", t, why)
		}
	}
	return true, ""
}

// What tags reads.
const (
	modeHail = iota // a hail: every rule
	// modeAuto is a hail for auto_allow: only match rules, over its
	// command as written. Command, opaque and ansi_c rules read a parsed,
	// folded, de-quoted command, which could only widen auto_allow.
	modeAuto
	modeText // prose (a question or answer): every tool's rules but command, opaque and ansi_c ones
)

// tags returns the tag of every classify rule that fits tool (any tool in
// modeText), one of subj and, for a Command, Opaque or ANSIC rule, cmd, in
// order.
// A Command rule reads cmd as written and resolved (resolve).
func (c Config) tags(tool, cmd string, subj []string, mode int) []string {
	var tags []string
	var (
		parsed  bool
		cmds    []shCommand
		opaque  bool
		ansiC   bool
		renders []string
	)
	for _, r := range c.ClassifyRules {
		parses := r.cmdRE != nil || r.Opaque || r.ANSIC
		if mode != modeHail && parses {
			continue
		}
		if r.Tool != "" && r.Tool != tool && mode != modeText || !matchAny(r.re, subj) {
			continue
		}
		if parses && !parsed {
			parsed = true
			cmds, opaque, ansiC = parseShell(cmd)
			for _, sc := range cmds {
				renders = append(renders, sc.render())
			}
			// Command rules also read the command with its own literal
			// variables filled in: o=-toolexec=x; go test $o reads as
			// go test -toolexec=x.
			r, ok := resolve(cmd)
			opaque = opaque || !ok
			if ok && r != cmd {
				rcmds, _ := shellCommands(r)
				for _, sc := range rcmds {
					renders = append(renders, sc.render())
				}
			}
		}
		if r.cmdRE != nil && !slices.ContainsFunc(renders, r.cmdRE.MatchString) {
			continue
		}
		if r.Opaque && !opaque && !opaqueArgs(cmd, cmds, r.argsRE, r.splitRE) {
			continue
		}
		if r.ANSIC && !ansiC {
			continue
		}
		tags = append(tags, r.Tag)
	}
	return tags
}

// matchAny reports whether re is nil or matches one of subj.
func matchAny(re *regexp.Regexp, subj []string) bool {
	return re == nil || slices.ContainsFunc(subj, re.MatchString)
}

// fits reports whether r's fields other than Option fit h, whose command
// reads as subj and whose classify tags are tags.
func (r MatchRule) fits(h Hail, subj, tags []string) bool {
	return (r.Category == "" || r.Category == h.Category) &&
		(r.Tag == "" || slices.Contains(tags, r.Tag)) &&
		(r.Tool == "" || r.Tool == h.Tool) &&
		matchAny(r.re, subj) &&
		(r.Alert == "" || h.Priority >= r.level)
}

// String lists r's set fields, for logs and rationales.
func (r MatchRule) String() string {
	var parts []string
	for _, f := range [][2]string{{"category", r.Category}, {"tag", r.Tag}, {"tool", r.Tool},
		{"match", r.Match}, {"option", r.Option}, {"alert", r.Alert}} {
		if f[1] != "" {
			parts = append(parts, fmt.Sprintf("%s %q", f[0], f[1]))
		}
	}
	return strings.Join(parts, ", ")
}
