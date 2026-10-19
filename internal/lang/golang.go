package lang

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/eideroliveira/aspect/internal/spec"
	"github.com/eideroliveira/aspect/internal/workspace"
)

func init() {
	register(&Profile{
		Name:        "go",
		DisplayName: "Go",
		SourceExt:   ".go",
		CodeDir:     goDir,
		TestDir:     goDir,
		IsTestFile:  func(p string) bool { return strings.HasSuffix(p, "_test.go") },
		SkipDir:     goSkipDir,
		Init:        goInit,
		Sync:        func(string, *spec.Spec, *spec.Tier, []string) error { return nil },
		Prepare:     [][]string{{"go", "mod", "tidy"}},
		Env:         []string{"GOFLAGS=-mod=mod", "GOWORK=off"},
		Steps: func(m *spec.Module) [][]string {
			return [][]string{
				{"go", "vet", goPattern(m)},
				{"go", "test", "-count=1", "-race", goPattern(m)},
			}
		},
		CheckImports: workspace.CheckGoImports,
		CoderRules: `Go rules:
- A module named "stock" lives in stock/ as package stock. Only write files inside that directory, and never files ending in _test.go.
- Import dependency modules by their import path (<module_path>/<module>). Do not re-implement them.
- Import only the standard library, this system's own module, and the modules listed under the stack section. The pipeline parses your imports and rejects anything else.
- Exported identifiers get doc comments. Errors are values; wrap with %w.`,
		TesterRules: `Go rules:
- Tests live in the module's directory as package <module> (internal tests) unless the spec requires the external package, in files ending in _test.go. Never write implementation files.
- Standard library testing only, no assertion frameworks. Table-driven tests and t.Run subtests.
- Concurrency constraints get a test that runs operations from many goroutines; the pipeline runs go test -race.
- Property-style tests use math/rand with a fixed seed so failures reproduce.`,
	})
}

// goDir is a Go module's directory: its spec path when it has one (one
// package, possibly the root), else <name>/.
func goDir(m *spec.Module) string {
	switch m.Path {
	case "":
		return m.Name + "/"
	case ".":
		return ""
	}
	return m.Path + "/"
}

// goSkipDir leaves out what ./... leaves out: testdata, vendor, directories
// starting with an underscore, and nested modules, which are other projects.
func goSkipDir(dir string) bool {
	name := filepath.Base(dir)
	if name == "testdata" || name == "vendor" || name == "node_modules" || strings.HasPrefix(name, "_") {
		return true
	}
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

// goPattern is the package pattern go vet and go test run on. A module laid
// out by name may hold subpackages; a module placed by path is exactly one
// package, and the packages below it belong to other modules.
func goPattern(m *spec.Module) string {
	switch m.Path {
	case "":
		return "./" + m.Name + "/..."
	case ".":
		return "."
	}
	return "./" + m.Path
}

func goInit(root string, _ *spec.Spec, t *spec.Tier) error {
	gomod := filepath.Join(root, "go.mod")
	if _, err := os.Stat(gomod); err == nil {
		return nil
	}
	version := runtime.Version()
	if ls := t.LanguageStack(); ls != nil && ls.Version != "" {
		version = ls.Version
	}
	version = strings.TrimPrefix(version, "go")
	if i := strings.Index(version, " "); i > 0 {
		version = version[:i]
	}
	content := fmt.Sprintf("module %s\n\ngo %s\n", t.ModulePath, version)
	return os.WriteFile(gomod, []byte(content), 0o644)
}
