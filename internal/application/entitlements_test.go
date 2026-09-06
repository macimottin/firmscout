package application_test

import (
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
)

// TestHistoryWindowByPlan is the revert detector for D22's fail-closed rule: a plan
// name FirmScout does not recognise is windowed, exactly like anonymous and free, so a
// misspelled or future plan string can never fall through to the complete archive.
func TestHistoryWindowByPlan(t *testing.T) {
	now, err := time.Parse(time.RFC3339, "2026-09-05T00:00:00Z")
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}

	windowed := []string{application.PlanAnonymous, application.PlanFree, "nonsense"}
	for _, plan := range windowed {
		got := application.HistoryWindow(plan, now)
		want := now.AddDate(0, -application.HistoryWindowMonths, 0)
		if !got.Equal(want) {
			t.Errorf("HistoryWindow(%q) = %v, want %v", plan, got, want)
		}
		if !application.HistoryWindowed(plan) {
			t.Errorf("HistoryWindowed(%q) = false, want true", plan)
		}
	}

	unwindowed := []string{application.PlanProfessional, application.PlanEnterprise, application.PlanInternal}
	for _, plan := range unwindowed {
		got := application.HistoryWindow(plan, now)
		if !got.IsZero() {
			t.Errorf("HistoryWindow(%q) = %v, want zero time", plan, got)
		}
		if application.HistoryWindowed(plan) {
			t.Errorf("HistoryWindowed(%q) = true, want false", plan)
		}
	}
}
