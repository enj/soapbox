package sync

import (
	"errors"
	"testing"
	"time"
)

func TestWorkflowBudget(t *testing.T) {
	now := time.Date(2026, 8, 13, 4, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		budget    WorkflowBudget
		exhausted bool
	}{
		{name: "no deadline"},
		{
			name: "room for another chunk",
			budget: WorkflowBudget{
				Deadline: now.Add(20 * time.Minute), Reserve: 5 * time.Minute,
				Now: func() time.Time { return now },
			},
		},
		{
			name: "reserve reaches deadline",
			budget: WorkflowBudget{
				Deadline: now.Add(5 * time.Minute), Reserve: 5 * time.Minute,
				Now: func() time.Time { return now },
			},
			exhausted: true,
		},
		{
			name: "deadline passed",
			budget: WorkflowBudget{
				Deadline: now.Add(-time.Second),
				Now:      func() time.Time { return now },
			},
			exhausted: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.budget.Check()
			if test.exhausted != errors.Is(err, ErrWorkflowBudget) {
				t.Fatalf("Check = %v, exhausted=%v", err, test.exhausted)
			}
		})
	}
}
