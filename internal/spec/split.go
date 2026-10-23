package spec

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Layout is a spec split across files: Files maps slash paths relative to
// the spec's directory to their content, Root names the entry point.
type Layout struct {
	Root  string
	Files map[string][]byte
}

// Paths returns the layout's files with the entry point first and the
// fragments sorted.
func (l *Layout) Paths() []string {
	var out []string
	for p := range l.Files {
		if p != l.Root {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return append([]string{l.Root}, out...)
}

// Dirs returns the directories the fragments live in, sorted.
func (l *Layout) Dirs() []string {
	seen := map[string]bool{}
	for p := range l.Files {
		if d := path.Dir(p); p != l.Root && !seen[d] {
			seen[d] = true
		}
	}
	return sortedKeys(seen)
}

// Split breaks a single-file spec into one file per module and per
// interface, and one file of entities per owning module, so a change to
// one package touches one small file:
//
//	aspect.yaml              system, goals, stack, constraints, the rest
//	interfaces/<name>.yaml   one interface with its surfaces
//	entities/<owner>.yaml    the entities a module owns (_unowned.yaml for none)
//	modules/<name>.yaml      one module
//	tiers/<tier>/...         the same per tier, for multi-tier specs
//
// The entry point pulls each directory in with a `dir:` include. Brief
// paths are rewritten relative to the fragment that holds them; comments
// and key order are kept. data must not use includes already (expand it
// first). name is the entry point's file name.
//
// Split checks its own result: the layout must load to the same spec as
// data, up to the order of modules, interfaces and entities, which `dir:`
// includes sort by file name and nothing downstream depends on.
func Split(data []byte, name string) (*Layout, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("spec is empty or not a mapping")
	}
	if p := findInclude(&doc); p != "" {
		return nil, fmt.Errorf("spec already uses includes (%s); split works on a single file, so expand it first", p)
	}
	root := doc.Content[0]
	sp := &splitter{fragments: map[string]*yaml.Node{}, owner: map[string]string{}}

	// Entity owners, from every module in declaration order; the first
	// owner wins, as an invalid spec may name several.
	_, tiers := mapEntry(root, "tiers")
	for _, mods := range append([]*yaml.Node{valueOf(root, "modules")}, tierLists(tiers, "modules")...) {
		for _, m := range items(mods) {
			for _, e := range items(valueOf(m, "entities")) {
				if _, ok := sp.owner[e.Value]; !ok && e.Kind == yaml.ScalarNode {
					sp.owner[e.Value] = valueOf(m, "name").Value
				}
			}
		}
	}

	system := valueOf(root, "system")
	if err := sp.each(valueOf(system, "interfaces"), "interfaces", "system.interfaces"); err != nil {
		return nil, err
	}
	if err := sp.entities(valueOf(system, "database"), "entities", "system.database"); err != nil {
		return nil, err
	}
	if err := sp.each(valueOf(root, "modules"), "modules", "modules"); err != nil {
		return nil, err
	}
	for i, t := range items(tiers) {
		tn := valueOf(t, "name")
		if tn == nil || tn.Value == "" {
			return nil, fmt.Errorf("tiers[%d] has no name", i)
		}
		file, err := fileName(tn.Value)
		if err != nil {
			return nil, fmt.Errorf("tiers[%d]: %w", i, err)
		}
		base := "tiers/" + file
		if err := sp.entities(valueOf(t, "database"), base+"/entities", fmt.Sprintf("tiers[%d].database", i)); err != nil {
			return nil, err
		}
		if err := sp.each(valueOf(t, "modules"), base+"/modules", fmt.Sprintf("tiers[%d].modules", i)); err != nil {
			return nil, err
		}
	}

	var fragPaths []string
	for p := range sp.fragments {
		fragPaths = append(fragPaths, p)
	}
	sort.Strings(fragPaths)
	folded := map[string]string{}
	for _, p := range fragPaths {
		if other, ok := folded[strings.ToLower(p)]; ok {
			return nil, fmt.Errorf("%s and %s differ only in case and would be one file on macOS and Windows; rename one", other, p)
		}
		folded[strings.ToLower(p)] = p
	}

	l := &Layout{Root: name, Files: map[string][]byte{}}
	b, err := encode(&doc)
	if err != nil {
		return nil, err
	}
	l.Files[name] = b
	for p, n := range sp.fragments {
		unbasePaths(n, path.Dir(p))
		if l.Files[p], err = encode(n); err != nil {
			return nil, err
		}
	}
	if err := l.check(data); err != nil {
		return nil, err
	}
	return l, nil
}

type splitter struct {
	fragments map[string]*yaml.Node // slash path -> mapping or sequence
	owner     map[string]string     // entity -> owning module
}

// each moves every entry of seq into dir/<name>.yaml and leaves a `dir:`
// include behind.
func (sp *splitter) each(seq *yaml.Node, dir, where string) error {
	if len(items(seq)) == 0 {
		return nil
	}
	for i, item := range seq.Content {
		n := valueOf(item, "name")
		if n == nil || n.Value == "" {
			return fmt.Errorf("%s[%d] has no name; name it before splitting", where, i)
		}
		file, err := fileName(n.Value)
		if err != nil {
			return fmt.Errorf("%s[%d]: %w", where, i, err)
		}
		p := dir + "/" + file + ".yaml"
		if _, dup := sp.fragments[p]; dup {
			return fmt.Errorf("%s[%d]: %q needs %s, which another entry already uses", where, i, n.Value, p)
		}
		sp.fragments[p] = item
	}
	leaveInclude(seq, dir)
	return nil
}

// entities moves a database's entities into dir/<owner>.yaml, one list per
// owning module, and leaves a `dir:` include behind.
func (sp *splitter) entities(db *yaml.Node, dir, where string) error {
	seq := valueOf(db, "entities")
	if len(items(seq)) == 0 {
		return nil
	}
	for i, e := range seq.Content {
		owner := "_unowned"
		if n := valueOf(e, "name"); n != nil {
			if o, ok := sp.owner[n.Value]; ok {
				owner = o
			}
		}
		file, err := fileName(owner)
		if err != nil {
			return fmt.Errorf("%s.entities[%d]: owner %w", where, i, err)
		}
		p := dir + "/" + file + ".yaml"
		frag := sp.fragments[p]
		if frag == nil {
			frag = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			sp.fragments[p] = frag
		}
		frag.Content = append(frag.Content, e)
	}
	leaveInclude(seq, dir)
	return nil
}

// leaveInclude replaces a list's entries with `- dir: <dir>`, relative to
// the entry point.
func leaveInclude(seq *yaml.Node, dir string) {
	seq.Style = 0
	seq.Content = []*yaml.Node{{
		Kind: yaml.MappingNode, Tag: "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "dir"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: dir},
		},
	}}
}

