package importer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/eideroliveira/aspect/internal/spec"
)

// copyMarker opens every brief `aspect import` copies out of a package.
const copyMarker = "<!-- Recovered by aspect import"

// PackageBrief is the document a package keeps for people and coding
// assistants, which can be its module's brief in place.
const PackageBrief = "CLAUDE.md"

// BriefMove proposes pointing a module's brief at its package's own
// document instead of a copy beside the spec.
type BriefMove struct {
	Module string
	// From is the current brief path ("" when the module had none), To the
	// package document, both relative to the spec's directory as Brief.Path
	// is.
	From, To string
	// Dropped names the other documents the copy carried, which the package
	// document does not.
	Dropped []string
	// Drifted reports that the copy's text of the package document no longer
	// matches the document: the copy was going stale.
	Drifted bool
}

// docSeparator is what briefs() writes between two copied documents.
const docSeparator = "\n\n---\n\n"

// copiedDoc is the line briefs() writes before each copied document, at the
// start of the copy's body or right after a separator. The documents it
// copies are Markdown files, so a name ends in .md: a document's own
// "<!-- TODO -->" after a horizontal rule is not one.
var copiedDoc = regexp.MustCompile(`(?:^|\n\n---\n\n)<!-- (\S+\.(?i:md)) -->\n\n`)

// InPlaceBriefs proposes, for every module placed by path whose package
// directory holds a CLAUDE.md, to read that file as its brief: when the
// module has no brief, or its brief is a copy `aspect import` made. A brief
// someone wrote is left alone and so is a module already reading its
// package's document. notes explain each module left as it is that a reader
// might expect to move: one whose import copy cannot move (no path:, no
// CLAUDE.md in its package), or one keeping a brief someone wrote. A tier's
// code is under <root>/<spec.Tier.CodeDir>.
func InPlaceBriefs(s *spec.Spec) (moves []BriefMove, notes []string) {
	specDir, err := filepath.Abs(s.Dir)
	if err != nil {
		return nil, []string{err.Error()}
	}
	for _, t := range s.EffectiveTiers() {
		codeRoot := filepath.Join(s.Root, filepath.FromSlash(t.CodeDir()))
		for _, m := range t.ModuleRefs() {
			isCopy := strings.HasPrefix(strings.TrimSpace(m.Brief.Text), copyMarker)
			if !m.Placed() {
				if isCopy {
					notes = append(notes, m.Name+": has no path:, so its package is unknown (see aspect paths); its copied brief stays")
				}
				continue
			}
			doc := filepath.Join(codeRoot, filepath.FromSlash(m.Path), PackageBrief)
			text, err := os.ReadFile(doc)
			if err != nil {
				if isCopy {
					notes = append(notes, m.Name+": "+filepath.ToSlash(filepath.Join(m.Path, PackageBrief))+" does not exist; its copied brief stays")
				}
				continue
			}
			rel, err := filepath.Rel(specDir, doc)
			if err != nil {
				notes = append(notes, m.Name+": "+err.Error())
				continue
			}
			to := filepath.ToSlash(rel)
			if m.Brief.Path != "" && filepath.Clean(filepath.FromSlash(m.Brief.Path)) == filepath.Clean(rel) {
				continue
			}
			if m.Brief.Path != "" && !isCopy {
				notes = append(notes, m.Name+": keeps its brief "+m.Brief.Path+", which is not an import copy")
				continue
			}
			mv := BriefMove{Module: m.Name, From: m.Brief.Path, To: to}
			if isCopy {
				mv.Dropped, mv.Drifted = compareCopy(m.Brief.Text, string(text))
			}
			moves = append(moves, mv)
		}
	}
	return moves, notes
}

// compareCopy reads an import copy against the package document now: the
// names of the documents it holds besides CLAUDE.md, and whether its
// CLAUDE.md is no longer the document as it is (briefs() wrote each one
// trimmed, under its name line, the documents separated by docSeparator).
// The copy matches only when it holds the document exactly, in its own
// place, so a document's own comments and rules cannot fake a match.
func compareCopy(copied, doc string) (dropped []string, drifted bool) {
	_, body, ok := strings.Cut(copied, "-->\n\n")
	if !ok {
		return nil, true
	}
	for _, m := range copiedDoc.FindAllStringSubmatch(body, -1) {
		if m[1] != PackageBrief {
			dropped = append(dropped, m[1])
		}
	}
	want := "<!-- " + PackageBrief + " -->\n\n" + strings.TrimSpace(doc)
	for from := 0; ; {
		i := strings.Index(body[from:], want)
		if i < 0 {
			return dropped, true
		}
		at := from + i
		tail := body[at+len(want):]
		startsDoc := at == 0 || strings.HasSuffix(body[:at], docSeparator)
		next := copiedDoc.FindStringIndex(tail)
		endsDoc := strings.TrimRight(tail, "\n") == "" || (next != nil && next[0] == 0)
		if startsDoc && endsDoc {
			return dropped, false
		}
		from = at + 1
	}
}
