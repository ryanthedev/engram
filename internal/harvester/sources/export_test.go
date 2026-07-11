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
)
