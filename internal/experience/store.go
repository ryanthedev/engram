package experience

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNoGate is returned by NewStore when no Gatekeeper is supplied.
// A T3 store without a gate would be an ungated write path to retrievable
// memory — exactly the bypass D5 forbids — so construction fails loudly rather
// than silently admitting everything.
var ErrNoGate = errors.New("experience: a Gatekeeper is mandatory (no ungated T3 store)")

// ErrNotFound is returned by Release/Recover when the target does not exist.
var ErrNotFound = errors.New("experience: not found")

// Backend is the persistence port Store depends on: two logical
// indices (admitted T3 + quarantine) behind a narrow interface. The in-memory
// MemBackend backs unit tests and the dev/e2e stack; OpenSearchBackend backs
// production (dependency inversion — the arrow points from infrastructure to
// this package, like acl.EdgeSource). The Backend performs NO gating; the gate
// lives above it in Store, so no Backend method is a write path a
// caller could reach without the gate.
type Backend interface {
	// PutAdmitted upserts exp into the admitted (retrievable) T3 index, keyed by
	// its Fingerprint.
	PutAdmitted(ctx context.Context, exp Experience) error
	// GetAdmitted returns the admitted experience for (tenant, fingerprint).
	// includeExpired=true returns soft-expired docs too (bi-temporal recovery);
	// false returns only live ones.
	GetAdmitted(ctx context.Context, tenantID, fingerprint string, includeExpired bool) (Experience, bool, error)
	// SearchAdmitted returns up to k LIVE admitted experiences matching query in
	// tenant, best-first. Soft-expired docs are excluded.
	SearchAdmitted(ctx context.Context, tenantID, query string, k int) ([]Experience, error)
	// ScanPrunable returns LIVE admitted experiences with Utility <= phiMax AND
	// RetrievalCount <= retrievalMax — the prune candidates.
	ScanPrunable(ctx context.Context, phiMax float64, retrievalMax int) ([]Experience, error)
	// SoftExpire stamps expired_at on the admitted doc (never deletes — D3).
	SoftExpire(ctx context.Context, tenantID, fingerprint string, at time.Time) error

	// PutQuarantine upserts q into the quarantine index (never retrievable).
	PutQuarantine(ctx context.Context, q Quarantined) error
	// ListQuarantine returns every quarantined record for tenant.
	ListQuarantine(ctx context.Context, tenantID string) ([]Quarantined, error)
	// GetQuarantine returns the quarantined record for (tenant, fingerprint).
	GetQuarantine(ctx context.Context, tenantID, fingerprint string) (Quarantined, bool, error)
	// DeleteQuarantine removes a quarantined record (on release).
	DeleteQuarantine(ctx context.Context, tenantID, fingerprint string) error
}

// Store is the SOLE writer of the admitted T3 index and the mandatory
// write-gate's enforcement point (D5). Its only path to the retrievable index —
// Admit — runs the Gatekeeper FIRST and indexes into the admitted store only on
// a clean Admit verdict; a Quarantine verdict (or ANY gate error, fail-closed)
// routes to the quarantine index, and a Reject drops the record. There is no
// method that writes the admitted index without passing the gate, so a
// direct-index write that skips the gate is structurally impossible.
//
// It is a deep module: callers see intent methods (Admit, ListQuarantine,
// Release, SearchAdmitted, Prune, Recover) and never the two index names, the
// dedup-by-fingerprint merge, the quarantine routing, or the soft-expire
// bi-temporal recovery it hides.
type Store struct {
	gate    Gatekeeper
	backend Backend
	logger  *slog.Logger
	now     func() time.Time
}

