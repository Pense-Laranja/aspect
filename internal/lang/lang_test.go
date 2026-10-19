package lang

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eideroliveira/aspect/internal/spec"
	"github.com/eideroliveira/aspect/internal/workspace"
)

func TestNames(t *testing.T) {
	if got := strings.Join(Names(), ","); got != "go,swift" {
		t.Fatalf("Names = %s", got)
	}
	if _, err := For("cobol"); err == nil {
		t.Fatal("want error for unknown language")
	}
}

var stock = &spec.Module{Name: "stock"}

func TestKeepModuleFilesSwift(t *testing.T) {
	p, _ := For("swift")
	files := []workspace.File{
		{Path: "Sources/Stock/A.swift", Content: "a"},
		{Path: "./Sources/Stock/B.swift", Content: "b"},
		{Path: "Sources/Ledger/L.swift", Content: "l"},
		{Path: "Tests/StockTests/T.swift", Content: "t"},
		{Path: "Sources/Stock/README.md", Content: "r"},
	}
	code := p.KeepModuleFiles(stock, files, false)
	tests := p.KeepModuleFiles(stock, files, true)
	if len(code) != 2 || code[1].Path != "Sources/Stock/B.swift" || len(tests) != 1 {
		t.Fatalf("code=%v tests=%v", code, tests)
	}
}

func TestSplitModuleFilesRefusesEscapesDuplicatesAndEmptyFiles(t *testing.T) {
	p, _ := For("go")
	files := []workspace.File{
		{Path: "stock/stock.go", Content: "package stock"},
		{Path: "stock/../ledger/ledger.go", Content: "package ledger"},
		{Path: "stock/./sub/../model.go", Content: "package stock"},
		{Path: "/stock/abs.go", Content: "package stock"},
		{Path: "stock/stock.go", Content: "package stock // again"},
		{Path: "stock/empty.go", Content: "  \n"},
		{Path: "stock/stock_test.go", Content: "package stock"},
		{Path: "", Content: "x"},
		{Path: "../outside/stock/x.go", Content: "package stock"},
	}
	kept, dropped := p.SplitModuleFiles(stock, files, false)
	if len(kept) != 2 || kept[0].Path != "stock/stock.go" || kept[1].Path != "stock/model.go" {
		t.Fatalf("kept = %+v", kept)
	}
	if kept[0].Content != "package stock" {
		t.Fatalf("the first copy of a repeated path must win, got %q", kept[0].Content)
	}
	if len(dropped) != 7 {
		t.Fatalf("dropped = %+v", dropped)
	}
	reasons := map[string]string{}
	for _, d := range dropped {
		reasons[d.Path] = d.Reason
	}
	if !strings.Contains(reasons["stock/../ledger/ledger.go"], "outside the module") {
		t.Errorf("traversal into a sibling module: %q", reasons["stock/../ledger/ledger.go"])
	}
	if !strings.Contains(reasons["../outside/stock/x.go"], "leaves the module") {
		t.Errorf("climb above the root: %q", reasons["../outside/stock/x.go"])
	}
	if !strings.Contains(reasons["stock/stock.go"], "repeated") || !strings.Contains(reasons["stock/empty.go"], "no content") {
		t.Errorf("reasons = %v", reasons)
	}
}

