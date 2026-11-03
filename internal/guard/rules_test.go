package guard

import (
	"reflect"
	"testing"
)

func checkRules(t *testing.T, r Rules, command, branch string) []string {
	t.Helper()
	return r.CheckRules(command, branch)
}

func TestRulesCommandStartPositions(t *testing.T) {
	r := mustBundle(t).Rules()
	for name, command := range map[string]string{
		"start of text":         "elevate now",
		"after a space":         "echo hi elevate now",
		"after a tab":           "echo\televate now",
		"after a newline":       "echo hi\nelevate now",
		"after a semicolon":     "echo hi;elevate now",
		"after an ampersand":    "echo hi&&elevate now",
		"after a pipe":          "echo hi|elevate now",
		"after a paren":         "(elevate now)",
		"after a backtick":      "echo `elevate now`",
		"after a unicode space": "echo elevate now",
	} {
		if got := checkRules(t, r, command, ""); !reflect.DeepEqual(got, []string{"elevate"}) {
			t.Errorf("%s: %q gave %v", name, command, got)
		}
	}
	for name, command := range map[string]string{
		"inside a word":    "preelevate now",
		"after a hyphen":   "x-elevate now",
		"after a dollar":   "echo $elevate now",
		"after a letter":   "elevated now",
		"no space after":   "elevate",
		"in single quotes": "echo 'elevate now'",
		"in double quotes": `echo "elevate now"`,
		"in a heredoc":     "cat <<EOF\nelevate now\nEOF",
	} {
		if got := checkRules(t, r, command, ""); len(got) != 0 {
			t.Errorf("%s: %q gave %v", name, command, got)
		}
	}
}

func TestRulesMacros(t *testing.T) {
	r := mustBundle(t).Rules()
	hit := func(command string, want ...string) {
		t.Helper()
		got := checkRules(t, r, command, "")
		if len(want) == 0 {
			want = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q gave %v, want %v", command, got, want)
		}
	}
	// space: tabs, runs of spaces and a no-break space all separate words.
	hit("deploy prod", "deploy_prod")
	hit("deploy \t  prod", "deploy_prod")
	hit("deploy prod", "deploy_prod")
	hit("deployprod")
	// word_end: the end of the text or any non-word character.
	hit("deploy prod", "deploy_prod")
	hit("deploy prod.", "deploy_prod")
	hit("deploy prod;", "deploy_prod")
	hit("deploy prod now", "deploy_prod")
	hit("deploy products")
	hit("deploy prod_2")
	hit("deploy prodé")
	hit("deploy prod٣")
	// word: relay needs one or more word characters before the @.
	hit("relay alice@host", "remote_copy")
	hit("relay @host")
	hit("relay élève@host", "remote_copy")
	// word_chars inside a bracket class, and space_chars through space.
	hit("beamto -r file user@host.example:/tmp", "remote_copy")
	hit("beamto -r file ./local")
}

func TestRulesSeveralPatternsOneName(t *testing.T) {
	r := mustBundle(t).Rules()
	for _, command := range []string{"deploy prod", "ship it", "ship it && deploy prod"} {
		if got := checkRules(t, r, command, ""); !reflect.DeepEqual(got, []string{"deploy_prod"}) {
			t.Errorf("%q gave %v", command, got)
		}
	}
}

func TestRulesOrderFollowsTheTableWithoutRepeats(t *testing.T) {
	r := mustBundle(t).Rules()
	got := checkRules(t, r, "settimer 5; elevate x; ship it; deploy prod; relay a@b", "")
	want := []string{"deploy_prod", "elevate", "remote_copy", "timer"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRulesException(t *testing.T) {
	r := mustBundle(t).Rules()
	for command, want := range map[string]bool{
		"settimer 5":              true,
		"settimer -x":             true,
		"settimer -qx":            true,
		"settimer -q5":            true,
		"settimer -q":             false,
		"settimer -q 5":           false,
		"settimer -q;":            false,
		"settimer ":               false,
		"settimer":                false,
		"settimer \t -q":          false,
		"settimer -q_":            true,
		"settimer -Q":             true,
		"x; settimer -q":          false,
		"settimer -q; settimer 9": true,
	} {
		got := len(checkRules(t, r, command, "")) > 0
		if got != want {
			t.Errorf("%q: hit = %v, want %v", command, got, want)
		}
	}
}

func TestRulesDefaultBranches(t *testing.T) {
	r := mustBundle(t).Rules()
	cases := []struct {
		command, branch string
		hit             bool
	}{
		{"git push origin trunk", "work", true},
		{"git push origin stable", "work", true},
		{"git push origin HEAD:trunk", "work", true},
		{"git push origin +work:refs/heads/stable", "work", true},
		{"git push origin work", "work", false},
		{"git push", "trunk", true},
		{"git push", "work", false},
		{"git push --all", "work", true},
		{"git push origin main", "work", false},
		{"git status", "trunk", false},
	}
	for _, tc := range cases {
		got := checkRules(t, r, tc.command, tc.branch)
		if hit := reflect.DeepEqual(got, []string{RulePushDefaultBranch}); hit != tc.hit {
			t.Errorf("%q on %q gave %v", tc.command, tc.branch, got)
		}
	}
}

func TestRulesEmptyDefaultBranchesTurnsPushCheckOff(t *testing.T) {
	r := mustBundle(t, func(b map[string]any) { section(b, "rules")["default_branches"] = []any{} }).Rules()
	for _, command := range []string{"git push origin trunk", "git push --all", "git push"} {
		if got := checkRules(t, r, command, "trunk"); len(got) != 0 {
			t.Errorf("%q gave %v", command, got)
		}
	}
	if got := checkRules(t, r, "elevate x", "trunk"); !reflect.DeepEqual(got, []string{"elevate"}) {
		t.Errorf("table rules stopped: %v", got)
	}
}

func TestRulesEmptyTableAndZeroValue(t *testing.T) {
	empty := mustBundle(t, func(b map[string]any) { section(b, "rules")["table"] = []any{} }).Rules()
	if got := empty.CheckRules("elevate x; git push origin trunk", ""); !reflect.DeepEqual(got, []string{RulePushDefaultBranch}) {
		t.Errorf("got %v", got)
	}
	if got := (Rules{}).CheckRules("elevate x; git push origin trunk", "trunk"); len(got) != 0 {
		t.Errorf("zero value gave %v", got)
	}
}

func TestMacrosNestAndUndefinedAreRefused(t *testing.T) {
	ok := mustBundle(t, func(b map[string]any) {
		m := section(b, "rules", "macros")
		m["a"] = "x"
		m["b"] = "${a}${a}"
		m["c"] = "${b}y"
		setMatch(b, 1, 0, "elevate${c}${space}")
	}).Rules()
	if got := ok.CheckRules("elevatexxy ", ""); !reflect.DeepEqual(got, []string{"elevate"}) {
		t.Errorf("nested macros gave %v", got)
	}
}
