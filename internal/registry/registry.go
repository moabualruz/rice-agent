// Package registry builds the lookup index over every source and ranks queries against it.
// It registers the find, show, index and guide commands.
//
// Interface contract used by other packages (keep signatures stable):
//
//	Load(getenv, opts) (*Index, error)
//	(*Index).Search(query, limit) []Result
//	Threshold
package registry

// Result is one ranked lookup hit.
type Result struct {
	Name    string  `json:"name"`
	Summary string  `json:"summary"`
	Kind    string  `json:"kind"`   // script, cli, skill, recipe, wanted, seen
	Scope   string  `json:"scope"`  // global or project
	Entry   string  `json:"entry"`  // command or path to run or read
	Source  string  `json:"source"` // agent-dry, project:just, project:npm, skill, wanted, miss
	Score   float64 `json:"score"`
}

// Options select the sources Load reads.
type Options struct {
	ProjectRoot string // Git root to discover project commands from; empty means the working directory's root
	NoProject   bool   // skip project discovery (used by gates and tests)
}

// Threshold is the minimum score for a result to count as a match.
var Threshold = 1.0

// Index is the loaded, ranked lookup structure.
type Index struct {
	stub bool
}

// Load reads every source. Stub until ticket 03 lands.
func Load(getenv func(string) string, opts Options) (*Index, error) {
	return &Index{stub: true}, nil
}

// Search returns at most limit results at or above Threshold, best first.
func (ix *Index) Search(query string, limit int) []Result {
	return nil
}