func TestSwiftSyncGeneratesManifest(t *testing.T) {
	s, err := spec.Parse([]byte(`
aspect: 1
system:
  name: shop_app
  intent: i
  language: swift
  module_path: com.example.shop
  goals: [{id: G1, statement: x}]
  stack:
    swift:
      frameworks:
        - {name: ComposableArchitecture, module: https://github.com/pointfreeco/swift-composable-architecture.git, version: "1.15.0"}
modules:
  - {name: ledger, intent: i, goals: [G1]}
  - {name: stock, intent: i, depends_on: [ledger]}
`))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, d := range []string{"Sources/Ledger", "Sources/Stock", "Tests/LedgerTests", "Tests/StockTests"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := swiftSync(root, s, s.EffectiveTiers()[0], []string{"ledger", "stock", "ghost"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "Package.swift"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name: "ShopApp"`,
		`.iOS(.v17), .macOS(.v14)`,
		`.package(url: "https://github.com/pointfreeco/swift-composable-architecture.git", from: "1.15.0")`,
		`.target(name: "Ledger", dependencies: [.product(name: "ComposableArchitecture", package: "swift-composable-architecture")])`,
		`.target(name: "Stock", dependencies: ["Ledger", .product(`,
		`.testTarget(name: "StockTests", dependencies: ["Stock"])`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("Package.swift lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(string(b), "Ghost") {
		t.Fatal("a module without a Sources directory must not be declared")
	}
}

func TestPascalCase(t *testing.T) {
	for in, want := range map[string]string{"stock": "Stock", "stock_ledger": "StockLedger", "a__b": "AB"} {
		if got := PascalCase(in); got != want {
			t.Errorf("PascalCase(%q) = %q", in, got)
		}
	}
}

// An imported spec places modules by package directory. A module so placed
// owns that one directory: not a sibling sharing its prefix, not the
// packages below it, and the root module owns only the root's files.
func TestOwnerByPath(t *testing.T) {
	p, _ := For("go")
	root := &spec.Module{Name: "root", Path: "."}
	modules := &spec.Module{Name: "modules", Path: "modules"}
	api := &spec.Module{Name: "modules_api", Path: "modules/api"}
	apikeys := &spec.Module{Name: "modules_apikeys", Path: "modules/apikeys"}
	underscore := &spec.Module{Name: "store_product_render", Path: "store/product_render"}
	legacy := &spec.Module{Name: "stock"} // laid out by name: owns stock/ and below
	mods := []*spec.Module{root, modules, api, apikeys, underscore, legacy}
	for file, want := range map[string]*spec.Module{
		"main.go":                        root,
		"main_test.go":                   root,
		"modules/modules.go":             modules,
		"modules/api/api.go":             api,
		"modules/api/api_test.go":        api,
		"modules/apikeys/keys.go":        apikeys,
		"modules/api/v2/v2.go":           nil, // a package no module claims
		"store/product_render/render.go": underscore,
		"store/store.go":                 nil,
		"stock/stock.go":                 legacy,
		"stock/sub/sub.go":               legacy,
		"stockpile/x.go":                 nil,
	} {
		if got := p.Owner(mods, file); got != want {
			t.Errorf("Owner(%s) = %v, want %v", file, name(got), name(want))
		}
	}
	if got := goPattern(root); got != "." {
		t.Errorf("root pattern = %q", got)
	}
	if got := goPattern(api); got != "./modules/api" {
		t.Errorf("path pattern = %q, want one package", got)
	}
	if got := goPattern(legacy); got != "./stock/..." {
		t.Errorf("legacy pattern = %q", got)
	}
}

// A coder writing a path-placed module may write only into its directory.
func TestSplitModuleFilesByPath(t *testing.T) {
	p, _ := For("go")
	api := &spec.Module{Name: "modules_api", Path: "modules/api"}
	files := []workspace.File{
		{Path: "modules/api/api.go", Content: "package api"},
		{Path: "modules/api/v2/v2.go", Content: "package v2"},
		{Path: "modules_api/api.go", Content: "package modules_api"},
	}
	kept, dropped := p.SplitModuleFiles(api, files, false)
	if len(kept) != 1 || kept[0].Path != "modules/api/api.go" || len(dropped) != 2 {
		t.Fatalf("kept=%+v dropped=%+v", kept, dropped)
	}
	root := &spec.Module{Name: "root", Path: "."}
	kept, _ = p.SplitModuleFiles(root, []workspace.File{{Path: "main.go", Content: "package main"}, {Path: "cmd/x.go", Content: "package main"}}, false)
	if len(kept) != 1 || kept[0].Path != "main.go" {
		t.Fatalf("root kept=%+v", kept)
	}
}

func name(m *spec.Module) string {
	if m == nil {
		return "<none>"
	}
	return m.Name
}
