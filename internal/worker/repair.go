package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
)

// SweepStore adds the repair-scan reads to the worker's Store seam.
// OpenSearchStore satisfies it.
type SweepStore interface {
	Store
	// LiveSuperseders returns live facts whose supersedes link is set —
	// chains whose predecessor close may be pending.
	LiveSuperseders(ctx context.Context, limit int) ([]store.VersionedFact, error)
	// DuplicateLiveContentKeys returns content keys with >1 live fact.
	DuplicateLiveContentKeys(ctx context.Context, limit int) ([]string, error)
	// LiveByContentKey returns every live fact carrying key.
	LiveByContentKey(ctx context.Context, key string) ([]store.VersionedFact, error)
	// FindByEventID returns the episodic docs carrying eventID.
	FindByEventID(ctx context.Context, eventID string) ([]memory.Episodic, error)
	// ClosedOverlapChainKeys returns chains that may carry a closed-closed
	// valid-time overlap (rule d input).
	ClosedOverlapChainKeys(ctx context.Context, limit int) ([]store.ChainKey, error)
	// ChainVersions returns all non-expired records for a chain, ascending by
	// (valid_at, doc id) — rule d walks this to trim overlaps.
	ChainVersions(ctx context.Context, key store.ChainKey) ([]store.VersionedFact, error)
}

// SweepReport counts what one sweep converged.
type SweepReport struct {
	// ClosesCompleted counts predecessor closes finished on behalf of
	// crashed step-4 writers (rule a).
	ClosesCompleted int
	// SiblingsClosed counts divergent same-predecessor successors closed
	// down to a single live head (rule a').
	SiblingsClosed int
	// DuplicatesClosed counts duplicate live content-key copies closed
	// (rule b).
	DuplicatesClosed int
	// LedgersResumed counts incomplete past-lease ledger entries re-driven
	// through the worker (rule c).
	LedgersResumed int
	// OverlapsTrimmed counts closed records whose interval was trimmed down to
	// their nearest valid-time successor to remove a closed-closed overlap
	// (rule d — the irreducible write-skew residual from the Phase-2 review's
	// Issue 3).
	OverlapsTrimmed int
}

// Sweeper is the repair sweep (D10): a scheduled pass that converges every
// partial write the crash-prone protocol can leave behind. Rules:
//
//	(a)  a live fact's supersedes target is still live → complete the
//	     guarded close the writer never finished (DW-2.8).
//	(a') multiple live facts share one supersedes target — divergent
//	     concurrent UPDATEs — → chain-close them in (valid_at, id) order so
//	     exactly one live head survives (DW-2.5).
//	(b)  multiple live facts share one content key → keep the earliest,
//	     close the rest with an empty interval (never-valid record).
//	(c)  ledger entries incomplete past their lease → resume through the
//	     worker (cached extraction when present — the LLM is not re-called).
//	(d)  a chain's closed record whose valid-time interval extends past a
//	     later record's valid_at → trim the earlier record down to that
//	     successor's valid_at (monotone, guarded). This is the retroactive
//	     closure of the per-document-OCC write-skew window the Phase-2 review
//	     adjudicated irreducible at prevention time (Issue 3): two closed
//	     records for one (tenant, subject, predicate) must never cover the
//	     same instant.
//
// Scheduling is the caller's concern (Run); at S1 the convergence SLO is
// ≤5 min, so any interval well under that satisfies D10.
type Sweeper struct {
	Store  SweepStore
	Worker *Worker
	// Limit bounds each scan (default 500).
	Limit int
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Run sweeps every interval until ctx is done. A failed sweep is logged, not
// fatal — the next pass retries; convergence only needs eventual passes.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rep, err := s.Sweep(ctx)
			if err != nil {
				s.logger().ErrorContext(ctx, "repair sweep failed", "error", err)
				continue
			}
			if rep != (SweepReport{}) {
				s.logger().InfoContext(ctx, "repair sweep converged writes",
					"closes", rep.ClosesCompleted, "siblings", rep.SiblingsClosed,
					"duplicates", rep.DuplicatesClosed, "ledgers", rep.LedgersResumed)
			}
		}
	}
}

