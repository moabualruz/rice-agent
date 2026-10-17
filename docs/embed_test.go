package docs

import "testing"

func TestTopics(t *testing.T) {
	want := map[string]bool{"forge": true, "optimizer": true, "reuse": true, "scout": true, "surgeon": true, "tools": true}
	got := map[string]bool{}
	for _, n := range Topics() {
		got[n] = true
		if _, ok := Topic(n); !ok {
			t.Errorf("Topic(%q) not found", n)
		}
	}
	for n := range want {
		if !got[n] {
			t.Errorf("missing topic %q in %v", n, Topics())
		}
	}
	if _, ok := Topic("../go.mod"); ok {
		t.Error("path traversal accepted")
	}
	if _, ok := Topic("guide"); !ok {
		t.Error("guide topic not readable")
	}
}
