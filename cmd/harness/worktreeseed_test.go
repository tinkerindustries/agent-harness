package main

import (
	"fmt"
	"slices"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/settings"
)

// TestPlanSeedLeavesLocalValuesAlone pins the contract that makes seeding
// safe to re-run: a credential already set in this worktree is not replaced
// by the source's copy, because an operator who set a different key here
// meant it. -overwrite is the way to say otherwise.
func TestPlanSeedLeavesLocalValuesAlone(t *testing.T) {
	keys := []string{"deepseek.api_key", "github.app_id", "github.app_private_key"}
	source := map[string]string{
		"deepseek.api_key": "sk-source",
		"github.app_id":    "4745645",
	}
	alreadySet := map[string]bool{"deepseek.api_key": true}

	got := planSeed(keys, source, alreadySet, false)
	want := []seedPlan{
		{"deepseek.api_key", seedActionHave},
		{"github.app_id", seedActionWrite},
		{"github.app_private_key", seedActionNotSource},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("planSeed =\n  %v\nwant\n  %v", got, want)
	}

	overwritten := planSeed(keys, source, alreadySet, true)
	if overwritten[0].Action != seedActionWrite {
		t.Errorf("with -overwrite, %s = %q, want it seeded", overwritten[0].Key, overwritten[0].Action)
	}
}

// TestDefaultSourceContainerFollowsComposeNaming pins the derivation of the
// main checkout's container name from its directory, which is how the source
// is found when -from is not given.
func TestDefaultSourceContainerFollowsComposeNaming(t *testing.T) {
	for path, want := range map[string]string{
		"/Users/geoff/Repos/agent-harness": "agent-harness-harness-1",
		"/srv/Agent_Harness":               "agent_harness-harness-1",
		"/srv/my.repo":                     "myrepo-harness-1",
	} {
		if got := defaultSourceContainer(path); got != want {
			t.Errorf("defaultSourceContainer(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestSeedableCredentialKeysExcludesTheControlToken pins the one credential
// that must not travel between installations: it guards that installation's
// own run-control endpoints, so a copy would make one stack's bearer token
// work on another.
func TestSeedableCredentialKeysExcludesTheControlToken(t *testing.T) {
	keys := settings.SeedableCredentialKeys()
	if slices.Contains(keys, settings.KeyHTTPControlToken) {
		t.Errorf("%s is seedable; it must not be", settings.KeyHTTPControlToken)
	}
	for _, want := range []string{
		settings.KeyDeepSeekAPIKey,
		settings.KeyKimiAPIKey,
		settings.KeyGoogleAPIKey,
		settings.KeyGitHubToken,
		settings.KeyGitHubAppID,
		settings.KeyGitHubAppPrivateKey,
	} {
		if !slices.Contains(keys, want) {
			t.Errorf("%s is not seedable; a new worktree would have to be given it by hand", want)
		}
	}
}
