// Package experience is Engram's task-experience memory (T3) and its
// mandatory write-gate. It is a TRUST BOUNDARY against untrusted, agent-supplied
// experience: the Experience-Following result (2505.16067) shows agents obey
// retrieved experience with r≈1, so a poisoned record actively harms every
// later agent that retrieves it. The threat model is poisoning, and the defence
// is a gate with NO bypass (D5): every experience is judged before it can be
// retrieved, and a judge timeout/error is fail-closed to Quarantine — never
// Admit.
//
// Shape (mirrors internal/ingest's LLM boundary): Gatekeeper is the judge
// interface with a deterministic RuleGatekeeper (fixtures/tests) and an
// OpenAI-compatible HTTPGatekeeper (production), config-selected. Store
// is the SOLE writer of the admitted T3 index; its only write path runs the gate
// first, so a direct-index write that skips the gate is structurally impossible
// (DW-5.2). Distillation is a worker Stage (D20); retrieval is a gated
// TierSource (Phase-4 seam); a background prune job soft-expires low-utility
// records bi-temporally (never deletes — D3).
//
// Architecture boundary: this package imports internal/{auth,memory,acl,
// retrieval,worker,store,ingest,embed} as needed but no transport/framework —
// the depguard boundary holds structurally.
package experience

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Verdict is the gate's three-way decision on one experience. It is the write
// authorization for T3: only Admit reaches the retrievable index.
type Verdict string

const (
	// Admit marks an experience as evidence-backed and consistent — index it
	// into the retrievable T3 tier.
	Admit Verdict = "admit"
	// Quarantine marks an experience as unproven or self-contradictory but not
	// obviously malicious — park it in the quarantine index, never retrievable,
	// reviewable and releasable only by an explicit human CLI action. It is also
	// the fail-closed target for any judge timeout/error.
	Quarantine Verdict = "quarantine"
	// Reject marks an experience as malicious or structurally invalid (e.g. a
	// prompt-injection payload in its evidence) — dropped and audit-logged,
	// never stored.
	Reject Verdict = "reject"
)

// Valid reports whether v is one of the three known verdicts. A Gatekeeper that
// returns anything else is treated as fail-closed (Quarantine) by the store.
func (v Verdict) Valid() bool {
	return v == Admit || v == Quarantine || v == Reject
}

// Outcome is the experience's outcome evidence — the material the gate judges.
// Evidence is concrete proof the task succeeded (a passing test id, a diff, an
// observed result); an empty Evidence with Success=true is a self-reported
// success the gate must not trust (→ Quarantine). Signals are the raw outcome
// signals used to detect contradiction.
type Outcome struct {
	// Success is the agent's self-reported task outcome (untrusted).
	Success bool `json:"success"`
	// Evidence lists concrete, checkable proof of the outcome. Empty means the
	// success is self-reported only.
	Evidence []string `json:"evidence,omitempty"`
	// Signals are raw outcome signals (e.g. "tests_passed", "tests_failed");
	// mutually contradictory signals quarantine the record.
	Signals []string `json:"signals,omitempty"`
}

// Experience is one T3 task-experience record: a distilled, reusable skill
// derived from a completed task, plus the provenance/scope fields the ACL needs
// and the bi-temporal timestamps prune relies on.
type Experience struct {
	// Task is the task the experience is about (the dedup fingerprint's basis).
	Task string `json:"task"`
	// Context is the situation the skill applies to.
	Context string `json:"context,omitempty"`
	// TrajectoryRef references the source episodic event/trajectory (provenance).
	TrajectoryRef string `json:"trajectory_ref,omitempty"`
	// Outcome is the outcome evidence the gate judges.
	Outcome Outcome `json:"outcome"`
	// Utility Φ is the experience's estimated usefulness in [0,1]; low Φ + low
	// RetrievalCount is what the prune job soft-expires.
	Utility float64 `json:"utility"`
	// DistilledSkill is the reusable skill text — the BM25 retrieval target.
	DistilledSkill string `json:"distilled_skill"`
	// Fingerprint = sha256(tenant · normalized-task); the dedup key and doc id.
	Fingerprint string `json:"fingerprint"`
	// RetrievalCount counts how often the admitted experience was retrieved; it
	// feeds the prune signal and the following-correlation metric.
	RetrievalCount int `json:"retrieval_count"`
	// MergedCount counts how many duplicate distillations folded into this one
	// (dedup accounting — same fingerprint merges, never double-counts).
	MergedCount int `json:"merged_count"`

	// Tenancy / provenance / scope (D16/D6) — carried onto every hit so the
	// retriever re-verifies the experience through the ACL predicate.
	TenantID     string   `json:"tenant_id"`
	TeamID       string   `json:"team_id,omitempty"`
	Scope        string   `json:"scope"`
	OwnerAgentID string   `json:"owner_agent_id"`
	SourceIDs    []string `json:"source_ids,omitempty"`

	// Bi-temporal timestamps (D3). ValidAt/CreatedAt are set on admit; ExpiredAt
	// is nil while live and set (never the doc deleted) when prune soft-expires.
	ValidAt   time.Time  `json:"valid_at"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiredAt *time.Time `json:"expired_at,omitempty"`
}

// Live reports whether the experience is currently retrievable (admitted and
// not soft-expired).
func (e Experience) Live() bool { return e.ExpiredAt == nil }

// Quarantined is a parked experience plus why the gate quarantined it — the
// `engram quarantine list` surface and the release input.
type Quarantined struct {
	Experience Experience `json:"experience"`
	// Verdict is the gate verdict that parked it (always Quarantine here; kept
	// explicit for audit).
	Verdict Verdict `json:"verdict"`
	// Reason is the human-readable gate reason.
	Reason string `json:"reason"`
	// At is when it was quarantined.
	At time.Time `json:"at"`
}

// Gatekeeper judges one experience at the T3 trust boundary. Implementations
// MUST be fail-closed: any inability to reach a confident Admit — a judge
// timeout, transport error, unparseable response, or an unrecognized verdict —
// MUST resolve to Quarantine (or Reject), NEVER Admit. RuleGatekeeper is the
// deterministic fixture judge; HTTPGatekeeper is the OpenAI-compatible LLM judge.
type Gatekeeper interface {
	// Evaluate returns the verdict for exp and a human-readable reason. A non-nil
	// error means the judge itself failed; the store treats that as fail-closed
	// Quarantine (the error is surfaced for logging, not to admit on failure).
	Evaluate(ctx context.Context, exp Experience) (verdict Verdict, reason string, err error)
}

// Distiller turns a completed task (its event + landed facts) into a candidate
// Experience, or reports that the event carries no distillable experience.
// RuleDistiller is the deterministic directive-driven implementation used in
// tests and the local e2e stack; a real distiller would be LLM-backed behind
// the same interface.
type Distiller interface {
	// Distill returns the candidate experience and ok=true when the event
	// contains a distillable task experience; ok=false skips the event.
	Distill(text string) (exp Experience, ok bool)
}

// normalizeTask lowercases and collapses whitespace so trivially-different
// phrasings of the same task fingerprint identically (dedup — DW edge case).
func normalizeTask(task string) string {
	return strings.Join(strings.Fields(strings.ToLower(task)), " ")
}

// Fingerprint returns the deterministic dedup key / doc id for a task under a
// tenant: identical tasks collapse to one doc so a duplicate experience merges
// rather than double-counting.
func Fingerprint(tenantID, task string) string {
	sum := sha256.Sum256([]byte(tenantID + "\x1f" + normalizeTask(task)))
	return hex.EncodeToString(sum[:])
}
