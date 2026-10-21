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

var copiedDoc = regexp.MustCompile(`(?m)^<!-- (\S+) -->$`)

// InPlaceBriefs proposes, for every module placed by path whose package
// directory holds a CLAUDE.md, to read that file as its brief: when the
// module has no brief, or its brief is a copy `aspect import` made. A brief
// someone wrote is left alone and so is a module already reading its
// package's document. notes explain the modules left as they are that had a
// copy. A single-tier spec's code is the repository root (Spec.Root), a
// tier's is <root>/<tier>.
func InPlaceBriefs(s *spec.Spec) (moves []BriefMove, notes []string) {
	multi := len(s.Tiers) > 0
	specDir, err := filepath.Abs(s.Dir)
	if err != nil {
		return nil, []string{err.Error()}
	}
	for _, t := range s.EffectiveTiers() {
		codeRoot := s.Root
		if multi {
			codeRoot = filepath.Join(s.Root, t.Name)
		}
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

// compareCopy reads an import copy (the marker, then each document under an
// <!-- NAME --> line, separated by ---) against the package document now.
func compareCopy(copied, doc string) (dropped []string, drifted bool) {
	locs := copiedDoc.FindAllStringSubmatchIndex(copied, -1)
	found := false
	for i, loc := range locs {
		name := copied[loc[2]:loc[3]]
		if name != PackageBrief {
			dropped = append(dropped, name)
			continue
		}
		end := len(copied)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := strings.TrimSuffix(strings.TrimSpace(copied[loc[1]:end]), "---")
		found = true
		drifted = strings.TrimSpace(body) != strings.TrimSpace(doc)
	}
	if !found {
		drifted = true
	}
	return dropped, drifted
}
