package regs

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// A shell command, as classify's Opaque and Command rules read it. This
// is no shell parser: only enough of one to split a command into simple
// commands, tell a command word from an argument and from quoted text,
// and skip comments, heredoc bodies and arithmetic.

// shWord is one word of a simple command.
type shWord struct {
	raw  string // as written
	text string // quotes and backslashes removed; expansions kept as written
	// opaque: the word holds an expansion, an unquoted glob (* ? [..],
	// extglob @(..)) or a brace expansion ({a,b}, {a..b}). An expansion
	// only in double-quoted directories (`"$GOROOT"/bin/go`,
	// `"$(go env GOROOT)/bin/gofmt"`) doesn't count: the name run is
	// still literal, and quoting rules out word splitting.
	opaque bool
	exps   []shExp // the expansions in it
}

// shExp is an expansion in a word.
type shExp struct {
	text   string // as written
	quoted bool   // inside double quotes: it doesn't split into words
	lead   bool   // no literal text comes before it in the word
}

// subst reports whether e is a command substitution, $(...) or `...`.
func (e shExp) subst() bool {
	return strings.HasPrefix(e.text, "`") || strings.HasPrefix(e.text, "$(") && !strings.HasPrefix(e.text, "$((")
}

// shCommand is a simple command's command word and arguments, once
// assignments, keywords and wrappers (env, sudo, xargs, ...) are skipped.
// A wrapper is a command too, with the rest of its words as arguments.
type shCommand struct {
	word shWord
	args []shWord
	// via names what runs it with arguments the command doesn't show:
	// "xargs", or "env -S" for a command in env's split string.
	via string
}

// render is c as Command rules read it: the command word's basename and
// its arguments, de-quoted, a space apart.
func (c shCommand) render() string {
	parts := []string{path.Base(c.word.text)}
	for _, a := range c.args {
		parts = append(parts, a.text)
	}
	return strings.Join(parts, " ")
}

// shellCommands splits cmd, folded, into its simple commands, those in
// command substitutions and unquoted heredoc bodies included, and reports
// whether one of them runs a command whose name can't be read without
// running the shell (an opaque command word), nesting passes maxDepth or
// the commands pass maxCommands.
func shellCommands(cmd string) (cmds []shCommand, opaque bool) {
	cmds, opaque, _ = parseShell(cmd)
	return cmds, opaque
}

// parseShell is shellCommands, also reporting whether cmd holds an ANSI-C
// string ($ then a single-quoted body) where bash decodes one: outside
// quotes, comments and heredoc bodies, in any word (an assignment, a for
// list, a case pattern, a redirection target or heredoc delimiter
// included), or in a substitution or ${...} anywhere. ansiC is true too
// when the scanner gave up before the end.
func parseShell(cmd string) (cmds []shCommand, opaque, ansiC bool) {
	s, ok := fold(strings.ReplaceAll(cmd, "\\\n", ""))
	if !ok {
		return nil, true, true
	}
	sc := scanner{s: s}
	sc.commands(-1)
	a := analyzer{}
	for _, ws := range sc.lists {
		if a.analyze(ws, false, "", 0); a.over {
			break
		}
	}
	for _, c := range a.cmds {
		opaque = opaque || c.word.opaque
	}
	return a.cmds, opaque || sc.deep || a.over, sc.ansiC || sc.deep
}

// opaqueCommand reports whether cmd runs something whose name or
// arguments can't be read statically (args and split nil read command
// words alone):
//   - an opaque command word (see shellCommands);
//   - an unresolvable expansion (see unresolved) in an argument of a
//     command whose basename args matches, or of one xargs or env -S runs;
//   - xargs running a command args matches without a visible subcommand
//     (xargs git, xargs rm): its arguments come from stdin;
//   - in an argument of a command whose basename split matches, a
//     substitution, or a variable the command doesn't assign literally
//     (unbound), that is unquoted or starts the argument: its value may
//     split into words or start with -, so it may be an option. A
//     substitution that only lists Go packages (goList) doesn't count.
func opaqueCommand(cmd string, args, split *regexp.Regexp) bool {
	cmds, opaque := shellCommands(cmd)
	return opaque || opaqueArgs(cmd, cmds, args, split)
}

