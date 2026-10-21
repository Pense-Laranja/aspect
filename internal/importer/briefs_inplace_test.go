package importer

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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
  - {name: done, path: pkg/done, intent: a, goals: [G1], brief: ../pkg/done/ASPECT.md}
  - {name: nodoc, path: pkg/nodoc, intent: a, goals: [G1], brief: briefs/nodoc.md}
  - {name: unplaced, intent: a, goals: [G1], brief: briefs/unplaced.md}
  - {name: empty, path: pkg/empty, intent: a, goals: [G1], brief: briefs/empty.md}
`,
		"_aspect/briefs/empty.md":    copyOf("ASPECT.md", "x"),
		"pkg/empty/ASPECT.md":        "  \n",
		"_aspect/briefs/fresh.md":    copyOf("ASPECT.md", "fresh rules", "README.md", "usage"),
		"_aspect/briefs/stale.md":    copyOf("ASPECT.md", "old rules"),
		"_aspect/briefs/own.md":      "Written by the owner.",
		"_aspect/briefs/nodoc.md":    copyOf("README.md", "usage"),
		"_aspect/briefs/unplaced.md": copyOf("ASPECT.md", "x"),
		"pkg/fresh/ASPECT.md":        "fresh rules\n",
		"pkg/stale/ASPECT.md":        "new rules",
		"pkg/none/ASPECT.md":         "rules",
		"pkg/own/ASPECT.md":          "rules",
		"pkg/done/ASPECT.md":         "rules",
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
	// An ASPECT.md the loader would refuse (a link out of the repository) is
	// never proposed: writing it would leave a spec that does not load.
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(repo, "pkg", "own", "sub")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if os.Symlink(outside, filepath.Join(linked, "ASPECT.md")) == nil {
		s.Modules = append(s.Modules, spec.Module{Name: "linked", Path: "pkg/own/sub", Intent: "a"})
	}
	moves, notes := InPlaceBriefs(s)
	if slices.ContainsFunc(moves, func(mv BriefMove) bool { return mv.Module == "linked" }) {
		t.Error("a package document the loader refuses is not proposed")
	}
	notes = slices.DeleteFunc(notes, func(n string) bool { return strings.HasPrefix(n, "linked: ") && strings.Contains(n, "symbolic link") })
	got := map[string]BriefMove{}
	for _, mv := range moves {
		got[mv.Module] = mv
	}
	if len(got) != 3 {
		t.Fatalf("moves = %+v", moves)
	}
	if mv := got["fresh"]; mv.To != "../pkg/fresh/ASPECT.md" || mv.Drifted || !slices.Equal(mv.Dropped, []string{"README.md"}) {
		t.Errorf("fresh = %+v", mv)
	}
	if mv := got["stale"]; !mv.Drifted || mv.From != "briefs/stale.md" {
		t.Errorf("stale = %+v", mv)
	}
	if mv := got["none"]; mv.From != "" || mv.To != "../pkg/none/ASPECT.md" {
		t.Errorf("none = %+v", mv)
	}
	if len(notes) != 4 {
		t.Errorf("want notes for own, nodoc, unplaced and empty, got %q", notes)
	}
	for _, want := range []string{"own: keeps its brief", "nodoc: pkg/nodoc/ASPECT.md does not exist", "unplaced: has no path:", "empty: pkg/empty/ASPECT.md is empty"} {
		if !slices.ContainsFunc(notes, func(n string) bool { return strings.HasPrefix(n, want) }) {
			t.Errorf("no note %q in %q", want, notes)
		}
	}
}

// Only a name line at the start of a copy or after briefs()' separator
// begins a document: a document's own comments, rules and separators-alike
// stay in it.
func TestCompareCopy(t *testing.T) {
	head := copyMarker + " from example.com/g. Edit freely; this is the module's brief. -->\n\n"
	doc := "# Pkg\n\nRules.\n\n<!-- prettier-ignore -->\n| a | b |\n\n---\n\n<!-- TODO -->\n\nMore.\n\n---"
	for name, tc := range map[string]struct {
		copied  string
		dropped []string
		drifted bool
	}{
		"same":          {head + "<!-- ASPECT.md -->\n\n" + doc, nil, false},
		"with others":   {head + "<!-- AGENTS.md -->\n\nAgents." + docSeparator + "<!-- ASPECT.md -->\n\n" + doc + docSeparator + "<!-- README.md -->\n\nUsage.", []string{"AGENTS.md", "README.md"}, false},
		"changed":       {head + "<!-- ASPECT.md -->\n\nOld rules.", nil, true},
		"shortened":     {head + "<!-- ASPECT.md -->\n\n" + doc + "\n\nA rule since removed.", nil, true},
		"written after": {head + "<!-- CLAUDE.md -->\n\nRules.", []string{"CLAUDE.md"}, false},
		"not a copy":    {"Written by hand.", nil, true},
	} {
		dropped, drifted := compareCopy(tc.copied, doc)
		if !slices.Equal(dropped, tc.dropped) || drifted != tc.drifted {
			t.Errorf("%s: dropped %q drifted %v, want %q %v", name, dropped, drifted, tc.dropped, tc.drifted)
		}
	}
}
