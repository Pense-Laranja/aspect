// Package importer recovers an Aspect spec from an existing codebase. The
// inventory is deterministic; the Describer reads one package at a time; the
// Synthesizer writes the system level; assembly is deterministic again so
// the same fragments always produce the same spec.
//
// In retarget mode (target language differs from the source) the
// Synthesizer designs the module list for the target platform and the
// assembler only sanitises and validates it.
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eideroliveira/aspect/internal/agents"
	"github.com/eideroliveira/aspect/internal/analyze"
	"github.com/eideroliveira/aspect/internal/llm"
	"github.com/eideroliveira/aspect/internal/spec"
)

// Options configure an import.
type Options struct {
	Root    string
	Include []string
	Exclude []string
	// Target language of the new system (monolith) or of its frontend tier
	// (api_backend, cloud_service); empty means the source language.
	Target     string
	TargetHint string
	// Topology of the new system. Empty means monolith when Target is the
	// source language and api_backend otherwise.
	Topology spec.Topology
	// BackendLanguage of the api_backend backend tier; empty means the
	// source language, in which case the backend mirrors the source packages.
	BackendLanguage string
	// Name overrides the system name (default: last element of the module path).
	Name string
	// ModulePath overrides the target module path.
	ModulePath string
	// Repository and Commit are recorded as provenance.
	Repository string
	Commit     string
	// CacheDir stores fragments so a re-run skips packages already described.
	CacheDir    string
	Concurrency int
	Log         io.Writer
}

// Result of an import.
type Result struct {
	Inventory *analyze.Inventory
	Fragments []agents.Fragment
	Synthesis agents.Synthesis
	Spec      *spec.Spec
	Issues    spec.Issues
	Warnings  []string
	Usage     llm.Usage
	// Briefs are sidecar documents to write next to the spec, keyed by the
	// relative path the spec references.
	Briefs map[string]string
}

// Run performs a full import.
func Run(ctx context.Context, c llm.Client, opts Options) (*Result, error) {
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	inv, err := analyze.Go(opts.Root, analyze.Options{Include: opts.Include, Exclude: opts.Exclude})
	if err != nil {
		return nil, err
	}
	if len(inv.Packages) == 0 {
		return nil, fmt.Errorf("importer: no packages found under %s", opts.Root)
	}
	res := &Result{Inventory: inv}
	fmt.Fprintf(opts.Log, "== describing %d packages (concurrency %d)\n", len(inv.Packages), opts.Concurrency)

	frags, usage, err := describeAll(ctx, c, inv, opts)
	res.Usage = usage
	if err != nil {
		return res, err
	}
	res.Fragments = frags

	fmt.Fprintf(opts.Log, "== synthesizing (%s, target %s, mode %s)\n", topologyOf(inv, opts), targetLanguage(inv, opts), modeOf(inv, opts))
	synth, resp, err := synthesize(ctx, c, inv, frags, opts)
	res.Usage.Add(resp)
	if err != nil {
		return res, err
	}
	res.Synthesis = synth

	res.Spec, res.Warnings = Assemble(inv, frags, synth, opts)
	res.Briefs = briefs(inv, res.Spec)
	res.Issues = spec.Validate(res.Spec)
	return res, nil
}

func targetLanguage(inv *analyze.Inventory, opts Options) string {
	if opts.Target != "" {
		return opts.Target
	}
	return inv.Language
}

func backendLanguage(inv *analyze.Inventory, opts Options) string {
	if opts.BackendLanguage != "" {
		return opts.BackendLanguage
	}
	return inv.Language
}

// topologyOf resolves the requested topology: monolith when the language
// does not change, api_backend when it does (a mobile app cannot connect to
// the database directly), unless the user chose.
func topologyOf(inv *analyze.Inventory, opts Options) spec.Topology {
	if opts.Topology != "" {
		return opts.Topology
	}
	if targetLanguage(inv, opts) != inv.Language {
		return spec.APIBackend
	}
	return spec.Monolith
}

