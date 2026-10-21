package spec

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Locate turns an issue path (`modules[3].scenarios[1].id`,
// `system.interfaces[0].surfaces[2]`, `module orders.brief`) into the file
// and line it was written at, such as `modules/orders.yaml:12`. Indices
// count entries of the assembled spec, which is what the validator walks,
// so they resolve through includes. It returns "" when the path does not
// name a node (a summary path like `modules`) or the spec was not loaded
// from disk.
func (src *Sources) Locate(path string) string {
	if src == nil || src.root == nil || path == "" {
		return ""
	}
	n, at := src.root, (*yaml.Node)(nil)
	rest := path
	// Brief owners are named rather than indexed: "module orders",
	// "tier backend".
	for _, owner := range []struct{ prefix, list string }{{"module ", "modules"}, {"tier ", "tiers"}} {
		if !strings.HasPrefix(rest, owner.prefix) {
			continue
		}
		name, tail, _ := strings.Cut(strings.TrimPrefix(rest, owner.prefix), ".")
		if n = namedEntry(src.root, owner.list, name, owner.list == "modules"); n == nil {
			return ""
		}
		at, rest = n, tail
	}
	for seg := range strings.SplitSeq(rest, ".") {
		if seg == "" {
			continue
		}
		key, idx, hasIdx := splitIndex(seg)
		if key != "" {
			k, v := mapEntry(n, key)
			if v == nil {
				break
			}
			at, n = k, v
		}
		if hasIdx {
			if n.Kind != yaml.SequenceNode || idx < 0 || idx >= len(n.Content) {
				break
			}
			n = n.Content[idx]
			at = n
		}
	}
	if at == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d", src.files[at], at.Line)
}

// splitIndex parses `name[3]` into ("name", 3, true); a segment without an
// index returns ok false. A non-numeric index such as `[?]` returns -1.
func splitIndex(seg string) (key string, idx int, ok bool) {
	open := strings.IndexByte(seg, '[')
	if open < 0 || !strings.HasSuffix(seg, "]") {
		return seg, 0, false
	}
	i, err := strconv.Atoi(seg[open+1 : len(seg)-1])
	if err != nil {
		i = -1
	}
	return seg[:open], i, true
}

// mapEntry returns the key and value nodes of key in mapping n.
func mapEntry(n *yaml.Node, key string) (k, v *yaml.Node) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i], n.Content[i+1]
		}
	}
	return nil, nil
}

// namedEntry finds the entry called name in root's list (`modules` or
// `tiers`). With inTiers it also looks at every tier's modules.
func namedEntry(root *yaml.Node, list, name string, inTiers bool) *yaml.Node {
	find := func(seq *yaml.Node) *yaml.Node {
		if seq == nil || seq.Kind != yaml.SequenceNode {
			return nil
		}
		for _, item := range seq.Content {
			if _, v := mapEntry(item, "name"); v != nil && v.Value == name {
				return item
			}
		}
		return nil
	}
	_, seq := mapEntry(root, list)
	if found := find(seq); found != nil || !inTiers {
		return found
	}
	if _, tiers := mapEntry(root, "tiers"); tiers != nil && tiers.Kind == yaml.SequenceNode {
		for _, t := range tiers.Content {
			_, mods := mapEntry(t, "modules")
			if found := find(mods); found != nil {
				return found
			}
		}
	}
	return nil
}

// ModuleFile is the file module name is written in (the root spec or an
// included fragment), as the loader opened it; "" when the spec was not
// loaded from disk or has no such module.
func (src *Sources) ModuleFile(name string) string {
	if src == nil || src.root == nil {
		return ""
	}
	n := namedEntry(src.root, "modules", name, true)
	if n == nil {
		return ""
	}
	return src.files[n]
}