// fileName turns an entry's name into a file name. Names are identifiers in
// a valid spec; anything that could escape the directory, hide the file
// from a `dir:` include or collide on a case-insensitive file system is
// refused rather than mangled.
func fileName(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\:*?"<>|`) || strings.TrimSpace(name) != name {
		return "", fmt.Errorf("name %q cannot be a file name", name)
	}
	return name, nil
}

// unbasePaths is the inverse of rebasePaths: brief and spec paths in a
// fragment become relative to the fragment's directory, so the loader's
// rebasing yields the original root-relative path.
func unbasePaths(n *yaml.Node, dir string) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind == yaml.ScalarNode && pathKeys[k.Value] && v.Kind == yaml.ScalarNode && v.Value != "" && !filepath.IsAbs(v.Value) {
				if rel, err := filepath.Rel(filepath.FromSlash(dir), filepath.FromSlash(v.Value)); err == nil {
					v.Value = filepath.ToSlash(rel)
				}
				continue
			}
			unbasePaths(v, dir)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			unbasePaths(c, dir)
		}
	}
}

// check writes the layout to a scratch directory, loads it back and
// compares it with the original.
func (l *Layout) check(original []byte) error {
	want, err := Parse(original)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "aspect-split-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := l.Write(tmp); err != nil {
		return err
	}
	data, err := Expand(filepath.Join(tmp, filepath.FromSlash(l.Root)))
	if err != nil {
		return fmt.Errorf("split layout does not load: %w", err)
	}
	got, err := Parse(data)
	if err != nil {
		return fmt.Errorf("split layout does not parse: %w", err)
	}
	normalize(want)
	normalize(got)
	if reflect.DeepEqual(want, got) {
		return nil
	}
	for _, part := range []struct {
		name      string
		want, got any
	}{
		{"modules", want.Modules, got.Modules},
		{"tiers", want.Tiers, got.Tiers},
		{"system.interfaces", want.System.Interfaces, got.System.Interfaces},
		{"system.database", want.System.Database, got.System.Database},
	} {
		if !reflect.DeepEqual(part.want, part.got) {
			return fmt.Errorf("split changed %s; please report this", part.name)
		}
	}
	return fmt.Errorf("split changed the spec; please report this")
}