// opaqueArgs is opaqueCommand's reading of arguments.
func opaqueArgs(cmd string, cmds []shCommand, args, split *regexp.Regexp) bool {
	s, _ := fold(cmd)
	if split != nil {
		var literal map[string]bool
		for _, c := range cmds {
			if !split.MatchString(path.Base(c.word.text)) {
				continue
			}
			for _, a := range c.args {
				for _, e := range a.exps {
					switch {
					case e.quoted && !e.lead:
					case e.subst():
						if !goList(e.text) {
							return true
						}
					default:
						if literal == nil {
							literal = literalVars(s)
						}
						if unbound(e.text, literal) {
							return true
						}
					}
				}
			}
		}
	}
	if args == nil {
		return false
	}
	bound := boundVars(s)
	for _, c := range cmds {
		base := path.Base(c.word.text)
		listed := args.MatchString(base)
		if c.via == "xargs" && listed && !visibleSubcommand(base, c.args) {
			return true
		}
		if !listed && c.via == "" {
			continue
		}
		for _, a := range c.args {
			for _, e := range a.exps {
				if unresolved(e.text, bound) {
					return true
				}
			}
		}
	}
	return false
}

// literalVars are the variables s assigns only literal values (no $ or
// backtick in them) or loops over a literal list or a goList, and the
// environment's well-known paths (envPaths) unless s assigns them some
// other way.
func literalVars(s string) map[string]bool {
	vars := map[string]bool{}
	set := func(name string, literal bool) {
		if v, has := vars[name]; !has || v {
			vars[name] = literal
		}
	}
	for _, m := range loopNames.FindAllStringSubmatch(s, -1) {
		set(m[1], m[2] != "" && (!unknownList.MatchString(m[3]) || goList(strings.TrimSpace(m[3]))))
	}
	for _, m := range assignment.FindAllStringSubmatch(s, -1) {
		set(m[1], !strings.ContainsAny(m[3], "$`"))
	}
	for _, n := range envPaths {
		set(n, true)
	}
	return vars
}

// envPaths are variables the environment sets to a path or a name, never
// to an option: $HOME/bin, $TMPDIR/x and $GOPATH/pkg read as bound.
var envPaths = []string{"HOME", "TMPDIR", "PWD", "GOPATH", "GOBIN", "GOROOT", "GOCACHE", "GOMODCACHE", "USER"}

// unbound reports whether the variable expansion e, as written, may
// expand to anything: a $"..." string, or a variable literal doesn't
// hold, the positional parameters included. The numeric $$ $? $! $#
// and ${#x}, arithmetic, $[...] and ANSI-C strings don't count.
func unbound(e string, literal map[string]bool) bool {
	switch {
	case strings.HasPrefix(e, `$"`):
		return true
	case strings.HasPrefix(e, "$((") || strings.HasPrefix(e, "$[") || strings.HasPrefix(e, "$'"):
		return false
	}
	body, braced := strings.CutPrefix(e, "${")
	if !braced {
		body = strings.TrimPrefix(e, "$")
	} else if strings.HasPrefix(body, "#") && len(body) > 2 {
		return false // ${#x}: a length
	}
	name := paramName.FindString(body)
	return name == "" || !strings.Contains("$?!#", name) && !literal[name]
}

var goListFlag = regexp.MustCompile(`^-(e|deps|find|(tags|mod)=\S*)$`)

// goList reports whether the substitution e, $(...) or backticks as
// written, only lists Go packages: go list with package patterns and
// none of the flags that print other text (-f, -json, -test, ...), piped
// through any number of greps that filter its lines. Import paths never
// start with -, so its words are no options: go test $(go list ./... |
// grep -v /e2e) stays readable.
func goList(e string) bool {
	var body string
	switch {
	case strings.HasPrefix(e, "$(") && strings.HasSuffix(e, ")"):
		body = e[2 : len(e)-1]
	case len(e) > 1 && e[0] == '`' && e[len(e)-1] == '`':
		body = e[1 : len(e)-1]
	default:
		return false
	}
	if strings.Contains(body, "$(") || strings.ContainsAny(body, ";&<>`\\\n") || strings.Contains(body, "||") {
		return false
	}
	cmds, opaque := shellCommands(body)
	if opaque || len(cmds) == 0 || !goCommand.MatchString(path.Base(cmds[0].word.text)) {
		return false
	}
	for i, c := range cmds {
		for _, a := range c.args {
			if a.opaque || len(a.exps) > 0 {
				return false
			}
		}
		if i > 0 && !grepFilter(c) {
			return false
		}
	}
	args := cmds[0].args
	if len(args) == 0 || args[0].text != "list" {
		return false
	}
	return !slices.ContainsFunc(args[1:], func(a shWord) bool {
		return strings.HasPrefix(a.text, "-") && !goListFlag.MatchString(a.text)
	})
}

