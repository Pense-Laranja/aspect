package spec

import (
	"fmt"
	"os"
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