func modeOf(inv *analyze.Inventory, opts Options) agents.Mode {
	switch topologyOf(inv, opts) {
	case spec.APIBackend, spec.CloudService:
		return agents.ModeTiered
	}
	if targetLanguage(inv, opts) != inv.Language {
		return agents.ModeRetarget
	}
	return agents.ModeMirror
}

// mirrorsBackend reports whether the api_backend backend tier keeps the
// source packages as its modules.
func mirrorsBackend(inv *analyze.Inventory, opts Options) bool {
	return topologyOf(inv, opts) == spec.APIBackend && backendLanguage(inv, opts) == inv.Language
}

func describeAll(ctx context.Context, c llm.Client, inv *analyze.Inventory, opts Options) ([]agents.Fragment, llm.Usage, error) {
	d := &agents.Describer{LLM: c}
	frags := make([]agents.Fragment, len(inv.Packages))
	errs := make([]error, len(inv.Packages))
	var mu sync.Mutex
	var usage llm.Usage
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	for i := range inv.Packages {
		pkg := &inv.Packages[i]
		if f, ok := loadCached(opts.CacheDir, pkg.Dir); ok {
			fmt.Fprintf(opts.Log, "   %-40s cached\n", pkg.Dir)
			frags[i] = f
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				errs[i] = ctx.Err()
				return
			}
			f, resp, err := d.Describe(ctx, inv, pkg)
			mu.Lock()
			usage.Add(resp)
			mu.Unlock()
			if err != nil {
				errs[i] = err
				fmt.Fprintf(opts.Log, "   %-40s error: %v\n", pkg.Dir, err)
				return
			}
			frags[i] = f
			saveCached(opts.CacheDir, pkg.Dir, f)
			fmt.Fprintf(opts.Log, "   %-40s %d entities, %d interfaces, %d ops, %d scenarios\n", pkg.Dir, len(f.Entities), len(f.Interfaces), len(f.Operations), len(f.Scenarios))
		}(i)
	}
	wg.Wait()
	return frags, usage, errors.Join(errs...)
}

func synthesize(ctx context.Context, c llm.Client, inv *analyze.Inventory, frags []agents.Fragment, opts Options) (agents.Synthesis, llm.Response, error) {
	key := fmt.Sprintf("_synthesis.%s.%s", topologyOf(inv, opts), targetLanguage(inv, opts))
	if cached, ok := loadCachedSynthesis(opts.CacheDir, key); ok {
		fmt.Fprintf(opts.Log, "   synthesis cached\n")
		return cached, llm.Response{}, nil
	}
	var mirrored []string
	if mirrorsBackend(inv, opts) {
		for _, p := range inv.Packages {
			mirrored = append(mirrored, ModuleName(p.Dir))
		}
	}
	s := &agents.Synthesizer{LLM: c}
	out, resp, err := s.Synthesize(ctx, agents.SynthesisInput{
		Inventory: inv, Fragments: frags, Mode: modeOf(inv, opts), Topology: topologyOf(inv, opts),
		Target: targetLanguage(inv, opts), BackendLanguage: backendLanguage(inv, opts),
		MirroredModules: mirrored, TargetHint: opts.TargetHint,
	})
	if err == nil {
		saveCachedSynthesis(opts.CacheDir, key, out)
	}
	return out, resp, err
}

func cachePath(dir, key string) string {
	return filepath.Join(dir, strings.ReplaceAll(strings.ReplaceAll(key, "/", "__"), ".", "_")+".json")
}

func loadCached(dir, pkgDir string) (agents.Fragment, bool) {
	var f agents.Fragment
	if dir == "" {
		return f, false
	}
	b, err := os.ReadFile(cachePath(dir, "pkg."+pkgDir))
	if err != nil || json.Unmarshal(b, &f) != nil {
		return f, false
	}
	return f, true
}

func saveCached(dir, pkgDir string, f agents.Fragment) {
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	if b, err := json.MarshalIndent(f, "", "  "); err == nil {
		_ = os.WriteFile(cachePath(dir, "pkg."+pkgDir), b, 0o644)
	}
}