var (
	goCommand  = regexp.MustCompile(`^go[0-9.]*$`)
	grepOption = regexp.MustCompile(`^(-[vEFiwx]+|--(invert-match|extended-regexp|fixed-strings|ignore-case|word-regexp|line-regexp))$`)
)

// grepFilter reports whether c is a grep that only filters its input's
// lines: no -o, no file to read, no option outside a short list.
func grepFilter(c shCommand) bool {
	if b := path.Base(c.word.text); b != "grep" && b != "egrep" && b != "fgrep" {
		return false
	}
	patterns, positional := 0, 0
	for i := 0; i < len(c.args); i++ {
		switch t := c.args[i].text; {
		case t == "-e":
			patterns++
			i++
		case strings.HasPrefix(t, "-e"):
			patterns++
		case strings.HasPrefix(t, "-") && t != "-":
			if !grepOption.MatchString(t) {
				return false
			}
		default:
			positional++
		}
	}
	return patterns > 0 && positional == 0 || patterns == 0 && positional == 1
}

// visibleSubcommand reports whether git or go* (base) is given its
// subcommand as a literal first argument.
func visibleSubcommand(base string, args []shWord) bool {
	return (base == "git" || strings.HasPrefix(base, "go")) && len(args) > 0 &&
		!args[0].opaque && len(args[0].exps) == 0 && !strings.HasPrefix(args[0].text, "-")
}

// maxDepth bounds the nesting of substitutions shellCommands reads, and
// maxCommands the simple commands it returns.
const (
	maxDepth    = 64
	maxCommands = 1 << 16
)

var (
	assignmentWord = regexp.MustCompile(`^[A-Za-z_]\w*(\[[^]]*\])?\+?=`)
	keywords       = map[string]bool{"!": true, "{": true, "}": true, "if": true, "then": true, "else": true,
		"elif": true, "fi": true, "do": true, "done": true, "while": true, "until": true,
		"esac": true, "coproc": true}
	// notCommands start a word list that runs no command of its own.
	notCommands = map[string]bool{"for": true, "select": true, "case": true, "in": true}
	// wrappers run their arguments as a command. Each lists its options
	// that take a value, and how many positional arguments come before the
	// command.
	wrappers = map[string]struct {
		valueOpts string // short options taking a value, e.g. "ug" for -u x -g y
		args      int
	}{
		"env": {"uCS", 0}, "command": {"", 0}, "builtin": {"", 0}, "exec": {"a", 0},
		"nohup": {"", 0}, "xargs": {"IiLlnPdEas", 0}, "sudo": {"ugCDhprtU", 0}, "doas": {"uC", 0},
		"nice": {"n", 0}, "timeout": {"sk", 1}, "stdbuf": {"ioe", 0}, "setsid": {"", 0},
		"time": {"fo", 0}, "busybox": {"", 0}, "strace": {"oepsuIEPXbS", 0}, "ltrace": {"oepsuIEn", 0},
		"watch": {"nd", 0}, "flock": {"wEc", 1}, "ionice": {"cnp", 0}, "chroot": {"", 1},
		"unshare": {"S", 0}, "nsenter": {"tSG", 0}, "chrt": {"", 1}, "taskset": {"", 1},
	}
	// findExecs start a command inside find's arguments.
	findExecs = map[string]bool{"-exec": true, "-execdir": true, "-ok": true, "-okdir": true}
)

// analyzer collects the commands of each simple command it is given.
type analyzer struct {
	cmds []shCommand
	over bool // past maxCommands, or env -S strings nested past maxDepth
}

func (a *analyzer) add(c shCommand) {
	if len(a.cmds) >= maxCommands {
		a.over = true
		return
	}
	a.cmds = append(a.cmds, c)
}

