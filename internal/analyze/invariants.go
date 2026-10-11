package analyze

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/eideroliveira/aspect/internal/spec"
)

// InvariantViolation is a deterministic finding where code violates a structural
// or architectural invariant.
type InvariantViolation struct {
	Category string `json:"category"` // correctness, quality, readability, performance, boundary
	Severity string `json:"severity"` // critical, high, medium, low
	Rule     string `json:"rule"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Message  string `json:"message"`
}

func (v InvariantViolation) String() string {
	return fmt.Sprintf("[%s/%s] %s:%d: %s (%s)", v.Category, v.Severity, v.File, v.Line, v.Message, v.Rule)
}

// VerifyModuleDependencies checks that in-module package imports respect
// the module dependency graph declared in spec.Module.DependsOn.
func VerifyModuleDependencies(inv *Inventory, s *spec.Spec) []InvariantViolation {
	if s == nil || inv == nil {
		return nil
	}
	var violations []InvariantViolation

	// Map directory prefix to module
	dirToModule := map[string]*spec.Module{}
	for _, m := range s.AllModules() {
		dir1 := m.Name
		dir2 := strings.ReplaceAll(m.Name, "_", "/")
		dirToModule[dir1] = m
		dirToModule[dir2] = m
	}

	findModule := func(pkgDir string) *spec.Module {
		if m, ok := dirToModule[pkgDir]; ok {
			return m
		}
		// Match prefixes for nested packages
		for dir, m := range dirToModule {
			if strings.HasPrefix(pkgDir, dir+"/") {
				return m
			}
		}
		return nil
	}

	for _, p := range inv.Packages {
		currentMod := findModule(p.Dir)
		if currentMod == nil {
			continue
		}

		declaredDeps := make(map[string]bool, len(currentMod.DependsOn)+1)
		declaredDeps[currentMod.Name] = true
		for _, dep := range currentMod.DependsOn {
			declaredDeps[dep] = true
		}

		for _, imp := range p.Imports {
			impDir := strings.TrimPrefix(imp, inv.ModulePath+"/")
			impMod := findModule(impDir)
			if impMod == nil {
				continue
			}

			if !declaredDeps[impMod.Name] {
				violations = append(violations, InvariantViolation{
					Category: "boundary",
					Severity: "high",
					Rule:     "module-depends-on",
					File:     p.Dir,
					Line:     1,
					Message: fmt.Sprintf("package %q (module %q) imports %q (module %q) without declaring it in depends_on",
						p.Dir, currentMod.Name, impDir, impMod.Name),
				})
			}
		}
	}

	return violations
}

// CheckCodeInvariants inspects a directory of Go source files for adherence
// to performance, quality, and readability invariants.
func CheckCodeInvariants(root string, files []string) ([]InvariantViolation, error) {
	var violations []InvariantViolation
	fset := token.NewFileSet()

	filterFile := func(relPath string) bool {
		if len(files) == 0 {
			return true
		}
		for _, f := range files {
			if f == relPath {
				return true
			}
		}
		return false
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "vendor" || base == "testdata" || base == "node_modules" || base == "external" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		if !filterFile(rel) {
			return nil
		}

		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil // ignore unparseable files
		}

		v := checkAST(fset, f, rel)
		violations = append(violations, v...)
		return nil
	})

	return violations, err
}

func checkAST(fset *token.FileSet, f *ast.File, filePath string) []InvariantViolation {
	var violations []InvariantViolation

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			// 1. Performance: Detecting unbounded database scans (.Find without .Limit in call chain)
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
				if sel.Sel.Name == "Find" {
					// Check if receiver chain has Limit
					if !chainHasLimit(sel.X) {
						pos := fset.Position(node.Pos())
						violations = append(violations, InvariantViolation{
							Category: "performance",
							Severity: "medium",
							Rule:     "unbounded-query-limit",
							File:     filePath,
							Line:     pos.Line,
							Message:  "database .Find() without preceding .Limit() may cause unbounded memory allocations",
						})
					}
				}

				// 2. Readability / Mandate: Hand-rolled custom filter chip/pill components in admin
				if (strings.HasPrefix(filePath, "admin/") || strings.HasPrefix(filePath, "modules/reports/")) &&
					(strings.Contains(sel.Sel.Name, "Chip") || strings.Contains(sel.Sel.Name, "SelectPill")) {
					pos := fset.Position(node.Pos())
					violations = append(violations, InvariantViolation{
						Category: "readability",
						Severity: "high",
						Rule:     "mandate-standard-filter",
						File:     filePath,
						Line:     pos.Line,
						Message:  "hand-rolled chip/pill filter controls are banned; use standard vx.FilterData / vx.VXFilter or translationFilterPanel",
					})
				}
			}

		case *ast.CompositeLit:
			// 3. Performance: http.Client without Timeout
			if isHTTPClient(node) {
				hasTimeout := false
				for _, elt := range node.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Timeout" {
							hasTimeout = true
							break
						}
					}
				}
				if !hasTimeout {
					pos := fset.Position(node.Pos())
					violations = append(violations, InvariantViolation{
						Category: "performance",
						Severity: "high",
						Rule:     "http-client-timeout",
						File:     filePath,
						Line:     pos.Line,
						Message:  "http.Client initialized without explicit Timeout; remote requests may hang indefinitely",
					})
				}
			}

		case *ast.RangeStmt:
			// 4. Performance: N+1 query pattern (database query inside for range loop)
			ast.Inspect(node.Body, func(inner ast.Node) bool {
				if call, ok := inner.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						if isGORMQueryMethod(sel.Sel.Name) {
							pos := fset.Position(call.Pos())
							violations = append(violations, InvariantViolation{
								Category: "performance",
								Severity: "high",
								Rule:     "n-plus-one-query-in-loop",
								File:     filePath,
								Line:     pos.Line,
								Message:  fmt.Sprintf("potential N+1 query: database call .%s() inside loop; consider batching with WHERE IN", sel.Sel.Name),
							})
						}
					}
				}
				return true
			})
		}
		return true
	})

	return violations
}

func chainHasLimit(expr ast.Expr) bool {
	for expr != nil {
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if sel.Sel.Name == "Limit" || sel.Sel.Name == "Take" || sel.Sel.Name == "First" || sel.Sel.Name == "Last" {
			return true
		}
		expr = sel.X
	}
	return false
}

func isHTTPClient(lit *ast.CompositeLit) bool {
	if sel, ok := lit.Type.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "http" && sel.Sel.Name == "Client" {
			return true
		}
	}
	return false
}

func isGORMQueryMethod(name string) bool {
	switch name {
	case "Find", "First", "Take", "Last", "Pluck", "Scan", "Count":
		return true
	default:
		return false
	}
}
