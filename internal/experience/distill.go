package experience

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/ryanthedev/engram/internal/memory"
)

// RuleDistiller is the deterministic, directive-driven Distiller: it builds a
// candidate Experience from an `experience:` directive in the event text, so
// distillation is reproducible in tests and the local e2e stack without an LLM
// (the same role RuleExtractor plays for extraction). A real MUSE-style
// distiller would summarize the trajectory behind the same interface.
//
// Directive syntax (one per event, pipe-separated; the first two positional
// fields are task and skill, the rest are key=value):
//
//	experience: <task> | <distilled skill> | success|failure | phi=<0..1> | evidence=<e1;e2> | signals=<s1;s2> | context=<...>
//
// Events without an `experience:` line yield ok=false (skipped).
type RuleDistiller struct{}

var _ Distiller = RuleDistiller{}

// Distill implements Distiller from an `experience:` directive. ok=false when
// the text carries no such directive.
func (RuleDistiller) Distill(text string) (Experience, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "experience:") {
			continue
		}
		return parseExperienceDirective(strings.TrimPrefix(line, "experience:")), true
	}
	return Experience{}, false
}

func parseExperienceDirective(body string) Experience {
	parts := strings.Split(body, "|")
	exp := Experience{Utility: 0.5}
	for i, p := range parts {
		p = strings.TrimSpace(p)
		switch {
		case i == 0:
			exp.Task = p
		case i == 1:
			exp.DistilledSkill = p
		case p == "success":
			exp.Outcome.Success = true
		case p == "failure":
			exp.Outcome.Success = false
		case strings.HasPrefix(p, "phi="):
			if v, err := strconv.ParseFloat(strings.TrimPrefix(p, "phi="), 64); err == nil {
				exp.Utility = v
			}
		case strings.HasPrefix(p, "evidence="):
			exp.Outcome.Evidence = splitList(strings.TrimPrefix(p, "evidence="))
		case strings.HasPrefix(p, "signals="):
			exp.Outcome.Signals = splitList(strings.TrimPrefix(p, "signals="))
		case strings.HasPrefix(p, "context="):
			exp.Context = strings.TrimPrefix(p, "context=")
		}
	}
	return exp
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ";") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// DistillStage is the MUSE-style distillation worker stage (D20): after an
// event's facts land, it distills a candidate experience from the event and
// pushes it through the mandatory write-gate (Store.Admit). It is
// registered via Worker.RegisterStage from cmd/engram-server/stages_experience.go
// in its own file, with no edits to the worker core.
//
// It is idempotent (stages run at-least-once): the experience's doc id is its
// fingerprint and Admit is an upsert-merge, so a replayed event re-distills to
// the same doc rather than double-counting. A store error is returned so the
// event re-queues; a non-Admit gate verdict is NOT an error (the record is
// quarantined/rejected by design, and the event completes).
type DistillStage struct {
	distiller Distiller
	store     *Store
	logger    *slog.Logger
}

// NewDistillStage returns a stage over the distiller and gated store. logger nil
// uses slog.Default().
func NewDistillStage(distiller Distiller, store *Store, logger *slog.Logger) *DistillStage {
	if logger == nil {
		logger = slog.Default()
	}
	return &DistillStage{distiller: distiller, store: store, logger: logger}
}

// Process implements worker.Stage. It distills the event and, when the event
// carries a task experience, gates and stores it. Provenance and scope are
// taken from the source event (never the untrusted directive) so the ACL and
// audit trail stay honest.
func (s *DistillStage) Process(ctx context.Context, ev memory.Episodic, _ []memory.SemanticFact) error {
	cand, ok := s.distiller.Distill(ev.Text)
	if !ok {
		return nil // no experience in this event
	}
	// Stamp provenance/scope from the event — the writer owns these, not the
	// agent-supplied directive.
	cand.TenantID = ev.TenantID
	cand.TeamID = ev.TeamID
	cand.Scope = ev.Scope
	cand.OwnerAgentID = ev.OwnerAgentID
	cand.TrajectoryRef = ev.EventID
	cand.SourceIDs = []string{ev.EventID}
	cand.Fingerprint = Fingerprint(cand.TenantID, cand.Task)

	verdict, err := s.store.Admit(ctx, cand)
	if err != nil {
		return fmt.Errorf("experience: admitting distilled experience for %s: %w", ev.EventID, err)
	}
	s.logger.InfoContext(ctx, "experience distilled", "event_id", ev.EventID,
		"fingerprint", cand.Fingerprint, "verdict", verdict)
	return nil
}