// analyze adds the command word of the simple command ws, skipping
// assignments, keywords and wrappers; each wrapper is a command too. A
// find's -exec and -ok bodies are commands too, each ending at ;, + or
// the next -exec, unless inFind: a find in a body is read once. via is
// what runs ws's command (shCommand.via); depth counts env -S nesting.
func (a *analyzer) analyze(ws []shWord, inFind bool, via string, depth int) {
	i := 0
	for i < len(ws) && !a.over {
		w := ws[i]
		base := path.Base(w.text)
		switch {
		case assignmentWord.MatchString(w.raw) || keywords[w.text]:
			i++
		case w.text == "function":
			i += 2 // the name
		case notCommands[w.text]:
			return
		case isWrapper(w):
			wr := wrappers[base]
			a.add(shCommand{word: w, args: ws[i+1:], via: via})
			if base == "xargs" {
				via = "xargs"
			}
			i++
			for i < len(ws) && strings.HasPrefix(ws[i].text, "-") && ws[i].text != "-" {
				opt := ws[i].text
				i++
				if opt == "--" {
					break
				}
				if base == "env" && (strings.HasPrefix(opt, "-S") || strings.HasPrefix(opt, "--split-string")) {
					val, has := strings.CutPrefix(opt, "-S")
					if !has {
						val, has = strings.CutPrefix(opt, "--split-string=")
					}
					if (!has || val == "") && i < len(ws) {
						val = ws[i].text
					}
					a.split(val, depth+1)
					return
				}
				if len(opt) == 2 && strings.ContainsRune(wr.valueOpts, rune(opt[1])) {
					i++
				}
			}
			for n := 0; n < wr.args && i < len(ws); n++ {
				i++
			}
		default:
			c := shCommand{word: w, args: ws[i+1:], via: via}
			a.add(c)
			if base != "find" || inFind {
				return
			}
			for j := 0; j < len(c.args); j++ {
				if !findExecs[c.args[j].text] {
					continue
				}
				end := j + 1
				for end < len(c.args) && c.args[end].text != ";" && c.args[end].text != "+" && !findExecs[c.args[end].text] {
					end++
				}
				a.analyze(c.args[j+1:end], true, "", depth)
				j = end - 1
			}
			return
		}
	}
}

// split adds the commands in env -S's string s.
func (a *analyzer) split(s string, depth int) {
	if depth > maxDepth {
		a.over = true
		return
	}
	sc := scanner{s: s}
	sc.commands(-1)
	a.over = a.over || sc.deep
	for _, ws := range sc.lists {
		a.analyze(ws, false, "env -S", depth)
	}
}

// isWrapper reports whether w names a wrapper.
func isWrapper(w shWord) bool {
	_, has := wrappers[path.Base(w.text)]
	return has && !w.opaque
}

// scanner is shellCommands' state: s from i on.
type scanner struct {
	s     string
	i     int
	depth int
	deep  bool       // nesting passed maxDepth
	ansiC bool       // an ANSI-C string was read where bash decodes it
	lists [][]shWord // every simple command's words
}

type heredoc struct {
	delim  string
	tabs   bool // <<-: leading tabs are stripped
	quoted bool // the delimiter is quoted: the body expands nothing
}

// Case states, for a case statement's patterns, whose ) closes nothing.
const (
	caseSubject  = iota + 1 // case WORD in
	casePatterns            // pat | pat )
	caseBody                // commands up to ;; or esac
)