// Sweep runs one full repair pass and reports what it converged. Individual
// convergence failures (usually optimistic losses to live workers) are
// logged and left for the next pass; only scan-level failures return an
// error.
func (s *Sweeper) Sweep(ctx context.Context) (SweepReport, error) {
	var rep SweepReport
	if err := s.sweepSupersessions(ctx, &rep); err != nil {
		return rep, err
	}
	if err := s.sweepDuplicates(ctx, &rep); err != nil {
		return rep, err
	}
	if err := s.sweepLedgers(ctx, &rep); err != nil {
		return rep, err
	}
	if err := s.sweepClosedOverlaps(ctx, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// sweepClosedOverlaps applies rule (d): within each chain, a closed record
// whose interval extends past its nearest valid-time successor's valid_at is
// trimmed down to that successor, so no two records ever cover the same
// instant. The trim funnels through the same monotone, guarded trimInterval
// primitive the late-arrival path uses; a chain already disjoint is a no-op.
// Live records are never trimmed here — closing a live fact is the write
// protocol's and rules (a)/(a')'s job, never rule (d)'s.
func (s *Sweeper) sweepClosedOverlaps(ctx context.Context, rep *SweepReport) error {
	keys, err := s.Store.ClosedOverlapChainKeys(ctx, s.limit())
	if err != nil {
		return fmt.Errorf("worker: sweep scanning overlap chains: %w", err)
	}
	for _, key := range keys {
		versions, err := s.Store.ChainVersions(ctx, key)
		if err != nil {
			return fmt.Errorf("worker: sweep reading chain %+v: %w", key, err)
		}
		sortByValidThenID(versions)
		for i := range versions {
			rec := versions[i]
			if rec.Fact.InvalidAt == nil {
				continue // live head: not rule (d)'s to touch
			}
			// Nearest record strictly after rec by valid time is the true
			// interval boundary. Trimming rec's close down to it removes any
			// overlap; trimInterval no-ops when rec already ends at/before it.
			succ, ok := firstAfter(versions, i)
			if !ok || !rec.Fact.InvalidAt.After(succ.Fact.ValidAt) {
				continue
			}
			if err := s.Worker.trimInterval(ctx, rec, succ.Fact.ValidAt); err != nil {
				if !errors.Is(err, store.ErrConflict) {
					s.logger().WarnContext(ctx, "sweep overlap trim failed", "doc_id", rec.ID, "error", err)
				}
				continue
			}
			rep.OverlapsTrimmed++
		}
	}
	return nil
}

// firstAfter returns the first record after index i (in the (valid_at, id)
// sorted slice) whose valid_at is strictly greater than versions[i]'s — the
// nearest true valid-time successor. Records sharing the exact valid_at are
// skipped: they cannot bound each other's interval.
func firstAfter(versions []store.VersionedFact, i int) (store.VersionedFact, bool) {
	for j := i + 1; j < len(versions); j++ {
		if versions[j].Fact.ValidAt.After(versions[i].Fact.ValidAt) {
			return versions[j], true
		}
	}
	return store.VersionedFact{}, false
}

// sweepSupersessions applies rules (a) and (a').
func (s *Sweeper) sweepSupersessions(ctx context.Context, rep *SweepReport) error {
	live, err := s.Store.LiveSuperseders(ctx, s.limit())
	if err != nil {
		return fmt.Errorf("worker: sweep scanning superseders: %w", err)
	}

	// Rule (a): a supersedes target must not outlive its successors.
	byTarget := map[string][]store.VersionedFact{}
	for _, vf := range live {
		byTarget[vf.Fact.Supersedes] = append(byTarget[vf.Fact.Supersedes], vf)
	}
	for target, sibs := range byTarget {
		sortByValidThenID(sibs)
		pred, ok, err := s.Store.GetFact(ctx, target)
		if err != nil {
			return fmt.Errorf("worker: sweep reading supersedes target %s: %w", target, err)
		}
		if !ok || pred.Fact.InvalidAt != nil || pred.Fact.ExpiredAt != nil {
			continue
		}
		// Close at the earliest successor's valid_at — the moment the chain
		// says the predecessor stopped being true. Skip inverted candidates
		// defensively (they cannot close anything).
		if closeAt, found := earliestNonInverted(sibs, pred.Fact.ValidAt); found {
			if s.tryClose(ctx, pred, closeAt, "completing crashed close") {
				rep.ClosesCompleted++
			}
		}
	}

	// Rule (a'): divergent concurrent UPDATEs converge to one live head.
	// Racing writers can NEST divergence (C supersedes A supersedes P while
	// D supersedes B supersedes P), so siblinghood is shared chain ANCESTRY,
	// not a shared direct target: the version chain is the explicit link
	// (D11), and every live descendant of one root belongs to one chain —
	// which must end in exactly one live head, chained in valid-time order.
	roots := newRootResolver(s.Store)
	byRoot := map[string][]store.VersionedFact{}
	for _, vf := range live {
		root, err := roots.resolve(ctx, vf)
		if err != nil {
			return fmt.Errorf("worker: sweep resolving chain root of %s: %w", vf.ID, err)
		}
		byRoot[root] = append(byRoot[root], vf)
	}
	for _, sibs := range byRoot {
		if len(sibs) < 2 {
			continue
		}
		sortByValidThenID(sibs)
		for i := 0; i+1 < len(sibs); i++ {
			if s.tryClose(ctx, sibs[i], sibs[i+1].Fact.ValidAt, "converging divergent update") {
				rep.SiblingsClosed++
			}
		}
	}
	return nil
}

// maxChainDepth bounds supersedes-chain walks (defensive: a cycle would be a
// protocol-bug state, and the sweep must terminate regardless).
const maxChainDepth = 64

// rootResolver walks supersedes links to each fact's chain root, memoizing
// across one sweep pass.
type rootResolver struct {
	store SweepStore
	memo  map[string]string
}

func newRootResolver(st SweepStore) *rootResolver {
	return &rootResolver{store: st, memo: map[string]string{}}
}

// resolve returns the chain root doc id for vf: the last ancestor reachable
// through supersedes links. A dangling link (target never indexed) roots at
// the dangling id — siblings sharing it still group together.
func (r *rootResolver) resolve(ctx context.Context, vf store.VersionedFact) (string, error) {
	if root, ok := r.memo[vf.ID]; ok {
		return root, nil
	}
	visited := []string{vf.ID}
	cur, next := vf.ID, vf.Fact.Supersedes
	for depth := 0; next != "" && depth < maxChainDepth; depth++ {
		if root, ok := r.memo[next]; ok {
			cur = root
			next = ""
			break
		}
		parent, ok, err := r.store.GetFact(ctx, next)
		if err != nil {
			return "", err
		}
		cur = next
		visited = append(visited, next)
		if !ok {
			break // dangling link: root at the dangling id
		}
		next = parent.Fact.Supersedes
		if slices.Contains(visited, next) {
			break // cycle guard (protocol-bug state; terminate at cur)
		}
	}
	for _, id := range visited {
		r.memo[id] = cur
	}
	return cur, nil
}

// sweepDuplicates applies rule (b): keep the earliest live copy of a content
// key, close the rest with empty intervals ([v, v) never matches any query).
func (s *Sweeper) sweepDuplicates(ctx context.Context, rep *SweepReport) error {
	keys, err := s.Store.DuplicateLiveContentKeys(ctx, s.limit())
	if err != nil {
		return fmt.Errorf("worker: sweep scanning duplicate content keys: %w", err)
	}
	for _, key := range keys {
		dups, err := s.Store.LiveByContentKey(ctx, key)
		if err != nil {
			return fmt.Errorf("worker: sweep reading duplicates of %s: %w", key, err)
		}
		if len(dups) < 2 {
			continue // converged since the aggregation ran
		}
		sortByValidThenID(dups)
		for _, d := range dups[1:] {
			if s.tryClose(ctx, d, d.Fact.ValidAt, "closing duplicate content key") {
				rep.DuplicatesClosed++
			}
		}
	}
	return nil
}

// sweepLedgers applies rule (c): re-drive each incomplete past-lease ledger
// entry through the worker, which resumes from the cached extraction when
// one exists.
func (s *Sweeper) sweepLedgers(ctx context.Context, rep *SweepReport) error {
	entries, err := s.Store.ScanIncomplete(ctx)
	if err != nil {
		return fmt.Errorf("worker: sweep scanning incomplete ledgers: %w", err)
	}
	for _, e := range entries {
		if e.Key.ExtractorVersion != s.Worker.cfg.ExtractorVersion {
			continue // a different pipeline version owns this entry
		}
		events, err := s.Store.FindByEventID(ctx, e.Key.EventID)
		if err != nil {
			return fmt.Errorf("worker: sweep finding event %s: %w", e.Key.EventID, err)
		}
		resumed := false
		for _, ev := range events {
			if ev.TenantID != e.Key.TenantID {
				continue
			}
			if err := s.Worker.ProcessEvent(ctx, ev); err != nil {
				s.logger().WarnContext(ctx, "sweep resume failed; next pass retries", "event_id", ev.EventID, "error", err)
				continue
			}
			resumed = true
			break // one successful resume completes the ledger entry
		}
		if resumed {
			rep.LedgersResumed++
		}
	}
	return nil
}

// tryClose attempts one guarded close and reports whether it (or a
// concurrent winner) landed. Optimistic losses that re-read as still-open
// are left for the next pass rather than retried in a tight loop.
func (s *Sweeper) tryClose(ctx context.Context, vf store.VersionedFact, at time.Time, why string) bool {
	err := s.Worker.closePredecessor(ctx, vf, at)
	if err != nil {
		if !errors.Is(err, store.ErrConflict) {
			s.logger().WarnContext(ctx, "sweep close failed", "doc_id", vf.ID, "why", why, "error", err)
		}
		return false
	}
	return true
}

// earliestNonInverted returns the earliest sibling valid_at that is not
// before floor (a legal close instant for the predecessor).
func earliestNonInverted(sibs []store.VersionedFact, floor time.Time) (time.Time, bool) {
	for _, sib := range sibs { // sorted ascending by valid_at
		if !sib.Fact.ValidAt.Before(floor) {
			return sib.Fact.ValidAt, true
		}
	}
	return time.Time{}, false
}

func sortByValidThenID(v []store.VersionedFact) {
	sort.Slice(v, func(i, j int) bool {
		if !v[i].Fact.ValidAt.Equal(v[j].Fact.ValidAt) {
			return v[i].Fact.ValidAt.Before(v[j].Fact.ValidAt)
		}
		return v[i].ID < v[j].ID
	})
}

func (s *Sweeper) limit() int {
	if s.Limit > 0 {
		return s.Limit
	}
	return 500
}

func (s *Sweeper) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
