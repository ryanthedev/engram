package experience

import (
	"context"
	"testing"

	"github.com/ryanthedev/engram/internal/memory"
)

func experienceEvent(tenant, eventID, text string) memory.Episodic {
	return memory.Episodic{
		EventID: eventID, TenantID: tenant, OwnerAgentID: "a1", Scope: "private", Text: text,
	}
}

// TestDistill_DirectiveToAdmit: the stage distills an `experience:` directive,
// gates it, and admits an evidence-backed success — provenance is taken from the
// event, not the directive.
func TestDistill_DirectiveToAdmit(t *testing.T) {
	store, _ := NewStore(RuleGatekeeper{}, NewMemBackend(), nil)
	stage := NewDistillStage(RuleDistiller{}, store, nil)
	ctx := context.Background()

	ev := experienceEvent("t1", "e1",
		"experience: speed up search | precompute the RRF pipeline | success | phi=0.8 | evidence=p95 dropped;trace#9 | signals=tests_passed")
	if err := stage.Process(ctx, ev, nil); err != nil {
		t.Fatalf("stage.Process: %v", err)
	}
	hits, _ := store.SearchAdmitted(ctx, "t1", "search RRF", 10)
	if len(hits) != 1 {
		t.Fatalf("distilled experience not admitted; %d hits", len(hits))
	}
	if hits[0].OwnerAgentID != "a1" || hits[0].TrajectoryRef != "e1" {
		t.Fatalf("provenance not taken from event: %+v", hits[0])
	}
}

// TestDistill_NonExperienceEventIsNoop: an event with no `experience:` directive
// is skipped cleanly.
func TestDistill_NonExperienceEventIsNoop(t *testing.T) {
	backend := NewMemBackend()
	store, _ := NewStore(RuleGatekeeper{}, backend, nil)
	stage := NewDistillStage(RuleDistiller{}, store, nil)
	ev := experienceEvent("t1", "e1", "fact: alice | prefers | dark-mode")
	if err := stage.Process(context.Background(), ev, nil); err != nil {
		t.Fatalf("noop stage errored: %v", err)
	}
	if hits, _ := store.SearchAdmitted(context.Background(), "t1", "alice", 10); len(hits) != 0 {
		t.Fatalf("non-experience event produced an experience")
	}
}

// TestDistill_Idempotent: replaying the same event (at-least-once) re-distills
// to the same fingerprint and merges rather than double-counting.
func TestDistill_Idempotent(t *testing.T) {
	store, _ := NewStore(RuleGatekeeper{}, NewMemBackend(), nil)
	stage := NewDistillStage(RuleDistiller{}, store, nil)
	ctx := context.Background()
	ev := experienceEvent("t1", "e1",
		"experience: fix the leak | close the reader | success | phi=0.7 | evidence=heap flat")
	for i := 0; i < 3; i++ {
		if err := stage.Process(ctx, ev, nil); err != nil {
			t.Fatal(err)
		}
	}
	hits, _ := store.SearchAdmitted(ctx, "t1", "leak reader", 10)
	if len(hits) != 1 {
		t.Fatalf("replayed event produced %d experiences; want 1 (idempotent)", len(hits))
	}
}

// TestDistill_PoisonedDirectiveQuarantined: a distilled experience that is a
// self-reported success with no evidence is quarantined, and the stage still
// completes without error (a non-Admit verdict is by design, not a failure).
func TestDistill_PoisonedDirectiveQuarantined(t *testing.T) {
	backend := NewMemBackend()
	store, _ := NewStore(RuleGatekeeper{}, backend, nil)
	stage := NewDistillStage(RuleDistiller{}, store, nil)
	ctx := context.Background()
	ev := experienceEvent("t1", "e1", "experience: trust me | it works | success | phi=0.9")
	if err := stage.Process(ctx, ev, nil); err != nil {
		t.Fatalf("stage errored on a quarantine verdict; want clean completion: %v", err)
	}
	if hits, _ := store.SearchAdmitted(ctx, "t1", "trust", 10); len(hits) != 0 {
		t.Fatalf("poisoned directive became retrievable")
	}
	if q, _ := store.ListQuarantine(ctx, "t1"); len(q) != 1 {
		t.Fatalf("poisoned directive not quarantined; %d in quarantine", len(q))
	}
}

// TestIsExperienceDirective pins the directive-line detector the CLI ingest
// advisory (internal/cli) relies on: true only for the experience: prefix,
// false on plain prose.
func TestIsExperienceDirective(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"experience prefix", "experience: fix bug | patch the reader | success", true},
		{"leading/trailing whitespace tolerated", "   experience: t | s | success   ", true},
		{"plain prose", "the experience was pretty good overall", false},
		{"empty line", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsExperienceDirective(tt.line); got != tt.want {
				t.Errorf("IsExperienceDirective(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestParseExperienceLine pins well-formed vs malformed experience: lines
// using the SAME parser Distill runs, so the CLI advisory can never disagree
// with what actually gets distilled. Well-formed requires both positional
// fields (task, distilled skill) to be non-empty.
func TestParseExperienceLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"well-formed, full grammar", "experience: fix the leak | close the reader | success | phi=0.7", true},
		{"well-formed, task and skill only", "experience: fix bug | patch code", true},
		{"malformed: no pipe at all (prose after prefix)", "experience: just a note with no pipes", false},
		{"malformed: skill field empty", "experience: fix bug | ", false},
		{"non-directive line", "the experience was memorable", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseExperienceLine(tt.line); got != tt.want {
				t.Errorf("ParseExperienceLine(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}