// commands reads a list of commands up to closer (')' or '`', -1 for the
// end of s), and past it.
func (sc *scanner) commands(closer int) {
	if sc.depth++; sc.depth > maxDepth {
		sc.deep, sc.i = true, len(sc.s)
		return
	}
	defer func() { sc.depth-- }()
	var cur []shWord
	flush := func() {
		if len(cur) > 0 {
			sc.lists = append(sc.lists, cur)
		}
		cur = nil
	}
	defer flush()
	target := false
	parens := 0
	var cases []int // a stack of case states
	var docs []heredoc
	inCase := func(st int) bool { return len(cases) > 0 && cases[len(cases)-1] == st }
	for sc.i < len(sc.s) && !sc.deep {
		c := sc.s[sc.i]
		switch {
		case int(c) == closer && (c != ')' || parens == 0 && !inCase(casePatterns)):
			sc.i++
			return
		case c == ' ' || c == '\t':
			sc.i++
		case c == '\n':
			sc.i++
			sc.skipHeredocs(docs)
			docs = nil
			flush()
			target = false
		case c == '#': // a comment: # starts no word here
			for sc.i < len(sc.s) && sc.s[sc.i] != '\n' {
				sc.i++
			}
		case c == '(' && len(cur) == 0 && strings.HasPrefix(sc.s[sc.i:], "(("):
			sc.arithmetic() // ((n=1<<3)): no command, no heredoc
		case c == '(' || c == ')':
			switch {
			case inCase(casePatterns) && c == ')':
				cases[len(cases)-1] = caseBody
			case inCase(casePatterns):
			case c == '(':
				parens++
			default:
				parens--
			}
			sc.i++
			flush()
			target = false
		case c == ';' || c == '&' || c == '|':
			if c == ';' && strings.HasPrefix(sc.s[sc.i:], ";;") && inCase(caseBody) {
				cases[len(cases)-1] = casePatterns
				sc.i += 2
				if sc.i < len(sc.s) && sc.s[sc.i] == '&' {
					sc.i++
				}
			} else if c == ';' && strings.HasPrefix(sc.s[sc.i:], ";&") && inCase(caseBody) {
				cases[len(cases)-1] = casePatterns
				sc.i += 2
			} else {
				sc.i++
			}
			flush()
			target = false
		case c == '<' || c == '>':
			if strings.HasPrefix(sc.s[sc.i:], "<<") && !strings.HasPrefix(sc.s[sc.i:], "<<<") {
				sc.i += 2
				d := heredoc{}
				if sc.i < len(sc.s) && sc.s[sc.i] == '-' {
					d.tabs = true
					sc.i++
				}
				for sc.i < len(sc.s) && (sc.s[sc.i] == ' ' || sc.s[sc.i] == '\t') {
					sc.i++
				}
				w := sc.word(closer)
				d.delim, d.quoted = heredocDelim(w.raw)
				docs = append(docs, d)
				continue
			}
			sc.i++
			if sc.i < len(sc.s) && sc.s[sc.i] == '(' {
				continue // process substitution: ( starts its commands
			}
			for sc.i < len(sc.s) && strings.IndexByte("<>&|", sc.s[sc.i]) >= 0 {
				sc.i++
			}
			target = true
		default:
			w := sc.word(closer)
			switch {
			case target:
				target = false
			case len(cur) == 0 && w.text == "case":
				cases = append(cases, caseSubject)
				cur = append(cur, w)
			case len(cur) == 0 && w.text == "esac" && len(cases) > 0:
				cases = cases[:len(cases)-1]
				cur = append(cur, w)
			case inCase(caseSubject) && w.text == "in":
				cases[len(cases)-1] = casePatterns
				flush()
			case inCase(casePatterns):
				if w.text == "esac" {
					cases = cases[:len(cases)-1]
				}
			default:
				cur = append(cur, w)
			}
		}
	}
}

// arithmetic skips an arithmetic command or expansion's ((...)), from its
// first (, reading any $(...) in it as commands.
func (sc *scanner) arithmetic() {
	depth := 0
	for sc.i < len(sc.s) && !sc.deep {
		switch c := sc.s[sc.i]; {
		case strings.HasPrefix(sc.s[sc.i:], "$(") && !strings.HasPrefix(sc.s[sc.i:], "$(("):
			sc.i += 2
			sc.commands(')')
			continue
		case c == '`':
			sc.backticks()
			continue
		case strings.HasPrefix(sc.s[sc.i:], "$'"):
			// Bash decodes $'...' in arithmetic, then expands any $(...)
			// the decoded text holds (in a subscript, say).
			sc.ansiC = true
			sc.i++
			sc.skipANSIC()
			continue
		case c == '(':
			depth++
		case c == ')':
			depth--
		}
		sc.i++
		if depth == 0 {
			return
		}
	}
}

// skipHeredocs skips the bodies of docs, which start at i. An unquoted
// delimiter's body expands $(...) and `...`: their commands are read.
func (sc *scanner) skipHeredocs(docs []heredoc) {
	for _, d := range docs {
		start, end := sc.i, len(sc.s)
		for sc.i < len(sc.s) {
			from := sc.i
			line, _, _ := strings.Cut(sc.s[sc.i:], "\n")
			sc.i = min(len(sc.s), sc.i+len(line)+1)
			if d.tabs {
				line = strings.TrimLeft(line, "\t")
			}
			if line == d.delim {
				end = from
				break
			}
		}
		if !d.quoted {
			sc.expandBody(sc.s[start:end])
		}
	}
}

// expandBody reads the substitutions in an unquoted heredoc body.
func (sc *scanner) expandBody(body string) {
	in := scanner{s: body, depth: sc.depth}
	for in.i < len(in.s) && !in.deep {
		switch c := in.s[in.i]; {
		case c == '\\':
			in.i = min(len(in.s), in.i+2)
		case c == '`':
			in.backticks()
		case c == '$':
			// quoted: $' and $" are literal in a heredoc body.
			st := wordState{quotedExpEnd: -1, lastSlash: -1, quoted: true}
			in.dollar(&st)
		default:
			in.i++
		}
	}
	sc.lists = append(sc.lists, in.lists...)
	sc.deep = sc.deep || in.deep
	sc.ansiC = sc.ansiC || in.ansiC
}

