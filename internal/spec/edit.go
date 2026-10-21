package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetModulePaths writes `path: <dir>` into the module entries named in
// paths, in whichever file of the spec each one is written (the root file or
// an included fragment). It edits text rather than re-marshalling, so
// comments, key order and quoting are kept: the new key goes on the line
// after the module's `name:`, at the same indentation. A module that already
// has a path is left alone, as is a name not in the spec. It returns the
// files it changed, sorted.
func SetModulePaths(specPath string, paths map[string]string) ([]string, error) {
	root, src, err := expandFile(specPath)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("%s: empty spec", specPath)
	}
	type insert struct {
		line int // 1-based line of the module's name key
		text string
	}
	edits := map[string][]insert{}
	for _, name := range sortedKeys(paths) {
		item := namedEntry(root, "modules", name, true)
		if item == nil {
			continue
		}
		if k, _ := mapEntry(item, "path"); k != nil {
			continue
		}
		if item.Style&yaml.FlowStyle != 0 {
			return nil, fmt.Errorf("module %s is written as a flow mapping ({...}); add `path: %s` to it by hand", name, paths[name])
		}
		key, _ := mapEntry(item, "name")
		value, err := yaml.Marshal(paths[name])
		if err != nil {
			return nil, err
		}
		file := src.files[key]
		edits[file] = append(edits[file], insert{
			line: key.Line,
			text: strings.Repeat(" ", key.Column-1) + "path: " + strings.TrimSpace(string(value)),
		})
	}
	var changed []string
	for file, ins := range edits {
		data, err := os.ReadFile(file)
		if err != nil {
			return changed, err
		}
		lines := strings.SplitAfter(string(data), "\n")
		// Insert bottom-up so earlier line numbers stay valid.
		sort.Slice(ins, func(i, j int) bool { return ins[i].line > ins[j].line })
		for _, in := range ins {
			if in.line < 1 || in.line > len(lines) {
				return changed, fmt.Errorf("%s:%d: module name line out of range", file, in.line)
			}
			if !strings.HasSuffix(lines[in.line-1], "\n") {
				lines[in.line-1] += "\n"
			}
			lines = append(lines[:in.line], append([]string{in.text + "\n"}, lines[in.line:]...)...)
		}
		info, err := os.Stat(file)
		if err != nil {
			return changed, err
		}
		if err := os.WriteFile(file, []byte(strings.Join(lines, "")), info.Mode().Perm()); err != nil {
			return changed, err
		}
		changed = append(changed, file)
	}
	sort.Strings(changed)
	return changed, nil
}

// SetModuleBriefs points the named modules' briefs at new files: briefs maps
// a module name to a path relative to the root spec's directory, as Brief.Path
// is. Each value is written relative to the file the module is written in
// (the root spec or an included fragment), as the loader expects. Like
// SetModulePaths it edits text: an existing `brief:` keeps its line (and any
// comment on it) with the new value; a module without one gets the key on
// the line after its `name:`. A name not in the spec is skipped. It returns
// the files it changed, sorted.
func SetModuleBriefs(specPath string, briefs map[string]string) ([]string, error) {
	root, src, err := expandFile(specPath)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("%s: empty spec", specPath)
	}
	rootDir := filepath.Dir(specPath)
	type edit struct {
		line    int  // 1-based
		col     int  // 1-based column the value starts at (replace)
		replace bool // replace the value on line from col, else insert after line
		text    string
	}
	edits := map[string][]edit{}
	for _, name := range sortedKeys(briefs) {
		item := namedEntry(root, "modules", name, true)
		if item == nil {
			continue
		}
		if item.Style&yaml.FlowStyle != 0 {
			return nil, fmt.Errorf("module %s is written as a flow mapping ({...}); set its brief by hand", name)
		}
		nameKey, _ := mapEntry(item, "name")
		file := src.files[nameKey]
		rel, err := filepath.Rel(filepath.Dir(file), filepath.Join(rootDir, filepath.FromSlash(briefs[name])))
		if err != nil {
			return nil, err
		}
		value, err := yaml.Marshal(filepath.ToSlash(rel))
		if err != nil {
			return nil, err
		}
		v := strings.TrimSpace(string(value))
		if k, val := mapEntry(item, "brief"); k != nil {
			if val.Kind != yaml.ScalarNode || val.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 || val.Line != k.Line {
				return nil, fmt.Errorf("module %s: its brief is not a one-line value; set it by hand", name)
			}
			if val.LineComment != "" {
				v += " " + val.LineComment
			}
			edits[file] = append(edits[file], edit{line: val.Line, col: val.Column, replace: true, text: v})
			continue
		}
		edits[file] = append(edits[file], edit{line: nameKey.Line, text: strings.Repeat(" ", nameKey.Column-1) + "brief: " + v})
	}
	var changed []string
	for _, file := range sortedKeys(edits) {
		data, err := os.ReadFile(file)
		if err != nil {
			return changed, err
		}
		lines := strings.SplitAfter(string(data), "\n")
		ed := edits[file]
		// Bottom-up so earlier line numbers stay valid.
		sort.Slice(ed, func(i, j int) bool { return ed[i].line > ed[j].line })
		for _, e := range ed {
			if e.line < 1 || e.line > len(lines) {
				return changed, fmt.Errorf("%s:%d: line out of range", file, e.line)
			}
			if e.replace {
				l := lines[e.line-1]
				nl := ""
				if strings.HasSuffix(l, "\n") {
					nl = "\n"
				}
				if e.col-1 > len(l) {
					return changed, fmt.Errorf("%s:%d: column out of range", file, e.line)
				}
				lines[e.line-1] = l[:e.col-1] + e.text + nl
				continue
			}
			if !strings.HasSuffix(lines[e.line-1], "\n") {
				lines[e.line-1] += "\n"
			}
			lines = append(lines[:e.line], append([]string{e.text + "\n"}, lines[e.line:]...)...)
		}
		info, err := os.Stat(file)
		if err != nil {
			return changed, err
		}
		if err := os.WriteFile(file, []byte(strings.Join(lines, "")), info.Mode().Perm()); err != nil {
			return changed, err
		}
		changed = append(changed, file)
	}
	sort.Strings(changed)
	return changed, nil
}
