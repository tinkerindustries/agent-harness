package promptvariant

import "testing"

// Cadence is in context tokens, not sub-turns: the decay this exists to fight
// is against the size of the window, and sub-turns vary enormously in what
// they add.
func TestReminderFiresOnContextGrowthNotOnSubTurns(t *testing.T) {
	p := PolicyByName("search-64k") // after 64k, every 32k

	if _, _, ok := p.Due(60_000, 0); ok {
		t.Error("reminded below the floor")
	}
	if _, _, ok := p.Due(64_000, 0); !ok {
		t.Fatal("no reminder at the floor")
	}
	// Twenty sub-turns that add almost nothing must not re-remind.
	for i := 0; i < 20; i++ {
		if _, _, ok := p.Due(70_000, 64_000); ok {
			t.Fatal("re-reminded before the context grew by the interval")
		}
	}
	if _, _, ok := p.Due(96_000, 64_000); !ok {
		t.Error("no reminder after the context grew by the interval")
	}
}

// One tool result that adds 100k should not fire a burst of reminders.
func TestReminderFiresOnceForOneLargeJump(t *testing.T) {
	p := PolicyByName("search-64k")
	if _, _, ok := p.Due(200_000, 0); !ok {
		t.Fatal("no reminder on the first large request")
	}
	if _, _, ok := p.Due(200_000, 200_000); ok {
		t.Error("reminded twice at the same context size")
	}
}

func TestNoPolicyNeverReminds(t *testing.T) {
	for _, name := range []string{"", NoReminders} {
		if _, _, ok := PolicyByName(name).Due(1_000_000, 0); ok {
			t.Errorf("policy %q reminded", name)
		}
	}
}

func TestEveryReminderPolicyIsUsable(t *testing.T) {
	for _, name := range ReminderPolicyNames() {
		if err := ValidateReminderPolicy(name); err != nil {
			t.Errorf("ValidateReminderPolicy(%q) = %v", name, err)
		}
		if ReminderPolicyDescription(name) == "" {
			t.Errorf("policy %q has no description", name)
		}
		if name == NoReminders {
			continue
		}
		p := reminderPolicies[name]
		if p.text == "" || p.everyTokens <= 0 || p.afterTokens <= 0 {
			t.Errorf("policy %q would never fire: %+v", name, p)
		}
	}
	if err := ValidateReminderPolicy("no-such-policy"); err == nil {
		t.Error("expected an unknown policy to be rejected")
	}
}

// A variant that names a reminder policy must name one this build has.
func TestVariantRemindersResolve(t *testing.T) {
	for _, name := range Names() {
		policy := ReminderPolicyFor(name)
		if policy == "" {
			continue
		}
		if err := ValidateReminderPolicy(policy); err != nil {
			t.Errorf("variant %q names %v", name, err)
		}
	}
}