// backticks reads a `...` substitution from its opening backtick. Inside
// it, \` and \\ stand for ` and \, so the body is unescaped and read as a
// shell command of its own: nested backticks read too.
func (sc *scanner) backticks() {
	var body strings.Builder
	sc.i++
	for sc.i < len(sc.s) && sc.s[sc.i] != '`' {
		if sc.s[sc.i] == '\\' && sc.i+1 < len(sc.s) && strings.IndexByte("`\\$", sc.s[sc.i+1]) >= 0 {
			sc.i++
		}
		body.WriteByte(sc.s[sc.i])
		sc.i++
	}
	sc.i = min(len(sc.s), sc.i+1)
	inner := scanner{s: body.String(), depth: sc.depth}
	inner.commands(-1)
	sc.lists = append(sc.lists, inner.lists...)
	sc.deep = sc.deep || inner.deep
	sc.ansiC = sc.ansiC || inner.ansiC
}

// wordState tracks what makes a word opaque while it is read.
type wordState struct {
	b                         strings.Builder
	glob, unquotedExp         bool
	exps                      []shExp
	lit                       bool // a literal byte was written
	bracket, brace, braceList bool
	quotedExpEnd, lastSlash   int // offsets in b; -1 for none
	quoted                    bool
}

func (ws *wordState) literal(c byte) {
	ws.lit = true
	if c == '/' {
		ws.lastSlash = ws.b.Len()
	}
	ws.b.WriteByte(c)
}

// expansion records an expansion just read, whose text is t.
func (ws *wordState) expansion(t string) {
	ws.b.WriteString(t)
	ws.exps = append(ws.exps, shExp{text: t, quoted: ws.quoted, lead: !ws.lit})
	if ws.quoted {
		ws.quotedExpEnd = ws.b.Len()
	} else {
		ws.unquotedExp = true
	}
}

// word reads one word.
func (sc *scanner) word(closer int) shWord {
	start := sc.i
	st := wordState{quotedExpEnd: -1, lastSlash: -1}
	done := func() shWord {
		text := st.b.String()
		exp := st.unquotedExp || st.quotedExpEnd >= 0 && st.quotedExpEnd > st.lastSlash
		return shWord{raw: sc.s[start:sc.i], text: text, opaque: exp || st.glob, exps: st.exps}
	}
	for sc.i < len(sc.s) && !sc.deep {
		c := sc.s[sc.i]
		switch c {
		case ' ', '\t', '\n', ';', '&', '|', '<', '>', ')':
			return done()
		case '(':
			// An extglob: @(..), +(..), !(..), *(..), ?(..).
			if sc.i > start && strings.IndexByte("@+!*?", sc.s[sc.i-1]) >= 0 {
				from := sc.i
				sc.arithmetic()
				st.b.WriteString(sc.s[from:sc.i])
				st.glob = true
				continue
			}
			return done()
		case '`':
			if closer == '`' {
				return done()
			}
			from := sc.i
			sc.backticks()
			st.expansion(sc.s[from:sc.i])
		case '\\':
			if sc.i+1 < len(sc.s) {
				st.literal(sc.s[sc.i+1])
			}
			sc.i = min(len(sc.s), sc.i+2)
		case '\'':
			j := strings.IndexByte(sc.s[sc.i+1:], '\'')
			if j < 0 {
				j = len(sc.s) - sc.i - 1
			}
			for _, b := range []byte(sc.s[sc.i+1 : sc.i+1+j]) {
				st.literal(b)
			}
			sc.i = min(len(sc.s), sc.i+j+2)
		case '"':
			sc.i++
			st.quoted = true
			sc.dquoted(&st)
			st.quoted = false
		case '$':
			sc.dollar(&st)
		case '*', '?':
			st.glob = true
			st.literal(c)
			sc.i++
		case '[':
			st.bracket = true
			st.literal(c)
			sc.i++
		case ']':
			st.glob = st.glob || st.bracket
			st.literal(c)
			sc.i++
		case '{':
			st.brace = true
			st.literal(c)
			sc.i++
		case ',':
			st.braceList = st.braceList || st.brace
			st.literal(c)
			sc.i++
		case '.':
			st.braceList = st.braceList || st.brace && strings.HasPrefix(sc.s[sc.i:], "..")
			st.literal(c)
			sc.i++
		case '}':
			st.glob = st.glob || st.braceList
			st.literal(c)
			sc.i++
		default:
			st.literal(c)
			sc.i++
		}
	}
	return done()
}

