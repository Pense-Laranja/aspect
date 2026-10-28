package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Includes let a spec be split across files. Anywhere a list is expected
// (modules, tiers, entities, interfaces, surfaces, goals), an entry of the
// form `- file: path.yaml` is replaced by that file's content: a list is
// spliced in, a mapping becomes one entry. `- dir: path/` includes every
// *.yaml and *.yml file in the directory, sorted by name. A mapping in any
// position may also be `{file: path.yaml}`. Paths are relative to the
// including file; brief and dependency paths inside included files are
// rebased so they stay relative to the root spec.
//
// Expansion happens on the YAML node tree before decoding, so unknown-key
// checking still applies to the assembled document.

// pathKeys are keys whose values are paths relative to the file they are
// written in; they are rebased when a fragment is included.
var pathKeys = map[string]bool{"brief": true, "spec": true}

// expander carries the state of one expansion. rootDir is the root spec's
// directory (brief paths are rebased against it); repoRoot is the
// repository it belongs to (RepositoryRoot), which no include may leave;
// visited guards against include cycles; files records the file every node
// was parsed from.
type expander struct {
	rootDir  string
	repoRoot string
	visited  map[string]bool
	files    map[*yaml.Node]string
}

// expandIncludes rewrites node in place. dir is the directory of the file
// node came from.
func (e *expander) expandIncludes(node *yaml.Node, dir string) error {
	switch node.Kind {
	case yaml.DocumentNode:
		for _, c := range node.Content {
			if err := e.expandIncludes(c, dir); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			val := node.Content[i+1]
			if kind, path := includeRef(val); kind == "file" {
				loaded, err := e.loadFragment(path, dir)
				if err != nil {
					return err
				}
				if loaded.Kind != yaml.MappingNode {
					return fmt.Errorf("%s: included in mapping position must contain a mapping", path)
				}
				node.Content[i+1] = loaded
				continue
			} else if kind == "dir" {
				return fmt.Errorf("%s: `dir` includes are only valid inside lists", path)
			}
			if err := e.expandIncludes(val, dir); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		var out []*yaml.Node
		for _, item := range node.Content {
			kind, path := includeRef(item)
			switch kind {
			case "":
				if err := e.expandIncludes(item, dir); err != nil {
					return err
				}
				out = append(out, item)
			case "file":
				loaded, err := e.loadFragment(path, dir)
				if err != nil {
					return err
				}
				out = append(out, splice(loaded)...)
			case "dir":
				if err := inRepository(e.repoRoot, filepath.Join(dir, path)); err != nil {
					return fmt.Errorf("include dir %s: %w", path, err)
				}
				files, err := fragmentFiles(filepath.Join(dir, path))
				if err != nil {
					return err
				}
				for _, f := range files {
					// fragmentFiles returns paths already joined with dir.
					loaded, err := e.loadFragment(f, "")
					if err != nil {
						return err
					}
					out = append(out, splice(loaded)...)
				}
			}
		}
		node.Content = out
	}
	return nil
}

// includeRef reports whether a node is an include directive: a mapping with
// exactly one key, `file` or `dir`, and a scalar value.
func includeRef(n *yaml.Node) (kind, path string) {
	if n.Kind != yaml.MappingNode || len(n.Content) != 2 {
		return "", ""
	}
	k, v := n.Content[0], n.Content[1]
	if k.Kind != yaml.ScalarNode || v.Kind != yaml.ScalarNode {
		return "", ""
	}
	if k.Value == "file" || k.Value == "dir" {
		return k.Value, v.Value
	}
	return "", ""
}

func splice(n *yaml.Node) []*yaml.Node {
	if n.Kind == yaml.SequenceNode {
		return n.Content
	}
	return []*yaml.Node{n}
}

func fragmentFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("include dir: %w", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			out = append(out, filepath.Join(dir, name))
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("include dir %s: no .yaml files", dir)
	}
	return out, nil
}

// loadFragment parses an included file, expands its own includes relative
// to its directory, and rebases its path-valued keys to the root spec.
func (e *expander) loadFragment(path, dir string) (*yaml.Node, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := inRepository(e.repoRoot, abs); err != nil {
		return nil, fmt.Errorf("include %s: %w", path, err)
	}
	if e.visited[abs] {
		return nil, fmt.Errorf("include cycle at %s", path)
	}
	e.visited[abs] = true
	defer delete(e.visited, abs)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("include: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("include %s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("include %s: empty file", path)
	}
	fragDir := filepath.Dir(path)
	if err := e.expandIncludes(&doc, fragDir); err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(e.rootDir, fragDir)
	if err != nil {
		rel = fragDir
	}
	e.rebasePaths(doc.Content[0], rel)
	e.record(&doc, path)
	return doc.Content[0], nil
}

// rebasePaths prefixes path-valued keys in a fragment with the fragment's
// directory relative to the root spec. Nodes already recorded by a nested
// include were rebased against rootDir when that inner fragment loaded, so
// the walk stops at their roots.
func (e *expander) rebasePaths(n *yaml.Node, rel string) {
	if _, done := e.files[n]; done {
		return
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind == yaml.ScalarNode && pathKeys[k.Value] && v.Kind == yaml.ScalarNode && v.Value != "" && !filepath.IsAbs(v.Value) {
				v.Value = filepath.ToSlash(filepath.Join(rel, v.Value))
				continue
			}
			e.rebasePaths(v, rel)
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			e.rebasePaths(c, rel)
		}
	}
}

// record marks every node under n that no nested include claimed as
// coming from file. Nested fragments are recorded first, so the walk stops
// at their roots.
func (e *expander) record(n *yaml.Node, file string) {
	if _, done := e.files[n]; done {
		return
	}
	e.files[n] = file
	for _, c := range n.Content {
		e.record(c, file)
	}
}

// Sources maps the nodes of an expanded spec back to the files they were
// written in, so an issue path like `modules[3].scenarios[1]` can be shown
// as `modules/orders.yaml:12`.
type Sources struct {
	root  *yaml.Node
	files map[*yaml.Node]string
}

// expandFile reads a spec file and resolves every include. It returns the
// document node (nil for an empty file) and where each node came from.
func expandFile(path string) (*yaml.Node, *Sources, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse spec: %w", err)
	}
	dir := filepath.Dir(path)
	abs, _ := filepath.Abs(path)
	repoRoot, err := RepositoryRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	e := &expander{rootDir: dir, repoRoot: repoRoot, visited: map[string]bool{abs: true}, files: map[*yaml.Node]string{}}
	if err := e.expandIncludes(&doc, dir); err != nil {
		return nil, nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil, nil
	}
	e.record(&doc, path)
	return doc.Content[0], &Sources{root: doc.Content[0], files: e.files}, nil
}

// Expand reads a spec file and returns its YAML with every include
// resolved, as `Load` sees it. Useful for debugging a split spec.
func Expand(path string) ([]byte, error) {
	b, _, err := expand(path)
	return b, err
}

func expand(path string) ([]byte, *Sources, error) {
	root, src, err := expandFile(path)
	if err != nil || root == nil {
		return []byte{}, nil, err
	}
	b, err := yaml.Marshal(root)
	return b, src, err
}