// NewStore returns a store over gate and backend. gate is MANDATORY:
// a nil gate returns ErrNoGate (there is no such thing as an ungated T3 store).
// logger nil uses slog.Default().
func NewStore(gate Gatekeeper, backend Backend, logger *slog.Logger) (*Store, error) {
	if gate == nil {
		return nil, ErrNoGate
	}
	if backend == nil {
		return nil, errors.New("experience: backend is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{gate: gate, backend: backend, logger: logger, now: time.Now}, nil
}

// Admit is the ONE write path to T3. It stamps the fingerprint, runs the
// mandatory gate, and routes by verdict:
//   - Admit  → dedup-merge into the admitted (retrievable) index;
//   - Quarantine (or ANY gate error — fail-closed) → the quarantine index;
//   - Reject → dropped and audit-logged, never stored.
//
// It returns the effective verdict so callers/tests can assert on it. A gate
// error is folded into a Quarantine verdict (never surfaced as an admit) and
// logged — the trust boundary must not let a broken judge admit anything.
func (s *Store) Admit(ctx context.Context, exp Experience) (Verdict, error) {
	if exp.TenantID == "" {
		return Reject, fmt.Errorf("experience: cannot admit a record with no tenant")
	}
	exp = s.stamp(exp)

	verdict, reason, gerr := s.gate.Evaluate(ctx, exp)
	if gerr != nil {
		// Fail-closed: a judge failure quarantines, never admits.
		s.logger.WarnContext(ctx, "experience gate failed; fail-closed to quarantine",
			"fingerprint", exp.Fingerprint, "err", gerr)
		verdict, reason = Quarantine, "fail-closed: "+reason
	}
	if !verdict.Valid() {
		// An implementation that returns an unknown verdict is treated as
		// fail-closed too — Admit is never the default.
		verdict, reason = Quarantine, "fail-closed: unknown verdict"
	}

	switch verdict {
	case Admit:
		if err := s.admitMerged(ctx, exp); err != nil {
			return Admit, err
		}
		return Admit, nil
	case Quarantine:
		q := Quarantined{Experience: exp, Verdict: Quarantine, Reason: reason, At: s.now().UTC()}
		if err := s.backend.PutQuarantine(ctx, q); err != nil {
			return Quarantine, fmt.Errorf("experience: quarantining %s: %w", exp.Fingerprint, err)
		}
		s.logger.InfoContext(ctx, "experience quarantined", "fingerprint", exp.Fingerprint, "reason", reason)
		return Quarantine, nil
	default: // Reject
		s.logger.WarnContext(ctx, "experience rejected (dropped)", "fingerprint", exp.Fingerprint, "reason", reason)
		return Reject, nil
	}
}

// admitMerged upserts an admitted experience, merging a same-fingerprint
// duplicate rather than double-counting (dedup edge case): the merge keeps the
// higher utility, unions provenance, bumps MergedCount, preserves the existing
// retrieval count, and clears any prior soft-expiry (a recurring, re-proven
// experience becomes live again).
func (s *Store) admitMerged(ctx context.Context, exp Experience) error {
	existing, ok, err := s.backend.GetAdmitted(ctx, exp.TenantID, exp.Fingerprint, true)
	if err != nil {
		return fmt.Errorf("experience: reading existing admitted %s: %w", exp.Fingerprint, err)
	}
	if ok {
		exp.MergedCount = existing.MergedCount + 1
		if existing.Utility > exp.Utility {
			exp.Utility = existing.Utility
		}
		exp.RetrievalCount = existing.RetrievalCount
		exp.CreatedAt = existing.CreatedAt
		exp.SourceIDs = unionStrings(existing.SourceIDs, exp.SourceIDs)
		exp.ExpiredAt = nil // re-proven: revive if it had been soft-expired
	}
	if err := s.backend.PutAdmitted(ctx, exp); err != nil {
		return fmt.Errorf("experience: admitting %s: %w", exp.Fingerprint, err)
	}
	return nil
}

// stamp fills the fingerprint and bi-temporal timestamps and defaults an empty
// scope to private (an experience with no scope is its owner's private memory,
// matching the store's normalizeScope).
func (s *Store) stamp(exp Experience) Experience {
	now := s.now().UTC()
	if exp.Fingerprint == "" {
		exp.Fingerprint = Fingerprint(exp.TenantID, exp.Task)
	}
	if exp.Scope == "" {
		exp.Scope = "private"
	}
	if exp.ValidAt.IsZero() {
		exp.ValidAt = now
	}
	if exp.CreatedAt.IsZero() {
		exp.CreatedAt = now
	}
	return exp
}

// SearchAdmitted returns up to k live admitted experiences matching query.
func (s *Store) SearchAdmitted(ctx context.Context, tenantID, query string, k int) ([]Experience, error) {
	if k <= 0 {
		k = 10
	}
	return s.backend.SearchAdmitted(ctx, tenantID, query, k)
}

// ListQuarantine returns every quarantined experience for tenant — the
// `engram quarantine list` surface.
func (s *Store) ListQuarantine(ctx context.Context, tenantID string) ([]Quarantined, error) {
	return s.backend.ListQuarantine(ctx, tenantID)
}

// Release moves a quarantined experience into the admitted index — the explicit
// human action `engram quarantine release` performs (never automatic). A
// released record is admitted directly (a human has reviewed it), then removed
// from quarantine.
func (s *Store) Release(ctx context.Context, tenantID, fingerprint string) error {
	q, ok, err := s.backend.GetQuarantine(ctx, tenantID, fingerprint)
	if err != nil {
		return fmt.Errorf("experience: reading quarantine %s: %w", fingerprint, err)
	}
	if !ok {
		return ErrNotFound
	}
	if err := s.admitMerged(ctx, s.stamp(q.Experience)); err != nil {
		return err
	}
	if err := s.backend.DeleteQuarantine(ctx, tenantID, fingerprint); err != nil {
		return fmt.Errorf("experience: clearing quarantine %s after release: %w", fingerprint, err)
	}
	s.logger.InfoContext(ctx, "experience released from quarantine", "fingerprint", fingerprint)
	return nil
}

// Recover returns an admitted experience INCLUDING one that prune has
// soft-expired — the bi-temporal recovery path proving prune never deletes.
func (s *Store) Recover(ctx context.Context, tenantID, fingerprint string) (Experience, error) {
	exp, ok, err := s.backend.GetAdmitted(ctx, tenantID, fingerprint, true)
	if err != nil {
		return Experience{}, err
	}
	if !ok {
		return Experience{}, ErrNotFound
	}
	return exp, nil
}

// Prune soft-expires every live admitted experience with Utility <= phiMax AND
// RetrievalCount <= retrievalMax (D3: soft-expire, never delete). It returns the
// number soft-expired. Recover still returns each pruned record; SearchAdmitted
// and the tier exclude them.
func (s *Store) Prune(ctx context.Context, phiMax float64, retrievalMax int) (int, error) {
	cands, err := s.backend.ScanPrunable(ctx, phiMax, retrievalMax)
	if err != nil {
		return 0, fmt.Errorf("experience: scanning prunable: %w", err)
	}
	now := s.now().UTC()
	pruned := 0
	for _, c := range cands {
		if err := s.backend.SoftExpire(ctx, c.TenantID, c.Fingerprint, now); err != nil {
			return pruned, fmt.Errorf("experience: soft-expiring %s: %w", c.Fingerprint, err)
		}
		pruned++
	}
	if pruned > 0 {
		s.logger.InfoContext(ctx, "experience prune soft-expired low-utility records", "count", pruned)
	}
	return pruned, nil
}

// unionStrings returns the de-duplicated union of two string slices, order
// preserved (a first).
func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, xs := range [][]string{a, b} {
		for _, x := range xs {
			if x != "" && !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	return out
}

// MemBackend is the in-memory Backend for unit tests and the dev/e2e stack. It
// is safe for concurrent use. It intentionally mirrors the OpenSearch backend's
// observable contract (live-only search/scan, include-expired recovery) so tests
// against it validate the same behaviour the cluster enforces.
type MemBackend struct {
	mu         sync.Mutex
	admitted   map[string]Experience  // key: tenant\x1ffingerprint
	quarantine map[string]Quarantined // key: tenant\x1ffingerprint
}

var _ Backend = (*MemBackend)(nil)

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{admitted: map[string]Experience{}, quarantine: map[string]Quarantined{}}
}

func memKey(tenantID, fingerprint string) string { return tenantID + "\x1f" + fingerprint }

// PutAdmitted implements Backend over the in-memory admitted map.
func (m *MemBackend) PutAdmitted(_ context.Context, exp Experience) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.admitted[memKey(exp.TenantID, exp.Fingerprint)] = exp
	return nil
}

// GetAdmitted implements Backend, honoring the includeExpired flag.
func (m *MemBackend) GetAdmitted(_ context.Context, tenantID, fingerprint string, includeExpired bool) (Experience, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.admitted[memKey(tenantID, fingerprint)]
	if !ok {
		return Experience{}, false, nil
	}
	if !includeExpired && !exp.Live() {
		return Experience{}, false, nil
	}
	return exp, true, nil
}

// SearchAdmitted implements Backend: live, tenant-scoped, best-first, top-k.
func (m *MemBackend) SearchAdmitted(_ context.Context, tenantID, query string, k int) ([]Experience, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := strings.ToLower(strings.TrimSpace(query))
	var out []Experience
	for _, exp := range m.admitted {
		if exp.TenantID != tenantID || !exp.Live() {
			continue
		}
		if q == "" || matchesQuery(exp, q) {
			out = append(out, exp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Utility != out[j].Utility {
			return out[i].Utility > out[j].Utility
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

func matchesQuery(exp Experience, q string) bool {
	hay := strings.ToLower(exp.Task + " " + exp.DistilledSkill + " " + exp.Context)
	for _, term := range strings.Fields(q) {
		if strings.Contains(hay, term) {
			return true
		}
	}
	return false
}

// ScanPrunable implements Backend: live docs at/below both prune ceilings.
func (m *MemBackend) ScanPrunable(_ context.Context, phiMax float64, retrievalMax int) ([]Experience, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Experience
	for _, exp := range m.admitted {
		if exp.Live() && exp.Utility <= phiMax && exp.RetrievalCount <= retrievalMax {
			out = append(out, exp)
		}
	}
	return out, nil
}

// SoftExpire implements Backend: stamps expired_at once, never deletes.
func (m *MemBackend) SoftExpire(_ context.Context, tenantID, fingerprint string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memKey(tenantID, fingerprint)
	exp, ok := m.admitted[key]
	if !ok {
		return ErrNotFound
	}
	if exp.ExpiredAt == nil { // soft-expire once; never delete
		t := at.UTC()
		exp.ExpiredAt = &t
		m.admitted[key] = exp
	}
	return nil
}

// PutQuarantine implements Backend over the in-memory quarantine map.
func (m *MemBackend) PutQuarantine(_ context.Context, q Quarantined) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quarantine[memKey(q.Experience.TenantID, q.Experience.Fingerprint)] = q
	return nil
}

// ListQuarantine implements Backend, tenant-scoped and fingerprint-sorted.
func (m *MemBackend) ListQuarantine(_ context.Context, tenantID string) ([]Quarantined, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Quarantined
	for _, q := range m.quarantine {
		if q.Experience.TenantID == tenantID {
			out = append(out, q)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Experience.Fingerprint < out[j].Experience.Fingerprint
	})
	return out, nil
}

// GetQuarantine implements Backend by (tenant, fingerprint) key.
func (m *MemBackend) GetQuarantine(_ context.Context, tenantID, fingerprint string) (Quarantined, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.quarantine[memKey(tenantID, fingerprint)]
	return q, ok, nil
}

// DeleteQuarantine implements Backend; a missing key is a no-op.
func (m *MemBackend) DeleteQuarantine(_ context.Context, tenantID, fingerprint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.quarantine, memKey(tenantID, fingerprint))
	return nil
}

// BumpRetrieval increments an admitted experience's retrieval count (used by
// the tier to feed the prune/following signals). A missing doc is a no-op. It
// satisfies the tier's optional retrievalBumper capability.
func (m *MemBackend) BumpRetrieval(_ context.Context, tenantID, fingerprint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memKey(tenantID, fingerprint)
	if exp, ok := m.admitted[key]; ok {
		exp.RetrievalCount++
		m.admitted[key] = exp
	}
	return nil
}
