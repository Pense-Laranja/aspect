package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Pense-Laranja/aspect/internal/spec"
)

// Marshal renders a spec as YAML with a provenance header.
func Marshal(s *spec.Spec, warnings []string) ([]byte, error) {
	body, err := yaml.Marshal(s)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("# Aspect specification recovered by `aspect import`.\n")
	if s.System.Source != nil {
		fmt.Fprintf(&b, "# Source: %s (%s) at %s, imported %s.\n", s.System.Source.Repository, s.System.Source.Language, s.System.Source.Commit, s.System.Source.ImportedAt)
	}
	b.WriteString("# Review intents, goals and scenarios before running the pipeline: they are\n# the model's reading of the code, not the owner's statement of it.\n")
	if len(warnings) > 0 {
		b.WriteString("#\n# Assembly warnings:\n")
		for _, w := range warnings {
			fmt.Fprintf(&b, "#   - %s\n", w)
		}
	}
	b.WriteString("\n")
	b.Write(body)
	return []byte(b.String()), nil
}

// Write stores the spec at path and its brief sidecars next to it.
func Write(path string, s *spec.Spec, warnings []string, briefs map[string]string) error {
	b, err := Marshal(s, warnings)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for rel, content := range briefs {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(path, b, 0o644)
}
