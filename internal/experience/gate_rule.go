package experience

import (
	"context"
	"strings"
)

// RuleGatekeeper is the deterministic, fixture-driven Gatekeeper: it judges an
// experience by explicit evidence rules rather than a live model, so the gate's
// admit/quarantine/reject behaviour is reproducible in unit tests and the local
// e2e stack (the same role RuleExtractor plays for extraction). Production swaps
// in HTTPGatekeeper behind the identical interface.
//
// The rules encode the threat model (poisoning): a record is only Admitted when
// it is evidence-backed, internally consistent, and free of injection payloads.
// Everything unproven is Quarantined; everything malicious or structurally
// invalid is Rejected. This is fail-closed by construction — the default arm is
// Quarantine, never Admit.
type RuleGatekeeper struct{}

var _ Gatekeeper = RuleGatekeeper{}

// injectionMarkers are substrings that betray a prompt-injection / poisoning
// payload smuggled into an experience's evidence, skill, or context. Their
// presence Rejects the record outright — this is untrusted input at a trust
// boundary, so the barricade rejects rather than sanitizes.
var injectionMarkers = []string{
	"ignore previous", "ignore all previous", "disregard the", "disregard all",
	"you are now", "system prompt", "exfiltrate", "reveal your", "curl http",
	"rm -rf", "<<poison>>", "override the gate", "bypass the gate",
}

// passSignals and failSignals are the contradictory outcome-signal families:
// an experience asserting both a pass-family and a fail-family signal is
// self-contradictory and cannot be trusted (→ Quarantine).
var (
	passSignals = []string{"pass", "passed", "success", "succeeded", "green", "ok"}
	failSignals = []string{"fail", "failed", "error", "errored", "red", "crash"}
)

// Evaluate implements Gatekeeper deterministically. It never returns a non-nil
// error: the rule judge cannot "time out", and every uncertain path resolves to
// Quarantine, so callers get a definite fail-closed verdict.
func (RuleGatekeeper) Evaluate(_ context.Context, exp Experience) (Verdict, string, error) {
	// Reject: structurally invalid or malicious. These never reach storage.
	if strings.TrimSpace(exp.Task) == "" {
		return Reject, "empty task: structurally invalid experience", nil
	}
	if exp.Utility < 0 || exp.Utility > 1 {
		return Reject, "utility out of [0,1]: fabricated score", nil
	}
	if marker, bad := containsInjection(exp); bad {
		return Reject, "injection/poison marker in payload: " + marker, nil
	}

	// Quarantine: unproven or self-contradictory but not obviously malicious.
	if strings.TrimSpace(exp.DistilledSkill) == "" {
		return Quarantine, "no distilled skill to retrieve", nil
	}
	if contradictorySignals(exp.Outcome.Signals) {
		return Quarantine, "contradictory outcome signals", nil
	}
	if !exp.Outcome.Success {
		return Quarantine, "outcome not reported successful", nil
	}
	if !hasEvidence(exp.Outcome) {
		return Quarantine, "self-reported success with no outcome evidence", nil
	}

	// Admit: evidence-backed, consistent, and clean.
	return Admit, "evidence-backed successful outcome", nil
}

// hasEvidence reports whether the outcome carries at least one non-blank piece
// of concrete evidence.
func hasEvidence(o Outcome) bool {
	for _, e := range o.Evidence {
		if strings.TrimSpace(e) != "" {
			return true
		}
	}
	return false
}

// containsInjection scans the free-text fields an attacker controls for an
// injection marker.
func containsInjection(exp Experience) (string, bool) {
	hay := strings.ToLower(strings.Join(append([]string{
		exp.DistilledSkill, exp.Context, exp.Task,
	}, exp.Outcome.Evidence...), "\n"))
	for _, m := range injectionMarkers {
		if strings.Contains(hay, m) {
			return m, true
		}
	}
	return "", false
}

// contradictorySignals reports whether the signal set contains both a
// pass-family and a fail-family signal.
func contradictorySignals(signals []string) bool {
	var sawPass, sawFail bool
	for _, s := range signals {
		l := strings.ToLower(strings.TrimSpace(s))
		if anyContains(l, passSignals) {
			sawPass = true
		}
		if anyContains(l, failSignals) {
			sawFail = true
		}
	}
	return sawPass && sawFail
}

func anyContains(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
