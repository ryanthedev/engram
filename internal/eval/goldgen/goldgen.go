// Package goldgen deterministically generates the seed gold set. The output
// is checked in at eval/goldset/seed.json; the split assignment (hash of the
// query id) is computed once here and FROZEN in the file — that checked-in
// assignment is the pre-registration DW-1.3 measures against. Regenerating
// yields byte-identical output.
package goldgen

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/ryanthedev/engram/internal/eval"
)

// fact is one seed knowledge item about a fictional engineering org; each
// yields a corpus doc and several gold queries in different query classes.
type fact struct {
	slug       string
	statement  string
	exactID    string // an exact token (ticket, service, code) BM25 must hit
	keyword    string // a keyword query
	paraphrase string // a semantic paraphrase kNN must hit
	multiTerm  string // a multi-term composite query
}

// seedFacts is the fixed knowledge base: 30 facts about the fictional
// "Meridian" engineering org — services, incidents, deploys, owners, configs.
var seedFacts = []fact{
	{"pay-timeout", "Service payments-api has a hard request timeout of 2500ms configured in PAY-cfg-114.", "PAY-cfg-114", "payments-api timeout", "how long before the payment service gives up on a request", "payments-api request timeout configuration value"},
	{"inc-4312", "Incident INC-4312 was caused by a leaked connection pool in orders-svc after the v2.14 deploy.", "INC-4312", "orders-svc incident", "what went wrong with the order service connection leak", "incident caused by connection pool leak orders"},
	{"own-search", "The search-platform team owns the opensearch-gateway service; escalation goes to @maya.chen.", "opensearch-gateway", "search-platform ownership", "who do I page when the search cluster proxy breaks", "opensearch gateway owning team escalation"},
	{"dep-freeze", "Deploy freezes start every quarter on the last Thursday and are tracked in REL-freeze-Q.", "REL-freeze-Q", "deploy freeze schedule", "when are we not allowed to ship to production", "quarterly deploy freeze start day tracking"},
	{"tok-rotate", "API tokens for vault-broker rotate every 12 hours; manual rotation uses runbook RB-77.", "RB-77", "vault-broker token rotation", "how often do the secret broker credentials refresh", "vault broker api token rotation interval runbook"},
	{"db-primary", "The billing Postgres primary is db-bill-01 in us-east-2a; failover target is db-bill-02.", "db-bill-01", "billing postgres primary", "which database node is the main one for invoicing", "billing postgres primary failover target zone"},
	{"rate-limit", "Public API rate limit is 600 requests per minute per key, enforced by edge-limiter.", "edge-limiter", "api rate limit", "how many calls can one api key make each minute", "public api per key rate limit enforcement"},
	{"inc-4388", "Incident INC-4388 traced to a bad glibc upgrade on the build fleet; fixed by pinning image bld-2024.09.", "INC-4388", "build fleet incident", "what broke the compile machines last autumn", "incident glibc upgrade build fleet pinned image"},
	{"kafka-lag", "Consumer lag alerts for the events pipeline fire at 30k messages via alert EVT-lag-30k.", "EVT-lag-30k", "kafka lag alert", "at what backlog does the event stream page us", "events pipeline consumer lag alert threshold"},
	{"gpu-quota", "Each ML team gets 64 A100-hours per day; overage requests go through ML-QUOTA board.", "ML-QUOTA", "gpu quota", "how much accelerator time does a team get daily", "ml team daily a100 hour quota overage"},
	{"cdn-purge", "CDN cache purges propagate globally within 90 seconds using the purge-fanout job.", "purge-fanout", "cdn purge", "how fast does clearing the edge cache take everywhere", "cdn cache purge global propagation time"},
	{"oncall-rot", "Primary on-call rotates weekly on Mondays 10:00 UTC; the schedule lives in PagerPlane.", "PagerPlane", "oncall rotation", "when does the pager hand over each week", "primary oncall weekly rotation handover time"},
	{"tf-state", "Terraform state for core-infra is stored in bucket tf-core-state-prod with DynamoDB locking.", "tf-core-state-prod", "terraform state", "where does infrastructure as code keep its state file", "terraform core infra state bucket locking"},
	{"sso-idp", "Workforce SSO runs through Okta tenant meridian-corp; break-glass accounts are in BG-vault.", "meridian-corp", "sso identity provider", "what do we log into when single sign on is down", "workforce sso okta tenant break glass"},
	{"canary-pct", "Canary deploys route 5% of traffic for 30 minutes before full rollout, gated by canary-judge.", "canary-judge", "canary percentage", "how much traffic does a trial release get first", "canary deploy traffic percent duration gate"},
	{"log-retent", "Production logs retain 30 days hot and 13 months cold in the logging-lake archive.", "logging-lake", "log retention", "how long do we keep production log data", "production log retention hot cold archive"},
	{"mtls-cert", "Internal mTLS certificates expire after 90 days and auto-renew at 60 via cert-minter.", "cert-minter", "mtls certificate expiry", "when do the internal service certs run out", "internal mtls certificate lifetime auto renew"},
	{"perf-slo", "checkout-web has a p95 latency SLO of 350ms measured at the edge, dashboard PERF-co-web.", "PERF-co-web", "checkout latency slo", "how fast must the purchase page respond", "checkout web p95 latency slo edge"},
	{"repo-mono", "All Go services live in the monorepo meridian/go-services; ownership is per-directory CODEOWNERS.", "go-services", "monorepo layout", "where does backend code live and who approves changes", "go services monorepo codeowners per directory"},
	{"sec-scan", "Container images are blocked from deploy on critical CVEs by the sentinel-scan admission gate.", "sentinel-scan", "security scanning", "what stops a vulnerable image from shipping", "container image critical cve admission gate"},
	{"flag-svc", "Feature flags are served by flagd-edge with a 10s propagation SLA; kill switches use KS- prefixes.", "flagd-edge", "feature flags", "how quickly does toggling a feature reach users", "feature flag propagation sla kill switch"},
	{"backup-rpo", "Billing data RPO is 5 minutes via WAL shipping; restore drills run monthly under DR-bill.", "DR-bill", "billing backup rpo", "how much invoice data could we lose at most", "billing rpo wal shipping restore drill"},
	{"inc-4501", "Incident INC-4501: expired SAML metadata broke vendor logins; renewal automated in job saml-refresh.", "INC-4501", "saml incident", "why could partners not sign in during the outage", "incident expired saml metadata vendor login"},
	{"queue-dlq", "Failed webhook deliveries retry 8 times with exponential backoff then land in hooks-dlq.", "hooks-dlq", "webhook retries", "what happens to a webhook that keeps failing", "webhook delivery retry count backoff dead letter"},
	{"cost-tag", "Every cloud resource must carry cost-center and service tags; untagged spend pages FinOps weekly.", "cost-center", "cost tagging", "which labels are mandatory on cloud resources", "cloud resource mandatory tags untagged spend"},
	{"api-vers", "Public API versions are date-pinned (e.g. 2025-06-01) and supported for 24 months after sunset notice.", "2025-06-01", "api versioning", "how long is an old api version kept alive", "public api date pinned version support window"},
	{"chaos-day", "Chaos experiments run in prod every second Wednesday, coordinated through the storm-runner tool.", "storm-runner", "chaos testing", "when do we deliberately break things in production", "chaos experiment prod schedule coordination tool"},
	{"data-resid", "EU customer data stays in eu-central-1 under policy DATA-RES-EU; cross-region copies are blocked.", "DATA-RES-EU", "data residency", "where must european customer records be stored", "eu data residency region policy cross region"},
	{"build-cache", "CI build cache hits average 82% using the bazel-remote cluster; misses page nobody.", "bazel-remote", "build cache", "how often does continuous integration reuse compiled work", "ci build cache hit rate remote cluster"},
	{"pii-mask", "PII is masked at ingestion by the scrub-gate library; raw values never reach the lake.", "scrub-gate", "pii masking", "how is personal data hidden before analytics", "pii masking ingestion library data lake"},
}

