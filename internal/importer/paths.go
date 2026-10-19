package importer

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/eideroliveira/aspect/internal/analyze"
	"github.com/eideroliveira/aspect/internal/spec"
)

// RecoverPaths derives the path of every Go module that has none, for a
// spec imported before the assembler recorded package directories. A
// module's path is the package directory whose ModuleName is the module's
// name, found by the same inventory the import ran (honouring the spec's
// source.exclude). root holds a single-tier spec's code; a tier's code is
// root/<tier>.
//
// A module that no package maps to, or that several do (modules/api and
// modules_api are both modules_api), is not guessed: it is reported in
// problems and left out of paths.
func RecoverPaths(s *spec.Spec, root string) (paths map[string]string, problems []string, err error) {
	paths = map[string]string{}
	var exclude []string
	if s.System.Source != nil {
		exclude = s.System.Source.Exclude
	}
	for _, t := range s.EffectiveTiers() {
		var missing []*spec.Module
		for i := range t.Modules {
			if t.Modules[i].Path == "" {
				missing = append(missing, &t.Modules[i])
			}
		}
		if len(missing) == 0 {
			continue
		}
		if t.Language != "go" {
			return nil, nil, fmt.Errorf("paths: tier %q is %s; only Go modules are placed by path", t.Name, t.Language)
		}
		dir := filepath.Join(root, t.Name)
		inv, err := analyze.Go(dir, analyze.Options{Exclude: exclude})
		if err != nil {
			return nil, nil, fmt.Errorf("paths: %w", err)
		}
		byName := map[string][]string{}
		for _, p := range inv.Packages {
			name := ModuleName(p.Dir)
			byName[name] = append(byName[name], ModulePath(p.Dir))
		}
		for _, m := range missing {
			switch dirs := byName[m.Name]; len(dirs) {
			case 0:
				problems = append(problems, fmt.Sprintf("module %s: no package under %s maps to it", m.Name, dir))
			case 1:
				paths[m.Name] = dirs[0]
			default:
				sort.Strings(dirs)
				problems = append(problems, fmt.Sprintf("module %s: ambiguous, packages %s all map to it; set its path by hand", m.Name, strings.Join(dirs, ", ")))
			}
		}
	}
	return paths, problems, nil
}
