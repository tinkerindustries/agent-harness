package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	contents := "# a comment\n\nDOTENV_TEST_A=hello\nDOTENV_TEST_B=\"quoted value\"\nDOTENV_TEST_C='single quoted'\n"
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Cleanup(func() {
		os.Unsetenv("DOTENV_TEST_A")
		os.Unsetenv("DOTENV_TEST_B")
		os.Unsetenv("DOTENV_TEST_C")
	})

	if err := LoadDotEnv(path); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}

	for key, want := range map[string]string{
		"DOTENV_TEST_A": "hello",
		"DOTENV_TEST_B": "quoted value",
		"DOTENV_TEST_C": "single quoted",
	} {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestLoadDotEnvDoesNotOverrideExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("DOTENV_TEST_EXISTING=from_file\n"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	os.Setenv("DOTENV_TEST_EXISTING", "from_environment")
	t.Cleanup(func() { os.Unsetenv("DOTENV_TEST_EXISTING") })

	if err := LoadDotEnv(path); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if got := os.Getenv("DOTENV_TEST_EXISTING"); got != "from_environment" {
		t.Errorf("existing env var overwritten: got %q, want from_environment", got)
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	if err := LoadDotEnv("/nonexistent/.env"); err != nil {
		t.Errorf("LoadDotEnv of missing file: want nil error, got %v", err)
	}
}

func TestDotEnvValuesDoesNotTouchTheEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("GEMINI_API_KEY=AI-from-file\n# comment\nOTHER=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("GEMINI_API_KEY")

	values, err := DotEnvValues(path)
	if err != nil {
		t.Fatalf("DotEnvValues: %v", err)
	}
	if values["GEMINI_API_KEY"] != "AI-from-file" || values["OTHER"] != "x" {
		t.Errorf("values = %v", values)
	}
	// The point of the function: a caller can read one value out of a file
	// without the rest of it becoming ambient for every process it spawns.
	if v, set := os.LookupEnv("GEMINI_API_KEY"); set {
		t.Errorf("DotEnvValues set GEMINI_API_KEY in the environment (%q); it must only parse", v)
	}
}

// The first spelling of a repeated key wins, matching what LoadDotEnv did
// when it set each value as it read.
func TestDotEnvValuesKeepsTheFirstOfARepeatedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("K=first\nK=second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := DotEnvValues(path)
	if err != nil {
		t.Fatalf("DotEnvValues: %v", err)
	}
	if values["K"] != "first" {
		t.Errorf("K = %q, want first", values["K"])
	}
}

// A missing file is an error here, where LoadDotEnv treats it as absent: the
// caller named the path, so a typo must be reported rather than turned into
// a later failure that never mentions the file.
func TestDotEnvValuesMissingFileIsAnError(t *testing.T) {
	if _, err := DotEnvValues(filepath.Join(t.TempDir(), "nope.env")); err == nil {
		t.Error("DotEnvValues of a missing file: want an error, got nil")
	}
}