// CheckTarget refuses a directory where writing the layout would change
// its meaning: a fragment directory that already holds YAML would have
// those files pulled in by the `dir:` include too.
func (l *Layout) CheckTarget(dir string) error {
	for _, d := range l.Dirs() {
		abs := filepath.Join(dir, filepath.FromSlash(d))
		for _, pattern := range []string{"*.yaml", "*.yml"} {
			if existing, _ := filepath.Glob(filepath.Join(abs, pattern)); len(existing) > 0 {
				return fmt.Errorf("%s already holds YAML (%s); move it away before splitting", abs, filepath.Base(existing[0]))
			}
		}
	}
	return nil
}

// Write stores the layout under dir, fragments first and the entry point
// last, so an interrupted write leaves the old entry point in charge.
func (l *Layout) Write(dir string) error {
	paths := l.Paths()
	for _, p := range append(paths[1:], paths[0]) {
		abs := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, l.Files[p], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// normalize puts the lists a split reorders into name order and cleans
// brief paths, which rebasing writes in canonical form.
func normalize(s *Spec) {
	sortModules := func(ms []Module) {
		sort.SliceStable(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
		for i := range ms {
			ms[i].Brief.Path = cleanPath(ms[i].Brief.Path)
			for j := range ms[i].Topics {
				ms[i].Topics[j].Brief.Path = cleanPath(ms[i].Topics[j].Brief.Path)
			}
		}
	}
	sortEntities := func(db *Database) {
		if db != nil {
			sort.SliceStable(db.Entities, func(i, j int) bool { return db.Entities[i].Name < db.Entities[j].Name })
		}
	}
	sortModules(s.Modules)
	sortEntities(s.System.Database)
	sort.SliceStable(s.System.Interfaces, func(i, j int) bool { return s.System.Interfaces[i].Name < s.System.Interfaces[j].Name })
	s.System.Brief.Path = cleanPath(s.System.Brief.Path)
	for i := range s.Tiers {
		sortModules(s.Tiers[i].Modules)
		sortEntities(s.Tiers[i].Database)
		s.Tiers[i].Brief.Path = cleanPath(s.Tiers[i].Brief.Path)
	}
}

func cleanPath(p string) string {
	if p == "" {
		return p
	}
	return path.Clean(p)
}

// findInclude returns the location of the first include directive in n, or
// "".
func findInclude(n *yaml.Node) string {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if kind, _ := includeRef(c); kind != "" {
				return fmt.Sprintf("line %d", c.Line)
			}
			if p := findInclude(c); p != "" {
				return p
			}
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			if kind, _ := includeRef(n.Content[i]); kind != "" {
				return fmt.Sprintf("line %d", n.Content[i].Line)
			}
			if p := findInclude(n.Content[i]); p != "" {
				return p
			}
		}
	}
	return ""
}

func valueOf(n *yaml.Node, key string) *yaml.Node {
	_, v := mapEntry(n, key)
	return v
}

// items returns a sequence's entries; nil for anything else.
func items(n *yaml.Node) []*yaml.Node {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	return n.Content
}

// tierLists returns key's value in every tier.
func tierLists(tiers *yaml.Node, key string) []*yaml.Node {
	var out []*yaml.Node
	for _, t := range items(tiers) {
		out = append(out, valueOf(t, key))
	}
	return out
}

func encode(n *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
