package worker_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/worker"
)

// fakeStore is an in-memory worker.SweepStore with the same conflict
// semantics as OpenSearch (op_type=create 409, guarded update 409, leases)
// plus crash-injection hooks: a hook registered for an op runs (under the
// lock) BEFORE the op applies, so returning an error simulates a worker
// killed at exactly that protocol step. The real query/mapping code paths
// are covered by the OpenSearchStore unit + integration tests; this fake
// exists to drive the worker's protocol logic deterministically.
type fakeStore struct {
	mu       sync.Mutex
	autoID   int
	episodic map[string]*memory.Episodic
	facts    map[string]*factRow
	ledger   map[string]*ledgerRow

	ledgerLease time.Duration
	hooks       map[string]func() error

	candidateCalls int
	lastCandidateK int
}

type factRow struct {
	fact        memory.SemanticFact
	seqNo       int64
	primaryTerm int64
}

type ledgerRow struct {
	key   memory.LedgerKey
	state store.LedgerState
	lease time.Time
}

var (
	_ worker.Store      = (*fakeStore)(nil)
	_ worker.SweepStore = (*fakeStore)(nil)
)

func newFakeStore() *fakeStore {
	return &fakeStore{
		episodic:    map[string]*memory.Episodic{},
		facts:       map[string]*factRow{},
		ledger:      map[string]*ledgerRow{},
		ledgerLease: time.Minute,
		hooks:       map[string]func() error{},
	}
}

// failOnce makes the next call of op fail with err — the crash-injection
// primitive.
func (f *fakeStore) failOnce(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fired := false
	f.hooks[op] = func() error {
		if fired {
			return nil
		}
		fired = true
		return err
	}
}

// hookOnce runs fn (under the lock, may mutate the fake directly) before the
// next call of op — used to inject a concurrent writer between a worker's
// read and its guarded write.
func (f *fakeStore) hookOnce(op string, fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fired := false
	f.hooks[op] = func() error {
		if !fired {
			fired = true
			fn()
		}
		return nil
	}
}

func (f *fakeStore) runHook(op string) error {
	if h := f.hooks[op]; h != nil {
		return h()
	}
	return nil
}

func (f *fakeStore) Append(_ context.Context, rec memory.Episodic) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.autoID++
	id := fmt.Sprintf("ep-%d", f.autoID)
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	f.episodic[id] = &rec
	return id, nil
}

func (f *fakeStore) Create(_ context.Context, id string, fact memory.SemanticFact) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.runHook("create"); err != nil {
		return err
	}
	if _, exists := f.facts[id]; exists {
		return fmt.Errorf("fake create %s: %w", id, store.ErrConflict)
	}
	f.facts[id] = &factRow{fact: fact, seqNo: 1, primaryTerm: 1}
	return nil
}

func (f *fakeStore) Update(_ context.Context, id string, fact memory.SemanticFact, ifSeqNo, ifPrimaryTerm int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.runHook("update"); err != nil {
		return err
	}
	row, exists := f.facts[id]
	if !exists || row.seqNo != ifSeqNo || row.primaryTerm != ifPrimaryTerm {
		return fmt.Errorf("fake update %s: %w", id, store.ErrConflict)
	}
	row.fact = fact
	row.seqNo++
	return nil
}

func (f *fakeStore) ClaimBatch(_ context.Context, n int, lease time.Duration) ([]memory.Episodic, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.runHook("claimbatch"); err != nil {
		return nil, err
	}
	now := time.Now()
	ids := make([]string, 0, len(f.episodic))
	for id, rec := range f.episodic {
		if rec.ProcessedAt != nil || rec.DeadLettered {
			continue
		}
		if rec.ClaimLeaseUntil != nil && rec.ClaimLeaseUntil.After(now) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := f.episodic[ids[i]], f.episodic[ids[j]]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return ids[i] < ids[j]
	})
	if len(ids) > n {
		ids = ids[:n]
	}
	out := make([]memory.Episodic, 0, len(ids))
	until := now.Add(lease)
	for _, id := range ids {
		rec := f.episodic[id]
		rec.Attempts++
		u := until
		rec.ClaimLeaseUntil = &u
		out = append(out, *rec)
	}
	return out, nil
}

func (f *fakeStore) Complete(_ context.Context, eventID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.runHook("complete"); err != nil {
		return err
	}
	now := time.Now().UTC()
	found := false
	for _, rec := range f.episodic {
		if rec.EventID == eventID {
			ts := now
			rec.ProcessedAt = &ts
			rec.ClaimLeaseUntil = nil
			found = true
		}
	}
	if !found {
		return fmt.Errorf("fake complete %s: %w", eventID, store.ErrNotFound)
	}
	return nil
}

