package model

import "testing"

func TestCompositeStatusKeepsIndependentLifecyclePhases(t *testing.T) {
	if status := CompositeStatus(RegistrationOpen, CompetitionUpcoming); status != StatusUpcoming {
		t.Fatalf("status=%s, want upcoming", status)
	}
	competition := Competition{Status: StatusOngoing}
	NormalizeLifecycle(&competition)
	if competition.RegistrationPhase != RegistrationClosed || competition.CompetitionPhase != CompetitionOngoing {
		t.Fatalf("legacy status was not migrated: %#v", competition)
	}
}