func loadCachedSynthesis(dir, key string) (agents.Synthesis, bool) {
	var s agents.Synthesis
	if dir == "" {
		return s, false
	}
	b, err := os.ReadFile(cachePath(dir, key))
	if err != nil || json.Unmarshal(b, &s) != nil {
		return s, false
	}
	return s, true
}

func saveCachedSynthesis(dir, key string, s agents.Synthesis) {
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	if b, err := json.MarshalIndent(s, "", "  "); err == nil {
		_ = os.WriteFile(cachePath(dir, key), b, 0o644)
	}
}

var nonIdent = regexp.MustCompile(`[^a-z0-9]+`)

// Ident turns any string into a spec identifier: lowercase, snake_case,
// starting with a letter.
func Ident(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(nonIdent.ReplaceAllString(s, "_"), "_")
	if s == "" {
		return "x"
	}
	if s[0] < 'a' || s[0] > 'z' {
		s = "p_" + s
	}
	return s
}

// ModuleName maps a package directory to a module name. The mapping loses
// information (modules/api and modules_api both become modules_api), so
// the assembler also records the directory itself as the module's path;
// see ModuleDir.
func ModuleName(dir string) string {
	if dir == "." || dir == "" {
		return "root"
	}
	return Ident(dir)
}

// ModuleDir is the spec path of the module mirrored from a package
// directory: the directory as a slash path, "." for the root.
func ModuleDir(dir string) string {
	if dir == "" {
		return "."
	}
	return filepath.ToSlash(dir)
}

// Assemble builds the spec from the inventory, the fragments and the
// synthesis. It never fails: everything it cannot reconcile becomes a
// warning and, where the spec would otherwise be invalid, a conservative
// default, so the user always gets a file to edit.
func Assemble(inv *analyze.Inventory, frags []agents.Fragment, synth agents.Synthesis, opts Options) (*spec.Spec, []string) {
	a := &assembler{inv: inv, opts: opts}
	target := targetLanguage(inv, opts)
	topology := topologyOf(inv, opts)
	s := &spec.Spec{Aspect: spec.Version}
	s.System.Topology = topology
	s.System.Name = opts.Name
	if s.System.Name == "" {
		s.System.Name = Ident(synth.Name)
		if synth.Name == "" {
			s.System.Name = Ident(filepath.Base(inv.ModulePath))
		}
	}
	s.System.Intent = strings.TrimSpace(synth.Intent)
	s.System.Constraints = synth.Constraints
	s.System.Source = &spec.Source{
		Language: inv.Language, Repository: opts.Repository, Commit: opts.Commit,
		ImportedAt: time.Now().UTC().Format(time.RFC3339), Frameworks: inv.Frameworks,
		Exclude: opts.Exclude,
	}

	switch modeOf(inv, opts) {
	case agents.ModeMirror:
		s.System.Language = target
		s.System.ModulePath = firstNonEmpty(opts.ModulePath, inv.ModulePath)
		if !emptyStack(synth.Stack) {
			s.System.Stack = spec.Stack{target: synth.Stack}
		}
		m := a.mirror(frags, synth.Interfaces, "")
		s.Modules = m.modules
		s.System.Database = m.database
		s.System.Interfaces = m.interfaces
	case agents.ModeRetarget:
		s.System.Language = target
		s.System.ModulePath = firstNonEmpty(opts.ModulePath, "com.example."+s.System.Name)
		if !emptyStack(synth.Stack) {
			s.System.Stack = spec.Stack{target: synth.Stack}
		}
		a.retarget(s, synth)
	case agents.ModeTiered:
		a.tiered(s, frags, synth, topology)
	}
	a.goals(s, synth)
	a.tidy(s)
	return s, a.warnings
}

