package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eideroliveira/aspect/internal/spec"
)

func TestModuleDir(t *testing.T) {
	for dir, want := range map[string]string{".": ".", "": ".", "modules/api": "modules/api", "store/product_render": "store/product_render"} {
		if got := ModuleDir(dir); got != want {
			t.Errorf("ModuleDir(%q) = %q, want %q", dir, got, want)
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

// A tiered spec: the Go tier's code is root/<tier>, a tier in another
// language is skipped rather than failing the whole recovery, and a package
// another module already holds by path is refused.
func TestRecoverPathsTiered(t *testing.T) {
	repo := t.TempDir()
	for p, c := range map[string]string{
		"backend/go.mod":             "module example.com/backend\n\ngo 1.24\n",
		"backend/modules/api/api.go": "package api\n",
		"backend/store/store.go":     "package store\n",
	} {
		abs := filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &spec.Spec{Tiers: []spec.Tier{
		{Name: "backend", Language: "go", Modules: []spec.Module{
			{Name: "store"}, {Name: "modules_api"}, {Name: "api_by_hand", Path: "modules/api"},
		}},
		{Name: "mobile", Language: "swift", Modules: []spec.Module{{Name: "catalog_feature"}}},
	}}
	paths, problems, err := RecoverPaths(s, repo)
	if err != nil {
		t.Fatalf("a swift tier must be skipped, got %v", err)
	}
	if len(paths) != 1 || paths["store"] != "store" {
		t.Errorf("paths = %v", paths)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "module modules_api: its package modules/api is already module api_by_hand's path") {
		t.Errorf("problems = %v", problems)
	}
}
