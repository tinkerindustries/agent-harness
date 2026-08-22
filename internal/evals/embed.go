package evals

import (
	"embed"
	"fmt"
	"path"
	"sort"
)

// suites/ holds the suites this build can run. They are embedded rather than
// read from disk because `harness serve` runs in a container that carries the
// binary and nothing else, and the HTTP surface has to be able to say which
// suites exist. A suite written outside the build can still be parsed with
// LoadSuite, exercised today only by this package's own tests.
//
//go:embed suites/*.json
var suiteFS embed.FS

// EmbeddedSuites are the built-in suites, by name. A suite that fails to parse
// is a build-time mistake caught by the package's own test, not something a
// caller has to handle.
func EmbeddedSuites() []*Suite {
	entries, err := suiteFS.ReadDir("suites")
	if err != nil {
		return nil
	}
	var out []*Suite
	for _, e := range entries {
		suite, err := embeddedSuite(e.Name())
		if err != nil {
			continue
		}
		out = append(out, suite)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// EmbeddedSuite loads one built-in suite by its name.
func EmbeddedSuite(name string) (*Suite, error) {
	for _, suite := range EmbeddedSuites() {
		if suite.Name == name {
			return suite, nil
		}
	}
	return nil, fmt.Errorf("evals: no built-in suite named %q", name)
}

func embeddedSuite(file string) (*Suite, error) {
	b, err := suiteFS.ReadFile(path.Join("suites", file))
	if err != nil {
		return nil, err
	}
	return parseSuite(b, file)
}
