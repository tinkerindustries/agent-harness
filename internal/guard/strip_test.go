package guard

import "testing"

func TestStripHeredocs(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"none", "echo hi", "echo hi"},
		{"plain", "cat <<EOF\ngh pr merge 1\nEOF\necho done", "cat \necho done"},
		{"quoted delimiter", "cat <<'EOF'\nbody\nEOF\n", "cat \n"},
		{"dash and indented close", "cat <<-EOF\nbody\n\t EOF\nnext", "cat \nnext"},
		{"unclosed stays", "cat <<EOF\nbody\n", "cat <<EOF\nbody\n"},
		{"closing line with trailing text stays", "cat <<EOF\nbody\nEOF x\n", "cat <<EOF\nbody\nEOF x\n"},
		{"shorter prefix closes", "cat <<EOF2\nbody\nEOF\nrest", "cat \nrest"},
		{"quoted needs whole word", "cat <<'EOF2'\nbody\nEOF\n", "cat <<'EOF2'\nbody\nEOF\n"},
		{"closing at end of text", "cat <<EOF\nbody\nEOF", "cat \n"},
		{"two", "a <<A\nx\nA\nb <<B\ny\nB\n", "a \nb \n"},
	}
	for _, c := range cases {
		if got := stripHeredocs(c.in); got != c.want {
			t.Errorf("%s: stripHeredocs(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestStripQuoted(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"none", "echo hi", "echo hi"},
		{"single", "echo 'gh pr merge'", "echo ''"},
		{"double", `git commit -m "sudo rm"`, `git commit -m ''`},
		{"escaped quote inside", `echo "a \" b" c`, `echo '' c`},
		{"dash c keeps text", `bash -c "gh pr merge 1"`, `bash -c  gh pr merge 1 `},
		{"eval keeps text", `eval 'sudo ls'`, `eval  sudo ls `},
		{"word before quote blocks the match", `echo a"b"`, `echo a"b"`},
		{"unclosed", `echo "abc`, `echo "abc`},
		{"backslash newline ends the string", "echo \"a\\\nb\"", "echo \"a\\\nb\""},
		{"adjacent", `"a""b"`, `''''`},
	}
	for _, c := range cases {
		if got := stripQuoted(c.in); got != c.want {
			t.Errorf("%s: stripQuoted(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