// briefs turns the documents found in mirrored packages into sidecar files
// (briefs/<module>.md) and points the module's brief at them. The text is
// also placed on the module so the in-memory spec validates and agents see
// it without a reload.
func briefs(inv *analyze.Inventory, s *spec.Spec) map[string]string {
	out := map[string]string{}
	for _, p := range inv.Packages {
		if len(p.Docs) == 0 {
			continue
		}
		m := s.Module(ModuleName(p.Dir))
		if m == nil {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s from %s. Edit freely; this is the module's brief. -->\n\n", copyMarker, p.ImportPath)
		for i, d := range p.Docs {
			if i > 0 {
				b.WriteString("\n\n---\n\n")
			}
			fmt.Fprintf(&b, "<!-- %s -->\n\n%s", d.Name, strings.TrimSpace(d.Content))
		}
		rel := filepath.ToSlash(filepath.Join("briefs", m.Name+".md"))
		out[rel] = b.String()
		m.Brief = spec.Brief{Path: rel, Text: b.String()}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

type assembler struct {
	inv      *analyze.Inventory
	opts     Options
	warnings []string
}

func (a *assembler) warn(format string, args ...any) {
	a.warnings = append(a.warnings, fmt.Sprintf(format, args...))
}

func emptyStack(ls spec.LanguageStack) bool {
	return ls.Version == "" && len(ls.Frameworks) == 0 && ls.ORM == nil && len(ls.AllowedModules) == 0 && ls.Guidance == ""
}

// mirrored is what one module per package yields.
type mirrored struct {
	modules    []spec.Module
	database   *spec.Database
	interfaces []spec.Interface
}

// mirror builds one module per package. Interface metadata from the
// synthesis wins over fragments; provider is the tier the mirrored modules
// live in.
func (a *assembler) mirror(frags []agents.Fragment, synthIfaces []spec.Interface, provider string) mirrored {
	byDir := map[string]agents.Fragment{}
	for _, f := range frags {
		byDir[f.Package] = f
	}
	names := map[string]string{} // import path -> module name
	for _, p := range a.inv.Packages {
		names[p.ImportPath] = ModuleName(p.Dir)
	}

	var out mirrored
	entityOwner := map[string]string{}
	ifaces := map[string]*spec.Interface{}
	var ifaceOrder []string
	for _, si := range synthIfaces {
		if provider != "" && si.Provider != "" && si.Provider != provider && si.Provider != spec.External {
			continue // belongs to another tier; the caller places it
		}
		name := Ident(si.Name)
		cp := si
		cp.Name = name
		cp.Surfaces = nil
		if cp.Provider == "" {
			cp.Provider = provider
		}
		ifaces[name] = &cp
		ifaceOrder = append(ifaceOrder, name)
	}

	for _, p := range a.inv.Packages {
		f := byDir[p.Dir]
		var invs []spec.Invariant
		for _, invStr := range f.Invariants {
			invStr = strings.TrimSpace(invStr)
			if invStr != "" && invStr != "{}" {
				invs = append(invs, spec.Invariant{Statement: invStr})
			}
		}
		m := spec.Module{
			Name:        ModuleName(p.Dir),
			Path:        ModuleDir(p.Dir),
			Intent:      strings.TrimSpace(f.Intent),
			Interface:   f.Operations,
			Invariants:  invs,
			Scenarios:   f.Scenarios,
			Constraints: f.Constraints,
		}
		if m.Intent == "" {
			m.Intent = fmt.Sprintf("Package %s (no description recovered).", p.ImportPath)
			a.warn("module %s: no intent recovered", m.Name)
		}
		for _, imp := range p.Imports {
			if n, ok := names[imp]; ok {
				m.DependsOn = append(m.DependsOn, n)
			}
		}
		sort.Strings(m.DependsOn)
		for _, e := range f.Entities {
			if owner, dup := entityOwner[e.Name]; dup {
				a.warn("entity %s defined by %s and %s; keeping %s", e.Name, owner, m.Name, owner)
				continue
			}
			entityOwner[e.Name] = m.Name
			if out.database == nil {
				out.database = &spec.Database{Engine: detectEngine(a.inv.Frameworks), Migrations: "auto", Test: spec.DBTest{Engine: "sqlite"}, Tier: provider}
			}
			out.database.Entities = append(out.database.Entities, e)
			m.Entities = append(m.Entities, e.Name)
		}
		for _, fi := range f.Interfaces {
			name := Ident(fi.Name)
			iface, ok := ifaces[name]
			if !ok {
				cp := fi
				cp.Name = name
				cp.Surfaces = nil
				cp.Provider = provider
				ifaces[name] = &cp
				iface = &cp
				ifaceOrder = append(ifaceOrder, name)
			}
			if iface.Kind == "" {
				iface.Kind = fi.Kind
			}
			if iface.Intent == "" {
				iface.Intent = fi.Intent
			}
			for _, sf := range fi.Surfaces {
				sf.Name = Ident(sf.Name)
				if surfaceExists(iface, sf.Name) {
					sf.Name = Ident(m.Name + "_" + sf.Name)
				}
				iface.Surfaces = append(iface.Surfaces, sf)
				m.Surfaces = append(m.Surfaces, iface.Name+"."+sf.Name)
			}
		}
		out.modules = append(out.modules, m)
	}
	for _, name := range ifaceOrder {
		if len(ifaces[name].Surfaces) > 0 {
			out.interfaces = append(out.interfaces, *ifaces[name])
		}
	}
	return out
}

// retarget takes the single-tier module design from the synthesis.
func (a *assembler) retarget(s *spec.Spec, synth agents.Synthesis) {
	s.System.Database = synth.Database
	if s.System.Database != nil && len(s.System.Database.Entities) == 0 {
		s.System.Database = nil
	}
	if s.System.Database != nil {
		s.System.Database.Tier = ""
	}
	for _, iface := range synth.Interfaces {
		if iface.Provider != spec.External {
			iface.Provider = ""
		}
		s.System.Interfaces = append(s.System.Interfaces, sanitizeInterface(iface))
	}
	s.Modules = sanitizeModules(synth.Modules)
	if len(s.Modules) == 0 {
		a.warn("the synthesis proposed no modules for the target; add them by hand")
	}
}

// tiered assembles api_backend and cloud_service specs.
func (a *assembler) tiered(s *spec.Spec, frags []agents.Fragment, synth agents.Synthesis, topology spec.Topology) {
	target := targetLanguage(a.inv, a.opts)
	backendLang := backendLanguage(a.inv, a.opts)

	if topology == spec.CloudService {
		// One app tier; everything server-side is external.
		s.System.Language = target
		s.System.ModulePath = firstNonEmpty(a.opts.ModulePath, "com.example."+s.System.Name)
		var app *spec.Tier
		for i := range synth.Tiers {
			if synth.Tiers[i].Language == target || app == nil {
				app = &synth.Tiers[i]
			}
		}
		if app == nil {
			a.warn("the synthesis proposed no app tier; add modules by hand")
			return
		}
		if !emptyStack(app.Stack[target]) {
			s.System.Stack = spec.Stack{target: app.Stack[target]}
		} else if !emptyStack(synth.Stack) {
			s.System.Stack = spec.Stack{target: synth.Stack}
		}
		s.Modules = sanitizeModules(app.Modules)
		if synth.Database != nil && len(synth.Database.Entities) > 0 {
			synth.Database.Tier = spec.External
			s.System.Database = synth.Database
		}
		if app.Database != nil && len(app.Database.Entities) > 0 {
			// A single-tier spec has no tier-local database: the cache is
			// the system database when the remote one is not described.
			if s.System.Database == nil {
				app.Database.Tier = ""
				s.System.Database = app.Database
			} else {
				a.warn("cloud_service app cache database dropped: a single-tier spec holds one database; merge it into the external model or use api_backend")
			}
		}
		for _, iface := range synth.Interfaces {
			if iface.Provider != spec.External {
				iface.Provider = ""
			}
			if iface.Provider == spec.External && iface.Service == "" {
				iface.Service = "hosted service (name it)"
			}
			s.System.Interfaces = append(s.System.Interfaces, sanitizeInterface(iface))
		}
		return
	}

	// api_backend: a backend tier and a frontend tier.
	var backend, frontend *spec.Tier
	for i := range synth.Tiers {
		t := &synth.Tiers[i]
		switch {
		case t.Language == backendLang && backend == nil && (t.Language != target || strings.Contains(strings.ToLower(t.Name), "back")):
			backend = t
		case frontend == nil:
			frontend = t
		default:
			a.warn("extra tier %q from the synthesis was dropped", t.Name)
		}
	}
	if backend == nil {
		backend = &spec.Tier{Name: "backend", Intent: "Own the data and serve the API.", Language: backendLang}
		a.warn("the synthesis proposed no backend tier; a default one was added")
	}
	if frontend == nil {
		frontend = &spec.Tier{Name: "app", Intent: "The frontend.", Language: target}
		a.warn("the synthesis proposed no frontend tier; add its modules by hand")
	}
	backend.Name = Ident(firstNonEmpty(backend.Name, "backend"))
	frontend.Name = Ident(firstNonEmpty(frontend.Name, "app"))
	backend.Language, frontend.Language = backendLang, target
	backend.ModulePath = firstNonEmpty(backend.ModulePath, a.inv.ModulePath)
	frontend.ModulePath = firstNonEmpty(a.opts.ModulePath, frontend.ModulePath, "com.example."+s.System.Name)
	frontend.DependsOn = []string{backend.Name}
	backend.DependsOn = nil
	if backend.Stack == nil && !emptyStack(synth.Stack) {
		backend.Stack = spec.Stack{backendLang: synth.Stack}
	}

	if mirrorsBackend(a.inv, a.opts) {
		m := a.mirror(frags, synth.Interfaces, backend.Name)
		existing := map[string]bool{}
		for _, mod := range m.modules {
			existing[mod.Name] = true
		}
		for _, add := range sanitizeModules(backend.Modules) {
			if existing[add.Name] {
				a.warn("backend addition %q collides with an existing module; dropped", add.Name)
				continue
			}
			m.modules = append(m.modules, add)
		}
		backend.Modules = m.modules
		s.System.Database = m.database
		if s.System.Database == nil && synth.Database != nil && len(synth.Database.Entities) > 0 {
			synth.Database.Tier = backend.Name
			s.System.Database = synth.Database
		}
		// Surfaces the synthesis added to a mirrored interface, or whole
		// interfaces of other tiers.
		byName := map[string]*spec.Interface{}
		for i := range m.interfaces {
			byName[m.interfaces[i].Name] = &m.interfaces[i]
		}
		for _, si := range synth.Interfaces {
			si = sanitizeInterface(si)
			if si.Provider == "" {
				si.Provider = backend.Name
			}
			if cur, ok := byName[si.Name]; ok && cur.Provider == si.Provider {
				for _, sf := range si.Surfaces {
					if !surfaceExists(cur, sf.Name) {
						cur.Surfaces = append(cur.Surfaces, sf)
					}
				}
				continue
			}
			if _, ok := byName[si.Name]; ok {
				a.warn("interface %q is provided by both tiers in the synthesis; the frontend copy was renamed %s_%s", si.Name, frontend.Name, si.Name)
				si.Name = Ident(frontend.Name + "_" + si.Name)
			}
			m.interfaces = append(m.interfaces, si)
			byName[si.Name] = &m.interfaces[len(m.interfaces)-1]
		}
		s.System.Interfaces = m.interfaces
	} else {
		backend.Modules = sanitizeModules(backend.Modules)
		if synth.Database != nil && len(synth.Database.Entities) > 0 {
			synth.Database.Tier = backend.Name
			s.System.Database = synth.Database
		}
		for _, si := range synth.Interfaces {
			si = sanitizeInterface(si)
			if si.Provider == "" {
				si.Provider = backend.Name
			}
			s.System.Interfaces = append(s.System.Interfaces, si)
		}
	}
	frontend.Modules = sanitizeModules(frontend.Modules)
	if frontend.Database != nil && len(frontend.Database.Entities) == 0 {
		frontend.Database = nil
	}
	if frontend.Database != nil {
		frontend.Database.Tier = ""
	}
	if backend.Database != nil {
		backend.Database = nil // the backend's database is the system database
	}
	s.Tiers = []spec.Tier{*backend, *frontend}
}

func sanitizeInterface(iface spec.Interface) spec.Interface {
	iface.Name = Ident(iface.Name)
	if iface.Provider != "" && iface.Provider != spec.External {
		iface.Provider = Ident(iface.Provider)
	}
	for i := range iface.Surfaces {
		iface.Surfaces[i].Name = Ident(iface.Surfaces[i].Name)
	}
	return iface
}

func sanitizeModules(mods []spec.Module) []spec.Module {
	out := make([]spec.Module, 0, len(mods))
	for _, m := range mods {
		m.Name = Ident(m.Name)
		for i, d := range m.DependsOn {
			m.DependsOn[i] = Ident(d)
		}
		m.Surfaces = sanitizeRefs(m.Surfaces)
		m.Consumes = sanitizeRefs(m.Consumes)
		out = append(out, m)
	}
	return out
}

func sanitizeRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, sf := range refs {
		iface, surf, ok := strings.Cut(sf, ".")
		if ok {
			out = append(out, Ident(iface)+"."+Ident(surf))
		} else {
			out = append(out, Ident(iface))
		}
	}
	return out
}

func surfaceExists(iface *spec.Interface, name string) bool {
	for _, sf := range iface.Surfaces {
		if sf.Name == name {
			return true
		}
	}
	return false
}

func detectEngine(frameworks []string) string {
	for _, f := range frameworks {
		switch f {
		case "pgx", "pq":
			return "postgres"
		case "sqlite3":
			return "sqlite"
		}
	}
	return "postgres"
}

// goals maps synthesis goals to modules, dropping references to modules that
// do not exist and guaranteeing at least one owned goal.
func (a *assembler) goals(s *spec.Spec, synth agents.Synthesis) {
	mods := map[string]*spec.Module{}
	for _, m := range s.AllModules() {
		mods[m.Name] = m
	}
	seen := map[string]bool{}
	for i, g := range synth.Goals {
		id := strings.TrimSpace(g.ID)
		if id == "" || seen[id] {
			id = fmt.Sprintf("G%d", i+1)
		}
		seen[id] = true
		var owners []string
		for _, ref := range g.Modules {
			name := ModuleName(ref)
			if m, ok := mods[name]; ok {
				m.Goals = append(m.Goals, id)
				owners = append(owners, name)
				continue
			}
			if m, ok := mods[Ident(ref)]; ok {
				m.Goals = append(m.Goals, id)
				owners = append(owners, m.Name)
			}
		}
		if len(owners) == 0 {
			a.warn("goal %s (%q) names no known module; dropped", id, g.Statement)
			continue
		}
		verify := spec.VerifyMethod(g.Verify)
		if verify == "" {
			verify = spec.VerifyTest
		}
		s.System.Goals = append(s.System.Goals, spec.Goal{ID: id, Statement: strings.TrimSpace(g.Statement), Verify: verify})
	}
	if len(s.System.Goals) == 0 {
		a.warn("no goals recovered; a placeholder goal owned by every module was added")
		s.System.Goals = []spec.Goal{{ID: "G1", Statement: "The system preserves the behaviour of the source it was imported from.", Verify: spec.VerifyReview}}
		for _, m := range s.AllModules() {
			m.Goals = []string{"G1"}
		}
	}
}

// tidy fixes what would otherwise fail validation for mechanical reasons:
// duplicate scenario ids, empty field types, references to unknown entities
// or frameworks, and surfaces implemented from the wrong tier.
func (a *assembler) tidy(s *spec.Spec) {
	entities := map[string]bool{}
	for _, db := range s.Databases() {
		// Synthesized databases never went through the loader's defaults.
		if db.Migrations == "" {
			db.Migrations = "auto"
		}
		if db.Test.Engine == "" {
			db.Test.Engine = "sqlite"
		}
		local := map[string]bool{}
		for i := range db.Entities {
			e := &db.Entities[i]
			entities[e.Name] = true
			local[e.Name] = true
			for j := range e.Fields {
				f := &e.Fields[j]
				if f.Type == "" {
					f.Type = "string"
					a.warn("entity %s field %s had no type; defaulted to string", e.Name, f.Name)
				}
				switch strings.ToLower(strings.TrimSpace(f.Key)) {
				case "", "primary":
				case "true", "yes", "pk", "primary_key", "primary key":
					f.Key = "primary"
				default:
					// The format has only single-field primary keys; a
					// composite or descriptive key cannot be expressed.
					a.warn("entity %s field %s had key %q; cleared (only \"primary\" is valid)", e.Name, f.Name, f.Key)
					f.Key = ""
				}
			}
		}
		for i := range db.Entities {
			e := &db.Entities[i]
			kept := e.Relations[:0]
			for _, r := range e.Relations {
				if local[r.Entity] {
					kept = append(kept, r)
				} else {
					a.warn("entity %s relation to unknown entity %s dropped", e.Name, r.Entity)
				}
			}
			e.Relations = kept
		}
	}
	providerSurfaces := map[string]string{}
	for i := range s.System.Interfaces {
		iface := &s.System.Interfaces[i]
		// The Synthesizer sometimes describes how an interface is served
		// ("chi + qor5 presets") where a tier name belongs. In a single-tier
		// spec the only valid providers are the tier itself ("") and
		// external; anything else would move every surface to consumes.
		if len(s.Tiers) == 0 && iface.Provider != "" && iface.Provider != spec.External {
			a.warn("interface %s had provider %q in a single-tier spec; cleared", iface.Name, iface.Provider)
			iface.Provider = ""
		}
		frameworks := map[string]bool{}
		if t := s.Tier(iface.Provider); t != nil && iface.Provider != spec.External {
			if ls := t.LanguageStack(); ls != nil {
				for _, f := range ls.Frameworks {
					frameworks[f.Name] = true
				}
			}
		}
		if iface.Framework != "" && !frameworks[iface.Framework] {
			a.warn("interface %s names framework %q not in the providing tier's stack; cleared", iface.Name, iface.Framework)
			iface.Framework = ""
		}
		for j := range iface.Surfaces {
			sf := &iface.Surfaces[j]
			providerSurfaces[iface.Name+"."+sf.Name] = iface.Provider
			if sf.Entity != "" && !entities[sf.Entity] {
				a.warn("surface %s.%s names unknown entity %q; cleared", iface.Name, sf.Name, sf.Entity)
				sf.Entity = ""
			}
			if iface.Kind == "http" && sf.Method == "" {
				sf.Method = "GET"
			}
		}
	}
	for _, t := range s.EffectiveTiers() {
		for i := range t.Modules {
			m := &t.Modules[i]
			ids := map[string]bool{}
			for j := range m.Scenarios {
				sc := &m.Scenarios[j]
				if sc.ID == "" || ids[sc.ID] {
					sc.ID = fmt.Sprintf("S%d", j+1)
				}
				ids[sc.ID] = true
				if sc.When == "" {
					sc.When = "(unspecified)"
				}
				if sc.Then == "" {
					sc.Then = "(unspecified)"
				}
			}
			for j := range m.Interface {
				if m.Interface[j].Signature == "" {
					m.Interface[j].Signature = m.Interface[j].Name + "()"
				}
			}
			// A module cannot implement what another tier or an external
			// service provides; it consumes it.
			var keep []string
			for _, ref := range m.Surfaces {
				resolved, _ := s.ResolveSurfaces([]string{ref})
				moved := false
				for _, r := range resolved {
					if r.System != "" {
						continue
					}
					if p := providerSurfaces[r.ID()]; p != t.Name {
						m.Consumes = append(m.Consumes, r.ID())
						moved = true
					}
				}
				if moved {
					a.warn("module %s listed %s under surfaces but does not provide it; moved to consumes", m.Name, ref)
				} else {
					keep = append(keep, ref)
				}
			}
			m.Surfaces = keep
			m.Consumes = dedupe(m.Consumes)
			m.Goals = dedupe(m.Goals)
		}
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
