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