// Generate builds the seed gold set: one corpus doc per fact and four gold
// queries per fact (exact-id, keyword, paraphrase, multi-term). Split is
// hash(query id) mod 2 — deterministic, roughly balanced, frozen at
// generation time.
func Generate() eval.GoldSet {
	var gs eval.GoldSet
	for _, f := range seedFacts {
		docID := "doc-" + f.slug
		gs.Corpus = append(gs.Corpus, eval.Doc{ID: docID, Text: f.statement})
		for class, text := range map[string]string{
			"exact-id":   f.exactID,
			"keyword":    f.keyword,
			"paraphrase": f.paraphrase,
			"multi-term": f.multiTerm,
		} {
			qid := fmt.Sprintf("q-%s-%s", f.slug, class)
			gs.Queries = append(gs.Queries, eval.Query{
				ID:          qid,
				Text:        text,
				ExpectedIDs: []string{docID},
				Split:       splitFor(qid),
				Class:       class,
			})
		}
	}
	// Map iteration order is random; fix a stable order for byte-identical
	// output.
	sort.Slice(gs.Queries, func(i, j int) bool { return gs.Queries[i].ID < gs.Queries[j].ID })
	return gs
}

// splitFor pre-registers the split: a salted hash of the query id. The salt
// was chosen (before any retrieval measurement existed) so the coin flips
// land near 50/50 with all four query classes represented in both splits
// (train=59 / holdout=61); it is now FROZEN — changing it after Phase-1
// measurements start would un-register the holdout.
func splitFor(qid string) string {
	h := sha256.Sum256([]byte("engram-split-v12:" + qid))
	if binary.BigEndian.Uint64(h[:8])%2 == 0 {
		return eval.SplitTrain
	}
	return eval.SplitHoldout
}
