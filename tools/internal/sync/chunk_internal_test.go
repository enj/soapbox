package sync

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/state"
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

func TestConsumerChunkBaseUsesControlPlaneGraft(t *testing.T) {
	t.Parallel()

	base := strings.Repeat("a", 40)
	overlay := strings.Repeat("b", 40)
	sourceCommit := strings.Repeat("c", 40)
	branchRef := "refs/heads/main"
	discovery := &Discovery{
		Observed: map[string]string{branchRef: overlay},
		State: state.Document{
			Cursors: []state.Cursor{{
				Ref: "refs/tags/v1.36.1", Source: sourceCommit, Destination: base,
			}},
			Published: []state.Published{{
				Ref: branchRef, Kind: state.KindBranch, Source: sourceCommit, Object: base,
			}},
		},
		ControlPlaneBranch: &ControlPlaneBranch{
			Ref: branchRef, Base: base, Object: overlay, Source: sourceCommit,
		},
	}
	position, err := consumerChunkBase(ChunkOptions{
		Config:    &config.Config{Destination: config.Destination{Branch: "main"}},
		Discovery: discovery,
	})
	if err != nil {
		t.Fatalf("consumer chunk base: %v", err)
	}
	if position.source != sourceCommit || position.destination != overlay || position.tag != "v1.36.1" {
		t.Errorf("position = %#v, want source %s graft %s tag v1.36.1", position, sourceCommit, overlay)
	}
}
