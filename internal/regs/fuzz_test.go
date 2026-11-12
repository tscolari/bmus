package regs

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Classify and opaqueCommand terminate on any input, without panicking.
func FuzzClassify(f *testing.F) {
	for _, s := range []string{"$(${x:-'}'})", "cat <<EOF\n$x\nEOF", "`a\\`b\\``", "x=$x$x; $x", "{a,b}$[1]$'x'",
		"((1<<2))\n$x", strings.Repeat("find -exec ", 30), "case a in a) $(case b in b) x;; esac);; esac", "/usr/bin/@(g)it", "find . -exec sudo rm {} +",
		"git $'\\x70\\160\\u0261\\U1\\cA\\'\\x'", "go test $(go list ./... | grep -v -e x) `go list`", `rm "$x" ${y:-z} $"q" "$(a)"`,
		"o=-x; go test $o; for b in a b; do git checkout $b; done",
		"grep 'a$' \"$'\" \\$' $'\\c' $'\\c\\\\' $'p\\x00\\'q'", "git -c core$N.pager+=x; for p in $(go list ./...); do rm $HOME/$p; done",
		"# don't 'a$'\ngit $'\\x70ush'", "cat <<E\nit's\nE\necho \"$(git $'\\x70')\" \"${x:-$'\\x70'}\" \"`$'\\x70'`\"",
		"cat <<$'E\\x4f'\nx\nEO\nx=$'-c' su $x; for y in $'a'; do echo > $'b' \"$'\" ${z:+$'c'}; done", "cat <<$\"E\"\n$'x\nE"} {
		f.Add(s)
	}
	var c Config
	if err := yaml.Unmarshal(Default, &c); err != nil {
		f.Fatal(err)
	}
	if err := CheckRegs(&c); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, s string) {
		opaqueCommand(s, nil, nil)
		c.Classify(Hail{Category: CategoryToolApproval, Tool: "Bash", Command: s})
	})
}
