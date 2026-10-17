package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRepositoryTools validates every manifest under tools/ so a tool author can run
// `go test ./internal/manifest/` before the check command exists.
func TestRepositoryTools(t *testing.T) {
	root := filepath.Join("..", "..", "tools")
	tools, err := LoadAll(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		info, err := os.Stat(tool.EntryPath())
		if err != nil {
			t.Errorf("%s: entry %s: %v", tool.Name, tool.Entry, err)
			continue
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("%s: entry %s is not executable", tool.Name, tool.Entry)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"missing fields": `{"schema":"tool/v1","name":"x"}`,
		"bad summary":    `{"schema":"tool/v1","name":"ab","summary":"does a thing nicely","intents":["a b c","d e f","g h i"],"kind":"script","scope":"global","entry":"ab","language":"bash","side_effects":"read","exit_codes":{"0":"ok","2":"usage"},"platforms":["linux-amd64"],"test":"bash test.sh","status":"trial","origin":{"date":"2026-10-17","builder":"model","task":"task-1"}}`,
		"write without dry_run": `{"schema":"tool/v1","name":"ab","summary":"Use when testing a thing","intents":["a b c","d e f","g h i"],"kind":"script","scope":"global","entry":"ab","language":"bash","side_effects":"remote-write","exit_codes":{"0":"ok","2":"usage"},"platforms":["linux-amd64"],"test":"bash test.sh","status":"trial","origin":{"date":"2026-10-17","builder":"model","task":"task-1"}}`,
	}
	for name, raw := range cases {
		if err := Validate([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := `{"schema":"tool/v1","name":"ab","summary":"Use when testing a thing","intents":["a b c","d e f","g h i"],"kind":"script","scope":"global","entry":"ab","language":"bash","side_effects":"read","exit_codes":{"0":"ok","2":"usage"},"platforms":["linux-amd64"],"test":"bash test.sh","status":"trial","origin":{"date":"2026-10-17","builder":"model","task":"task-1"}}`
	if err := Validate([]byte(ok)); err != nil {
		t.Errorf("valid manifest rejected: %v", err)
	}
}