// dquoted reads the rest of a "..." string into st.
func (sc *scanner) dquoted(st *wordState) {
	for sc.i < len(sc.s) && !sc.deep {
		switch c := sc.s[sc.i]; c {
		case '\\':
			if sc.i+1 < len(sc.s) {
				st.literal(sc.s[sc.i+1])
			}
			sc.i = min(len(sc.s), sc.i+2)
		case '"':
			sc.i++
			return
		case '`':
			from := sc.i
			sc.backticks()
			st.expansion(sc.s[from:sc.i])
		case '$':
			sc.dollar(st)
		default:
			st.literal(c)
			sc.i++
		}
	}
}

// dollar reads a $ and what it expands into st: an expansion, or a
// literal $.
func (sc *scanner) dollar(st *wordState) {
	from := sc.i
	sc.i++
	if sc.i == len(sc.s) {
		st.literal('$')
		return
	}
	switch c := sc.s[sc.i]; {
	case st.quoted && (c == '\'' || c == '"'):
		// Inside "..." (or a heredoc body), $' and $" are a literal $.
		st.literal('$')
		return
	case strings.HasPrefix(sc.s[sc.i:], "(("):
		sc.arithmetic()
	case c == '(':
		sc.i++
		sc.commands(')')
	case c == '{' || c == '[':
		sc.braced(c)
	case c == '\'':
		sc.ansiC = true
		sc.skipANSIC()
	case c == '"':
		// $"..." is a translated string: as opaque as any expansion.
		sc.i++
		inner := wordState{quotedExpEnd: -1, lastSlash: -1, quoted: true}
		sc.dquoted(&inner)
	case c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
		strings.IndexByte("@*#?$!-", c) >= 0:
		sc.i++
		if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
			for sc.i < len(sc.s) && isNameByte(sc.s[sc.i]) {
				sc.i++
			}
		}
	default:
		st.literal('$')
		return
	}
	st.expansion(sc.s[from:sc.i])
}

// skipANSIC skips an ANSI-C string's body, from its opening quote and
// past its closing one; \ escapes the next byte.
func (sc *scanner) skipANSIC() {
	for sc.i++; sc.i < len(sc.s) && sc.s[sc.i] != '\''; sc.i++ {
		if sc.s[sc.i] == '\\' {
			sc.i++
		}
	}
	sc.i = min(len(sc.s), sc.i+1)
}

// heredocDelim is the delimiter a heredoc's word raw stands for, and
// whether any of it is quoted (the body then expands nothing). As in
// bash, quotes and backslashes are removed, $'...' is decoded and $"..."
// reads as "...": <<$'EOF' ends at a line EOF.
func heredocDelim(raw string) (delim string, quoted bool) {
	if !strings.ContainsAny(raw, `'"\`) {
		return raw, false
	}
	s := decodeANSIC(raw)
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				j = len(s) - i - 1
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '$' && i+1 < len(s) && s[i+1] == '"':
		case c == '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\", s[i+1]) >= 0 {
					i++
				}
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

func isNameByte(c byte) bool {
	return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

// braced reads ${...} or $[...] from its bracket, quote-aware, reading any
// $(...) or `...` in it as commands.
func (sc *scanner) braced(open byte) {
	end := byte('}')
	if open == '[' {
		end = ']'
	}
	depth := 0
	for sc.i < len(sc.s) && !sc.deep {
		switch c := sc.s[sc.i]; {
		case strings.HasPrefix(sc.s[sc.i:], "$(") && !strings.HasPrefix(sc.s[sc.i:], "$(("):
			sc.i += 2
			sc.commands(')')
			continue
		case c == '`':
			sc.backticks()
			continue
		case c == '\\':
			sc.i = min(len(sc.s), sc.i+2)
			continue
		case strings.HasPrefix(sc.s[sc.i:], "$'"):
			// Bash decodes $'...' inside ${...}, quoted or not.
			sc.ansiC = true
			sc.i++
			sc.skipANSIC()
			continue
		case c == '\'':
			if j := strings.IndexByte(sc.s[sc.i+1:], '\''); j >= 0 {
				sc.i += j + 2
			} else {
				sc.i = len(sc.s)
			}
			continue
		case c == '"':
			sc.i++
			discard := wordState{quotedExpEnd: -1, lastSlash: -1, quoted: true}
			sc.dquoted(&discard)
			continue
		case c == open:
			depth++
		case c == end:
			depth--
		}
		sc.i++
		if depth == 0 {
			return
		}
	}
}