func (f *fakeStore) DeadLetter(_ context.Context, eventID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for _, rec := range f.episodic {
		if rec.EventID == eventID {
			rec.DeadLettered = true
			rec.DeadLetterReason = reason
			rec.ClaimLeaseUntil = nil
			found = true
		}
	}
	if !found {
		return fmt.Errorf("fake dead-letter %s: %w", eventID, store.ErrNotFound)
	}
	return nil
}

func (f *fakeStore) ClaimLedger(_ context.Context, key memory.LedgerKey) (store.LedgerEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.runHook("claimledger"); err != nil {
		return store.LedgerEntry{}, err
	}
	id := key.DocID()
	if row, exists := f.ledger[id]; exists {
		return store.LedgerEntry{Key: row.key, State: cloneState(row.state), Claimed: false, LeaseUntil: row.lease}, nil
	}
	row := &ledgerRow{key: key, state: store.LedgerState{Phase: store.LedgerClaimed}, lease: time.Now().Add(f.ledgerLease)}
	f.ledger[id] = row
	return store.LedgerEntry{Key: key, State: cloneState(row.state), Claimed: true, LeaseUntil: row.lease}, nil
}

func (f *fakeStore) UpdateLedger(_ context.Context, key memory.LedgerKey, state store.LedgerState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.runHook("updateledger"); err != nil {
		return err
	}
	f.ledger[key.DocID()] = &ledgerRow{key: key, state: cloneState(state), lease: time.Now().Add(f.ledgerLease)}
	return nil
}

func (f *fakeStore) ScanIncomplete(context.Context) ([]store.LedgerEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	var out []store.LedgerEntry
	for _, row := range f.ledger {
		if row.state.Phase == store.LedgerComplete || row.lease.After(now) {
			continue
		}
		out = append(out, store.LedgerEntry{Key: row.key, State: cloneState(row.state), LeaseUntil: row.lease})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.EventID < out[j].Key.EventID })
	return out, nil
}

func (f *fakeStore) GetFact(_ context.Context, id string) (store.VersionedFact, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, exists := f.facts[id]
	if !exists {
		return store.VersionedFact{}, false, nil
	}
	return store.VersionedFact{ID: id, Fact: row.fact, SeqNo: row.seqNo, PrimaryTerm: row.primaryTerm}, true, nil
}

