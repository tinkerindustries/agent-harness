package config

import (
	"bufio"
	"os"
	"strings"
)

// LoadDotEnv reads KEY=VALUE lines from path into the process environment,
// skipping keys already set so real environment variables always win. A
// missing file is not an error: .env is a local convenience and production
// deployments set the environment directly.
func LoadDotEnv(path string) error {
	values, err := DotEnvValues(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for key, value := range values {
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		os.Setenv(key, value)
	}
	return nil
}

// DotEnvValues parses path's KEY=VALUE lines and returns them without
// touching the process environment, for a caller that wants one value out of
// a file rather than the whole file made ambient — `harness gemini-session
// -env`, whose environment every command a session runs inherits.
//
// A missing file is an error here, unlike LoadDotEnv: a caller that named a
// path meant it, and the alternative is a puzzling failure later that never
// mentions the file. LoadDotEnv keeps its own tolerance by ignoring
// os.IsNotExist.
//
// The first spelling of a repeated key wins, which is what LoadDotEnv did
// when it set the environment as it read: once a key was set, the check for
// an existing value skipped every later line naming it.
func DotEnvValues(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if _, seen := values[key]; seen {
			continue
		}
		values[key] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}
