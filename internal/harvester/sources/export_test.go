package sources

// Export internal variables/functions for external tests.
var (
	ExportedSetMaxResponseBytes = func(val int64) func() {
		orig := maxResponseBytes
		maxResponseBytes = val
		return func() { maxResponseBytes = orig }
	}
	ExportedSetMaxTokenLen = func(val int) func() {
		orig := maxTokenLen
		maxTokenLen = val
		return func() { maxTokenLen = orig }
	}
	ExportedSetAllowLocalGitTransport = func(val bool) func() {
		orig := allowLocalGitTransport
		allowLocalGitTransport = val
		return func() { allowLocalGitTransport = orig }
	}
	ExportedSetAllowLoopbackCrawl = func(val bool) func() {
		orig := allowLoopbackCrawl
		allowLoopbackCrawl = val
		return func() { allowLoopbackCrawl = orig }
	}
)
