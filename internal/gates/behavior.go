package gates

import "github.com/moabualruz/agent-dry/internal/manifest"

// ToolTestPasses runs the manifest's behavior test under the same limits and environment as G5.
func ToolTestPasses(t *manifest.Tool) bool {
	r := runToolTest(t)
	return r.startErr == nil && !r.timedOut && r.code == 0
}

func runToolTest(t *manifest.Tool) proc {
	return runProc(t.Dir, testTimeout, true, "bash", "-c", t.Test)
}
