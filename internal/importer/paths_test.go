package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eideroliveira/aspect/internal/spec"
)

func TestModulePath(t *testing.T) {
	for dir, want := range map[string]string{".": ".", "": ".", "modules/api": "modules/api", "store/product_render": "store/product_render"} {
		if got := ModulePath(dir); got != want {
			t.Errorf("ModulePath(%q) = %q, want %q", dir, got, want)
		}
	}
}

// A spec imported before modules recorded their directory gets each path
// back from the packages on disk; a name two packages map to is reported,
// not guessed, and an excluded tree is not searched.
func TestRecoverPaths(t *testing.T) {
	repo := t.TempDir()
	for p, c := range map[string]string{
		"go.mod":                         "module example.com/shop\n\ngo 1.24\n",
		"shop.go":                        "package shop\n",
		"modules/api/api.go":             "package api\n",
		"modules/jobs/jobtypes/types.go": "package jobtypes\n",
		"store/product_render/render.go": "package product_render\n",
		"report/x/x.go":                  "package x\n",
		"report_x/x.go":                  "package report_x\n",
		"cmd/tool/main.go":               "package main\n",
		"cmd_tool/tool.go":               "package cmd_tool\n",
	} {
		abs := filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mod := func(name string) spec.Module { return spec.Module{Name: name, Intent: "x"} }
	s := &spec.Spec{Modules: []spec.Module{
		mod("root"), mod("modules_api"), mod("modules_jobs_jobtypes"), mod("store_product_render"),
		mod("report_x"), mod("cmd_tool"), mod("gone"), {Name: "placed", Path: "somewhere"},
	}}
	s.System.Language = "go"
	s.System.Source = &spec.Source{Exclude: []string{"cmd"}}

	paths, problems, err := RecoverPaths(s, repo)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"root": ".", "modules_api": "modules/api", "modules_jobs_jobtypes": "modules/jobs/jobtypes",
		"store_product_render": "store/product_render", "cmd_tool": "cmd_tool",
	}
	if len(paths) != len(want) {
		t.Errorf("paths = %v", paths)
	}
	for name, p := range want {
		if paths[name] != p {
			t.Errorf("%s: path %q, want %q", name, paths[name], p)
		}
	}
	text := strings.Join(problems, "\n")
	for _, w := range []string{"module report_x: ambiguous, packages report/x, report_x all map to it", "module gone: no package"} {
		if !strings.Contains(text, w) {
			t.Errorf("missing problem %q in:\n%s", w, text)
		}
	}
	if len(problems) != 2 {
		t.Errorf("problems = %v", problems)
	}
}
