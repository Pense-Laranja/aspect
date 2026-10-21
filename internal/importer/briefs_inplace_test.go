package importer

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/eideroliveira/aspect/internal/spec"
)

func TestInPlaceBriefs(t *testing.T) {
	repo := t.TempDir()
	copyOf := func(docs ...string) string {
		out := copyMarker + " from example.com/g. Edit freely; this is the module's brief. -->\n\n"
		for i := 0; i+1 < len(docs); i += 2 {
			if i > 0 {
				out += "\n\n---\n\n"
			}
			out += "<!-- " + docs[i] + " -->\n\n" + docs[i+1]
		}
		return out
	}
	files := map[string]string{
		".git/HEAD": "ref: refs/heads/main\n",
		"_aspect/aspect.yaml": `aspect: 1
system:
  name: g
  intent: shop
  module_path: example.com/g
  goals: [{id: G1, statement: x}]
modules:
  - {name: fresh, path: pkg/fresh, intent: a, goals: [G1], brief: briefs/fresh.md}
  - {name: stale, path: pkg/stale, intent: a, goals: [G1], brief: briefs/stale.md}
  - {name: none, path: pkg/none, intent: a, goals: [G1]}
  - {name: own, path: pkg/own, intent: a, goals: [G1], brief: briefs/own.md}
  - {name: done, path: pkg/done, intent: a, goals: [G1], brief: ../pkg/done/CLAUDE.md}
  - {name: nodoc, path: pkg/nodoc, intent: a, goals: [G1], brief: briefs/nodoc.md}
  - {name: unplaced, intent: a, goals: [G1], brief: briefs/unplaced.md}
`,
		"_aspect/briefs/fresh.md":    copyOf("CLAUDE.md", "fresh rules", "README.md", "usage"),
		"_aspect/briefs/stale.md":    copyOf("CLAUDE.md", "old rules"),
		"_aspect/briefs/own.md":      "Written by the owner.",
		"_aspect/briefs/nodoc.md":    copyOf("README.md", "usage"),
		"_aspect/briefs/unplaced.md": copyOf("CLAUDE.md", "x"),
		"pkg/fresh/CLAUDE.md":        "fresh rules\n",
		"pkg/stale/CLAUDE.md":        "new rules",
		"pkg/none/CLAUDE.md":         "rules",
		"pkg/own/CLAUDE.md":          "rules",
		"pkg/done/CLAUDE.md":         "rules",
	}
	for rel, content := range files {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := spec.Load(filepath.Join(repo, "_aspect", "aspect.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	moves, notes := InPlaceBriefs(s)
	got := map[string]BriefMove{}
	for _, mv := range moves {
		got[mv.Module] = mv
	}
	if len(got) != 3 {
		t.Fatalf("moves = %+v", moves)
	}
	if mv := got["fresh"]; mv.To != "../pkg/fresh/CLAUDE.md" || mv.Drifted || !slices.Equal(mv.Dropped, []string{"README.md"}) {
		t.Errorf("fresh = %+v", mv)
	}
	if mv := got["stale"]; !mv.Drifted || mv.From != "briefs/stale.md" {
		t.Errorf("stale = %+v", mv)
	}
	if mv := got["none"]; mv.From != "" || mv.To != "../pkg/none/CLAUDE.md" {
		t.Errorf("none = %+v", mv)
	}
	if len(notes) != 3 {
		t.Errorf("want notes for own, nodoc and unplaced, got %q", notes)
	}
}