func (f *fakeStore) Candidates(_ context.Context, fact memory.SemanticFact, k int) ([]store.VersionedFact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candidateCalls++
	f.lastCandidateK = k
	var out []store.VersionedFact
	for _, id := range f.sortedFactIDs() {
		row := f.facts[id]
		fc := row.fact
		if fc.InvalidAt != nil || fc.ExpiredAt != nil || fc.TenantID != fact.TenantID {
			continue
		}
		if fc.ContentKey == fact.ContentKey ||
			(fc.Subject == fact.Subject && fc.Predicate == fact.Predicate) ||
			(fact.Statement != "" && strings.Contains(fc.Statement, fact.Subject)) {
			out = append(out, store.VersionedFact{ID: id, Fact: fc, SeqNo: row.seqNo, PrimaryTerm: row.primaryTerm})
		}
		if len(out) == k {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) ValidTimeNeighbors(_ context.Context, fact memory.SemanticFact, selfID string) (pred, succ *store.VersionedFact, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.sortedFactIDs() {
		row := f.facts[id]
		fc := row.fact
		if id == selfID || fc.ExpiredAt != nil ||
			fc.TenantID != fact.TenantID || fc.Subject != fact.Subject || fc.Predicate != fact.Predicate {
			continue
		}
		vf := store.VersionedFact{ID: id, Fact: fc, SeqNo: row.seqNo, PrimaryTerm: row.primaryTerm}
		if fc.ValidAt.After(fact.ValidAt) {
			if succ == nil || fc.ValidAt.Before(succ.Fact.ValidAt) || (fc.ValidAt.Equal(succ.Fact.ValidAt) && id < succ.ID) {
				v := vf
				succ = &v
			}
		} else if pred == nil || fc.ValidAt.After(pred.Fact.ValidAt) || (fc.ValidAt.Equal(pred.Fact.ValidAt) && id > pred.ID) {
			v := vf
			pred = &v
		}
	}
	return pred, succ, nil
}

func (f *fakeStore) LiveSuperseders(_ context.Context, limit int) ([]store.VersionedFact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.VersionedFact
	for _, id := range f.sortedFactIDs() {
		row := f.facts[id]
		if row.fact.InvalidAt == nil && row.fact.ExpiredAt == nil && row.fact.Supersedes != "" {
			out = append(out, store.VersionedFact{ID: id, Fact: row.fact, SeqNo: row.seqNo, PrimaryTerm: row.primaryTerm})
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) DuplicateLiveContentKeys(context.Context, int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	counts := map[string]int{}
	for _, row := range f.facts {
		if row.fact.InvalidAt == nil && row.fact.ExpiredAt == nil {
			counts[row.fact.ContentKey]++
		}
	}
	var keys []string
	for key, n := range counts {
		if n > 1 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (f *fakeStore) LiveByContentKey(_ context.Context, key string) ([]store.VersionedFact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.VersionedFact
	for _, id := range f.sortedFactIDs() {
		row := f.facts[id]
		if row.fact.InvalidAt == nil && row.fact.ExpiredAt == nil && row.fact.ContentKey == key {
			out = append(out, store.VersionedFact{ID: id, Fact: row.fact, SeqNo: row.seqNo, PrimaryTerm: row.primaryTerm})
		}
	}
	return out, nil
}

func (f *fakeStore) ClosedOverlapChainKeys(_ context.Context, limit int) ([]store.ChainKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	counts := map[store.ChainKey]int{}
	for _, row := range f.facts {
		if row.fact.ExpiredAt != nil {
			continue
		}
		counts[store.ChainKey{TenantID: row.fact.TenantID, Subject: row.fact.Subject, Predicate: row.fact.Predicate}]++
	}
	var keys []store.ChainKey
	for k, n := range counts {
		if n > 1 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Subject != keys[j].Subject {
			return keys[i].Subject < keys[j].Subject
		}
		return keys[i].Predicate < keys[j].Predicate
	})
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	return keys, nil
}

func (f *fakeStore) ChainVersions(_ context.Context, key store.ChainKey) ([]store.VersionedFact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.VersionedFact
	for _, id := range f.sortedFactIDs() {
		row := f.facts[id]
		if row.fact.ExpiredAt != nil ||
			row.fact.TenantID != key.TenantID || row.fact.Subject != key.Subject || row.fact.Predicate != key.Predicate {
			continue
		}
		out = append(out, store.VersionedFact{ID: id, Fact: row.fact, SeqNo: row.seqNo, PrimaryTerm: row.primaryTerm})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Fact.ValidAt.Equal(out[j].Fact.ValidAt) {
			return out[i].Fact.ValidAt.Before(out[j].Fact.ValidAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (f *fakeStore) FindByEventID(_ context.Context, eventID string) ([]memory.Episodic, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []memory.Episodic
	for _, rec := range f.episodic {
		if rec.EventID == eventID {
			out = append(out, *rec)
		}
	}
	return out, nil
}

// --- test-side inspection helpers (lock-guarded copies) ---

func (f *fakeStore) sortedFactIDs() []string {
	ids := make([]string, 0, len(f.facts))
	for id := range f.facts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// liveFacts returns every live fact keyed by doc id.
func (f *fakeStore) liveFacts() map[string]memory.SemanticFact {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]memory.SemanticFact{}
	for id, row := range f.facts {
		if row.fact.InvalidAt == nil && row.fact.ExpiredAt == nil {
			out[id] = row.fact
		}
	}
	return out
}

// allFacts returns every fact (live and closed) keyed by doc id.
func (f *fakeStore) allFacts() map[string]memory.SemanticFact {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]memory.SemanticFact{}
	for id, row := range f.facts {
		out[id] = row.fact
	}
	return out
}

// factSeqNo returns the current seq_no of a fact doc (concurrency probe).
func (f *fakeStore) factSeqNo(id string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.facts[id]; ok {
		return row.seqNo
	}
	return -1
}

// eventRow returns the first episodic row carrying eventID.
func (f *fakeStore) eventRow(eventID string) (memory.Episodic, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rec := range f.episodic {
		if rec.EventID == eventID {
			return *rec, true
		}
	}
	return memory.Episodic{}, false
}

// seedFact inserts a fully-formed fact row directly (bypassing the write
// protocol) so a test can construct a specific bi-temporal end state — used
// by the rule (d) regressions, which reproduce the write-skew overlap the
// Phase-2 review adjudicated unreachable through sequential protocol steps.
func (f *fakeStore) seedFact(id string, fact memory.SemanticFact) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.facts[id] = &factRow{fact: fact, seqNo: 1, primaryTerm: 1}
}

// closeFactDirectly simulates a concurrent worker closing a fact out from
// under the caller (bumps the seq_no so guarded writes lose).
func (f *fakeStore) closeFactDirectly(id string, at time.Time) {
	row := f.facts[id] // callers hold the lock via hookOnce
	inv := at
	now := time.Now().UTC()
	row.fact.InvalidAt = &inv
	row.fact.InvalidatedTxAt = &now
	row.seqNo++
}

func cloneState(s store.LedgerState) store.LedgerState {
	out := store.LedgerState{Phase: s.Phase}
	out.Extraction = append([]byte(nil), s.Extraction...)
	out.CompletedActions = append([]string(nil), s.CompletedActions...)
	return out
}