// Variables the command binds some other way than a literal assignment.
var (
	readNames   = regexp.MustCompile(`\b(?:read|mapfile|readarray|getopts|let|declare\s+-n|local\s+-n|typeset\s+-n)\b([^;&|\n)]*)`)
	printfNames = regexp.MustCompile(`\bprintf\s+(?:-\S+\s+)*-v\s*([A-Za-z_]\w*)`)
	// loopNames is a for or select loop's variable and its word list, if
	// any. A list of literal words is resolved like an assignment.
	loopNames   = regexp.MustCompile(`\b(?:for|select)\s+([A-Za-z_]\w*)(\s+in\b([^;&|\n]*))?`)
	unknownList = regexp.MustCompile("[$`*?\\[{]")
	// substAssignment assigns a substitution's output: x=$(...), x="`...`".
	substAssignment = regexp.MustCompile("(?:^|[\\s;&|(])([A-Za-z_]\\w*)\\+?=[^\\s;&|]*?(?:\\$\\(|`)")
	arrayNames      = regexp.MustCompile(`(?:^|[\s;&|(])([A-Za-z_]\w*)\+?=\(|\b(?:declare|local|typeset|readonly)\s+-\w*[aA]\w*\s+([A-Za-z_]\w*)`)
	setPositional   = regexp.MustCompile(`\bset\s+(--|[^-\s])`)
	nameToken       = regexp.MustCompile(`[A-Za-z_]\w*`)
)

// boundVars are the variables s binds some other way than a literal
// assignment or a for loop over literal words: read, mapfile, printf -v,
// getopts, let, a nameref, for over a glob or expansion, x=$(...) or
// x=`...`, an array, and set -- for the positional parameters.
func boundVars(s string) map[string]bool {
	bound := map[string]bool{}
	for _, m := range readNames.FindAllStringSubmatch(s, -1) {
		for _, n := range nameToken.FindAllString(m[1], -1) {
			bound[n] = true
		}
	}
	for _, m := range printfNames.FindAllStringSubmatch(s, -1) {
		bound[m[1]] = true
	}
	for _, m := range loopNames.FindAllStringSubmatch(s, -1) {
		if m[2] == "" || unknownList.MatchString(m[3]) && !goList(strings.TrimSpace(m[3])) {
			bound[m[1]] = true
		}
	}
	for _, m := range substAssignment.FindAllStringSubmatch(s, -1) {
		bound[m[1]] = true
	}
	for _, m := range arrayNames.FindAllStringSubmatch(s, -1) {
		bound[m[1]+m[2]] = true
	}
	if setPositional.MatchString(s) {
		for _, n := range strings.Split("@ * # 0 1 2 3 4 5 6 7 8 9", " ") {
			bound[n] = true
		}
	}
	return bound
}

// unresolved reports whether the expansion e, as written, can't be read
// statically: it names a variable in bound, or is a ${...} with an
// operator resolve doesn't apply (/ % # :N ^ , ! subscripts ${#x}).
// Substitutions ($(...), `...`) and unbound plain variables don't count.
func unresolved(e string, bound map[string]bool) bool {
	if body, has := strings.CutPrefix(e, "${"); has {
		body = strings.TrimSuffix(body, "}")
		if strings.HasPrefix(body, "!") || strings.HasPrefix(body, "#") && len(body) > 1 {
			return true
		}
		name := paramName.FindString(body)
		rest := body[len(name):]
		return name == "" || rest != "" && !hasParamOp(rest) || bound[name]
	}
	if name, has := strings.CutPrefix(e, "$"); has && name != "" && (isNameByte(name[0]) || strings.IndexByte("@*#?$!-", name[0]) >= 0) {
		return bound[name]
	}
	return false
}

// hasParamOp reports whether rest starts with an operator param applies.
func hasParamOp(rest string) bool {
	for _, op := range paramOps {
		if strings.HasPrefix(rest, op) {
			return true
		}
	}
	return false
}
