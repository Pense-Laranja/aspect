package spec

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Severity of a validation issue.
type Severity string

const (
	// Error issues make the spec unusable; the pipeline refuses to run.
	Error Severity = "error"
	// Warning issues are reported but do not block the pipeline.
	Warning Severity = "warning"
)

// Issue is one validation finding, addressed by a dotted path into the spec.
type Issue struct {
	Severity Severity
	Path     string
	Message  string
	// Location is the file and line Path was written at
	// (`modules/orders.yaml:12`), when the spec was loaded from disk and
	// the path names a node.
	Location string
}

func (i Issue) String() string {
	if i.Location != "" {
		return fmt.Sprintf("%s: %s: %s: %s", i.Location, i.Severity, i.Path, i.Message)
	}
	return fmt.Sprintf("%s: %s: %s", i.Severity, i.Path, i.Message)
}

// Issues is a list of findings with helpers for the CLI and the pipeline.
type Issues []Issue

// HasErrors reports whether any issue is blocking.
func (is Issues) HasErrors() bool {
	for _, i := range is {
		if i.Severity == Error {
			return true
		}
	}
	return false
}

var (
	identRe       = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	entityRe      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	invariantIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

var invariantCategories = []string{
	"correctness", "performance", "quality", "readability",
	"security", "data", "contract", "i18n", "architecture",
}

var invariantEnforcements = []string{"test", "ast", "review", "invariant"}

// SupportedLanguages lists the languages the pipeline can build and test.
var SupportedLanguages = []string{"go", "swift"}

// External is the provider name for interfaces and databases served by a
// hosted service outside the spec.
const External = "external"

var (
	dbEngines      = []string{"postgres", "mysql", "sqlite"}
	migrationModes = []string{"auto", "files"}
	relationKinds  = []string{"has_one", "has_many", "belongs_to", "many_to_many"}
	interfaceKinds = []string{"http", "web", "cli", "grpc", "app", "job", "mcp"}
	// fieldTypes are the neutral entity field types; a field may also name
	// an entity. Agents map them to each language and database engine.
	fieldTypes = []string{"string", "int", "int64", "float", "bool", "time", "decimal", "uuid", "bytes", "json"}
	topologies = []Topology{Monolith, APIBackend, CloudService}
)

type collector struct {
	issues Issues
}

func (c *collector) add(sev Severity, path, format string, args ...any) {
	c.issues = append(c.issues, Issue{Severity: sev, Path: path, Message: fmt.Sprintf(format, args...)})
}

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// ctx carries the resolved shape of the spec through validation.
type ctx struct {
	s        *Spec
	tiers    []*Tier
	multi    bool
	tierName map[string]bool
	goals    map[string]bool
	// entityDB maps entity name to the owning database's tier ("" for the
	// implicit tier, External for a hosted database).
	entityDB map[string]string
	// surfaceProvider maps "iface.surface" to the providing tier.
	surfaceProvider map[string]string
	// tierPath maps a tier to its yaml path prefix for modules.
	modulePath func(t *Tier, i int) string
}

// Validate checks the semantic rules of a spec: identifiers, references,
// dependency cycles, goal coverage, and ownership of entities and surfaces
// across tiers. It returns every issue found rather than stopping at the
// first one, so a user can fix a spec in one pass.
func Validate(s *Spec) Issues {
	c := &collector{}
	x := &ctx{s: s, tiers: s.EffectiveTiers(), multi: len(s.Tiers) > 0, tierName: map[string]bool{}, entityDB: map[string]string{}, surfaceProvider: map[string]string{}}
	x.modulePath = func(t *Tier, i int) string {
		if !x.multi {
			return fmt.Sprintf("modules[%d]", i)
		}
		for j := range s.Tiers {
			if &s.Tiers[j] == t {
				return fmt.Sprintf("tiers[%d].modules[%d]", j, i)
			}
		}
		return fmt.Sprintf("tiers[?].modules[%d]", i)
	}
	x.goals = validateSystem(c, x)
	validateTiers(c, x)
	validateDatabases(c, x)
	validateInterfaces(c, x)
	validateModules(c, x)
	validateModuleNesting(c, x)
	validateTopology(c, x)
	validateBriefs(c, s)
	validateDependencies(c, s)
	for i := range c.issues {
		c.issues[i].Location = s.Sources.Locate(c.issues[i].Path)
	}
	return c.issues
}

func validateDependencies(c *collector, s *Spec) {
	seen := map[string]bool{}
	for i, d := range s.System.Dependencies {
		p := fmt.Sprintf("system.dependencies[%d]", i)
		if d.Name == "" {
			c.add(Error, p+".name", "is required")
		} else if !identRe.MatchString(d.Name) {
			c.add(Error, p+".name", "%q must match %s", d.Name, identRe)
		} else if d.Name == External {
			c.add(Error, p+".name", "%q is reserved", External)
		} else if seen[d.Name] {
			c.add(Error, p+".name", "duplicate dependency %q", d.Name)
		} else if s.Tier(d.Name) != nil && d.Name != "" {
			c.add(Error, p+".name", "%q is also a tier name", d.Name)
		}
		seen[d.Name] = true
		if d.Spec == "" {
			c.add(Error, p+".spec", "is required (path of the dependency's spec)")
		} else if filepath.IsAbs(d.Spec) {
			c.add(Error, p+".spec", "%q must be a relative path", d.Spec)
		}
		if _, ok := s.Deps[d.Name]; !ok && d.Spec != "" {
			c.add(Warning, p, "dependency %q was not loaded (parsed from memory); references into it cannot be checked", d.Name)
		}
	}
}

func validateBriefs(c *collector, s *Spec) {
	for owner, b := range s.Briefs() {
		if b.Path == "" {
			continue
		}
		if err := s.checkBriefPath(b.Path); err != nil {
			c.add(Error, owner+".brief", "%v", err)
			continue
		}
		if strings.TrimSpace(b.Text) == "" {
			c.add(Warning, owner+".brief", "%q was not loaded (parsed from memory, or the file is empty); agents will not see it", b.Path)
		}
	}
}

func validateSystem(c *collector, x *ctx) map[string]bool {
	s := x.s
	if s.Aspect != Version {
		c.add(Error, "aspect", "unsupported spec version %d (this build understands %d)", s.Aspect, Version)
	}
	if s.System.Name == "" {
		c.add(Error, "system.name", "is required")
	} else if !identRe.MatchString(s.System.Name) {
		c.add(Error, "system.name", "%q must match %s", s.System.Name, identRe)
	}
	if strings.TrimSpace(s.System.Intent) == "" {
		c.add(Error, "system.intent", "is required: agents cannot judge a system without a stated intent")
	}
	if s.System.Topology != "" {
		ok := false
		for _, t := range topologies {
			if s.System.Topology == t {
				ok = true
			}
		}
		if !ok {
			c.add(Error, "system.topology", "%q is not one of monolith, api_backend, cloud_service", s.System.Topology)
		}
	}
	if x.multi {
		if len(s.Modules) > 0 {
			c.add(Error, "modules", "top-level modules and tiers are exclusive; move the modules into a tier")
		}
		if s.System.ModulePath != "" || len(s.System.Stack) > 0 {
			c.add(Warning, "system", "language, module_path and stack are ignored when tiers are declared; set them per tier")
		}
	} else {
		if !oneOf(s.System.Language, SupportedLanguages) {
			c.add(Error, "system.language", "%q is not supported (one of %s)", s.System.Language, strings.Join(SupportedLanguages, ", "))
		}
		if s.System.ModulePath == "" {
			c.add(Error, "system.module_path", "is required (the module or package path of the generated code)")
		}
		validateStack(c, "system.stack", s.System.Language, s.System.Stack)
	}
	if len(s.System.Goals) == 0 {
		c.add(Error, "system.goals", "at least one goal is required: without goals there is nothing to validate against")
	}
	goals := map[string]bool{}
	for i, g := range s.System.Goals {
		p := fmt.Sprintf("system.goals[%d]", i)
		if g.ID == "" {
			c.add(Error, p+".id", "is required")
		} else if goals[g.ID] {
			c.add(Error, p+".id", "duplicate goal id %q", g.ID)
		}
		goals[g.ID] = true
		if strings.TrimSpace(g.Statement) == "" {
			c.add(Error, p+".statement", "is required")
		}
		switch g.Verify {
		case VerifyTest, VerifyInvariant, VerifyReview:
		default:
			c.add(Error, p+".verify", "%q is not one of test, invariant, review", g.Verify)
		}
	}
	return goals
}

func validateTiers(c *collector, x *ctx) {
	if !x.multi {
		x.tierName[""] = true
		return
	}
	s := x.s
	if len(s.Tiers) == 1 {
		c.add(Warning, "tiers", "a single tier can be written as a plain spec (language, module_path and modules at the top)")
	}
	for i, t := range s.Tiers {
		p := fmt.Sprintf("tiers[%d]", i)
		if t.Name == "" {
			c.add(Error, p+".name", "is required")
		} else if !identRe.MatchString(t.Name) {
			c.add(Error, p+".name", "%q must match %s", t.Name, identRe)
		} else if t.Name == External {
			c.add(Error, p+".name", "%q is reserved for hosted services", External)
		} else if x.tierName[t.Name] {
			c.add(Error, p+".name", "duplicate tier %q", t.Name)
		}
		x.tierName[t.Name] = true
		if strings.TrimSpace(t.Intent) == "" {
			c.add(Error, p+".intent", "is required")
		}
		if !oneOf(t.Language, SupportedLanguages) {
			c.add(Error, p+".language", "%q is not supported (one of %s)", t.Language, strings.Join(SupportedLanguages, ", "))
		}
		if t.ModulePath == "" {
			c.add(Error, p+".module_path", "is required")
		}
		if len(t.Modules) == 0 {
			c.add(Error, p+".modules", "tier %q has no modules", t.Name)
		}
		validateStack(c, p+".stack", t.Language, t.Stack)
	}
	for i, t := range s.Tiers {
		for j, d := range t.DependsOn {
			p := fmt.Sprintf("tiers[%d].depends_on[%d]", i, j)
			if d == t.Name {
				c.add(Error, p, "tier %q depends on itself", t.Name)
			} else if !x.tierName[d] {
				c.add(Error, p, "unknown tier %q", d)
			}
		}
	}
	names := make([]string, 0, len(s.Tiers))
	deps := map[string][]string{}
	for _, t := range s.Tiers {
		names = append(names, t.Name)
		deps[t.Name] = t.DependsOn
	}
	if cycle := findCycle(names, deps); cycle != nil {
		c.add(Error, "tiers", "dependency cycle: %s", strings.Join(cycle, " -> "))
	}
}

func validateStack(c *collector, path, language string, stack Stack) {
	langs := make([]string, 0, len(stack))
	for l := range stack {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	for _, lang := range langs {
		ls := stack[lang]
		p := path + "." + lang
		if lang != language {
			c.add(Warning, p, "configured for %q but the language is %q; it will be ignored", lang, language)
		}
		names := map[string]bool{}
		for i, f := range ls.Frameworks {
			fp := fmt.Sprintf("%s.frameworks[%d]", p, i)
			if f.Name == "" {
				c.add(Error, fp+".name", "is required")
			} else if names[f.Name] {
				c.add(Error, fp+".name", "duplicate framework %q", f.Name)
			}
			names[f.Name] = true
			if f.Module == "" {
				c.add(Error, fp+".module", "is required (the import path agents must use for %q)", f.Name)
			}
		}
		if ls.ORM != nil && ls.ORM.Module == "" {
			c.add(Error, p+".orm.module", "is required")
		}
		for i, m := range ls.AllowedModules {
			if strings.TrimSpace(m) == "" {
				c.add(Error, fmt.Sprintf("%s.allowed_modules[%d]", p, i), "is empty")
			}
		}
	}
}

func validateDatabases(c *collector, x *ctx) {
	s := x.s
	if db := s.System.Database; db != nil {
		owner := db.Tier
		switch {
		case !x.multi && owner != "" && owner != External:
			c.add(Error, "system.database.tier", "%q names a tier but the spec has none", owner)
		case x.multi && owner == "":
			c.add(Error, "system.database.tier", "is required in a multi-tier spec: name the tier that owns the database, or %q", External)
		case x.multi && owner != External && !x.tierName[owner]:
			c.add(Error, "system.database.tier", "unknown tier %q", owner)
		}
		validateDatabase(c, x, "system.database", db, owner)
	}
	for i, t := range s.Tiers {
		if t.Database == nil {
			continue
		}
		p := fmt.Sprintf("tiers[%d].database", i)
		if t.Database.Tier != "" {
			c.add(Warning, p+".tier", "is implied for a tier-local database")
		}
		validateDatabase(c, x, p, t.Database, t.Name)
	}
}

func validateDatabase(c *collector, x *ctx, p string, db *Database, owner string) {
	if !oneOf(db.Engine, dbEngines) {
		c.add(Error, p+".engine", "%q is not one of %s", db.Engine, strings.Join(dbEngines, ", "))
	}
	if !oneOf(db.Migrations, migrationModes) {
		c.add(Error, p+".migrations", "%q is not one of %s", db.Migrations, strings.Join(migrationModes, ", "))
	}
	if !oneOf(db.Test.Engine, dbEngines) {
		c.add(Error, p+".test.engine", "%q is not one of %s", db.Test.Engine, strings.Join(dbEngines, ", "))
	}
	if db.Test.Engine != "sqlite" && db.Test.DSNEnv == "" && owner != External {
		c.add(Warning, p+".test", "tests run against %s but no dsn_env is set; generated tests will need a running database", db.Test.Engine)
	}
	if len(db.Entities) == 0 {
		c.add(Error, p+".entities", "a database with no entities declares nothing to persist")
	}
	local := map[string]bool{}
	for i, e := range db.Entities {
		ep := fmt.Sprintf("%s.entities[%d]", p, i)
		if e.Name == "" {
			c.add(Error, ep+".name", "is required")
		} else if !entityRe.MatchString(e.Name) {
			c.add(Error, ep+".name", "%q must match %s", e.Name, entityRe)
		} else if _, dup := x.entityDB[e.Name]; dup {
			c.add(Error, ep+".name", "duplicate entity %q (entity names are unique across all databases)", e.Name)
		}
		x.entityDB[e.Name] = owner
		local[e.Name] = true
		if strings.TrimSpace(e.Intent) == "" {
			c.add(Warning, ep+".intent", "entity %q has no intent; the Validator cannot judge whether it models the right thing", e.Name)
		}
		if len(e.Fields) == 0 {
			c.add(Error, ep+".fields", "entity %q has no fields", e.Name)
		}
		fields := map[string]bool{}
		primaries := 0
		for j, f := range e.Fields {
			fp := fmt.Sprintf("%s.fields[%d]", ep, j)
			if f.Name == "" {
				c.add(Error, fp+".name", "is required")
			} else if fields[f.Name] {
				c.add(Error, fp+".name", "duplicate field %q in entity %q", f.Name, e.Name)
			}
			fields[f.Name] = true
			if f.Type == "" {
				c.add(Error, fp+".type", "is required")
			} else if !oneOf(f.Type, fieldTypes) && x.s.Entity(f.Type) == nil {
				c.add(Warning, fp+".type", "%q is neither a neutral type (%s) nor an entity; another language or database cannot map it (lists and structured values are json)", f.Type, strings.Join(fieldTypes, ", "))
			}
			switch f.Key {
			case "":
			case "primary":
				primaries++
			default:
				c.add(Error, fp+".key", "%q is not \"primary\"", f.Key)
			}
		}
		if primaries == 0 {
			c.add(Warning, ep, "entity %q has no primary key field; agents will add a surrogate id", e.Name)
		}
	}
	for i, e := range db.Entities {
		for j, r := range e.Relations {
			rp := fmt.Sprintf("%s.entities[%d].relations[%d]", p, i, j)
			if !oneOf(r.Kind, relationKinds) {
				c.add(Error, rp+".kind", "%q is not one of %s", r.Kind, strings.Join(relationKinds, ", "))
			}
			if !local[r.Entity] {
				c.add(Error, rp+".entity", "unknown entity %q in this database", r.Entity)
			}
		}
	}
}

func validateInterfaces(c *collector, x *ctx) {
	s := x.s
	names := map[string]bool{}
	for i, iface := range s.System.Interfaces {
		p := fmt.Sprintf("system.interfaces[%d]", i)
		if iface.Name == "" {
			c.add(Error, p+".name", "is required")
		} else if !identRe.MatchString(iface.Name) {
			c.add(Error, p+".name", "%q must match %s", iface.Name, identRe)
		} else if names[iface.Name] {
			c.add(Error, p+".name", "duplicate interface %q", iface.Name)
		}
		names[iface.Name] = true
		if !oneOf(iface.Kind, interfaceKinds) {
			c.add(Error, p+".kind", "%q is not one of %s", iface.Kind, strings.Join(interfaceKinds, ", "))
		}
		if strings.TrimSpace(iface.Intent) == "" {
			c.add(Error, p+".intent", "is required")
		}
		provider := iface.Provider
		switch {
		case provider == External:
			if iface.Service == "" {
				c.add(Warning, p+".service", "external interface %q does not name its service", iface.Name)
			}
		case !x.multi && provider != "":
			c.add(Error, p+".provider", "%q names a tier but the spec has none (use %q for a hosted service)", provider, External)
		case x.multi && provider == "":
			c.add(Error, p+".provider", "is required in a multi-tier spec: the tier that serves %q, or %q", iface.Name, External)
		case x.multi && !x.tierName[provider]:
			c.add(Error, p+".provider", "unknown tier %q", provider)
		}
		if iface.Framework != "" {
			var stack *LanguageStack
			if t := s.Tier(provider); t != nil && provider != External {
				stack = t.LanguageStack()
			}
			found := false
			if stack != nil {
				for _, f := range stack.Frameworks {
					if f.Name == iface.Framework {
						found = true
					}
				}
			}
			if !found {
				c.add(Error, p+".framework", "%q is not declared in the providing tier's stack", iface.Framework)
			}
		}
		if len(iface.Surfaces) == 0 {
			c.add(Error, p+".surfaces", "interface %q exposes nothing", iface.Name)
		}
		seen := map[string]bool{}
		for j, sf := range iface.Surfaces {
			sp := fmt.Sprintf("%s.surfaces[%d]", p, j)
			if sf.Name == "" {
				c.add(Error, sp+".name", "is required")
			} else if !identRe.MatchString(sf.Name) {
				c.add(Error, sp+".name", "%q must match %s", sf.Name, identRe)
			} else if seen[sf.Name] {
				c.add(Error, sp+".name", "duplicate surface %q in interface %q", sf.Name, iface.Name)
			}
			seen[sf.Name] = true
			x.surfaceProvider[iface.Name+"."+sf.Name] = provider
			switch iface.Kind {
			case "http":
				if sf.Route == "" || sf.Method == "" {
					c.add(Error, sp, "http surfaces need route and method")
				}
			case "web", "grpc", "app", "job", "mcp":
				if sf.Route == "" {
					c.add(Error, sp+".route", "is required for %s surfaces", iface.Kind)
				}
			}
			if (iface.Kind == "http" || iface.Kind == "web") && sf.Route != "" && !strings.HasPrefix(sf.Route, "/") {
				c.add(Warning, sp+".route", "%q is not a path: a page event is an `event` with its page as route, a background job belongs in a job interface, an AI tool in an mcp interface, middleware takes the pattern it wraps (/*), and a function called in-process is a module operation, not a surface", sf.Route)
			}
			if sf.Event != "" && iface.Kind != "web" {
				c.add(Error, sp+".event", "events are dispatched by pages; only web surfaces have one")
			}
			if sf.Entity != "" && s.Entity(sf.Entity) == nil {
				c.add(Error, sp+".entity", "unknown entity %q", sf.Entity)
			}
			if len(sf.Operations) > 0 && sf.Entity == "" {
				c.add(Warning, sp+".operations", "operations are listed but no entity is named")
			}
			for k, op := range sf.Operations {
				if !identRe.MatchString(op) {
					c.add(Warning, fmt.Sprintf("%s.operations[%d]", sp, k), "%q is not a verb: use list, create, read, update, delete or a lowercase custom verb such as close, not the function that implements it", op)
				}
			}
		}
	}
}

// validateModulePath checks a module's code directory: a clean relative
// slash path inside the tier, used by one module only. Only the Go profile
// places modules by path; the others derive the layout from the name.
func validateModulePath(c *collector, p string, t *Tier, m *Module, seen map[string]string) {
	switch {
	case t.Language != "go":
		c.add(Error, p, "module %q: path is supported only in Go tiers (this one is %s)", m.Name, t.Language)
	case strings.Contains(m.Path, "\\") || path.IsAbs(m.Path) || path.Clean(m.Path) != m.Path:
		c.add(Error, p, "module %q: path %q must be a clean relative slash path (\".\" for the root)", m.Name, m.Path)
	case m.Path == ".." || strings.HasPrefix(m.Path, "../"):
		c.add(Error, p, "module %q: path %q leaves the code root", m.Name, m.Path)
	default:
		if other, dup := seen[m.Path]; dup {
			c.add(Error, p, "module %q: path %q is already module %q's", m.Name, m.Path, other)
		}
		seen[m.Path] = m.Name
	}
}

// validateModuleNesting refuses a module placed by path inside the
// directory of a Go module laid out by name: the latter owns everything
// below <name>/, so the two would claim the same files, and its Coder could
// write into the placed module's package.
func validateModuleNesting(c *collector, x *ctx) {
	for _, t := range x.tiers {
		if t.Language != "go" {
			continue
		}
		for i := range t.Modules {
			m := &t.Modules[i]
			if !m.Placed() {
				continue
			}
			for _, o := range t.Modules {
				if o.Placed() || o.Name == "" {
					continue
				}
				if m.Path == o.Name || strings.HasPrefix(m.Path, o.Name+"/") {
					c.add(Error, x.modulePath(t, i)+".path", "module %q: path %q lies inside %s/, which module %q owns entirely; give %q a path too", m.Name, m.Path, o.Name, o.Name, o.Name)
				}
			}
		}
	}
}

func validateModules(c *collector, x *ctx) {
	s := x.s
	if !x.multi && len(s.Modules) == 0 {
		c.add(Error, "modules", "at least one module is required")
	}
	mods := map[string]*Tier{}
	covered := map[string]bool{}
	entityOwner := map[string][]string{}
	surfaceOwner := map[string][]string{}
	surfaceConsumers := map[string][]string{}

	for _, t := range x.tiers {
		paths := map[string]string{}
		for i := range t.Modules {
			m := &t.Modules[i]
			p := x.modulePath(t, i)
			if m.Path != "" {
				validateModulePath(c, p+".path", t, m, paths)
			}
			if m.Name == "" {
				c.add(Error, p+".name", "is required")
			} else if !identRe.MatchString(m.Name) {
				c.add(Error, p+".name", "%q must match %s", m.Name, identRe)
			} else if _, dup := mods[m.Name]; dup {
				c.add(Error, p+".name", "duplicate module name %q (module names are unique across tiers)", m.Name)
			}
			mods[m.Name] = t
			if strings.TrimSpace(m.Intent) == "" {
				c.add(Error, p+".intent", "is required")
			}
			if len(m.Goals) == 0 {
				c.add(Warning, p+".goals", "module %q owns no goal; the Validator will only check its intent", m.Name)
			}
			for j, g := range m.Goals {
				if !x.goals[g] {
					c.add(Error, fmt.Sprintf("%s.goals[%d]", p, j), "unknown goal %q", g)
				}
				covered[g] = true
			}
			if len(m.Scenarios) == 0 && len(m.Invariants) == 0 && !topicsHaveInvariants(m) {
				c.add(Warning, p, "module %q has no scenarios or invariants; tests will be inferred from the interface only", m.Name)
			}
			seenInvariantID := map[string]bool{}
			validateInvariants(c, p, m.Name, m.Invariants, seenInvariantID)
			validateTopics(c, p, m, seenInvariantID)
			seenScenario := map[string]bool{}
			for j, sc := range m.Scenarios {
				sp := fmt.Sprintf("%s.scenarios[%d]", p, j)
				if sc.ID == "" {
					c.add(Error, sp+".id", "is required")
				} else if seenScenario[sc.ID] {
					c.add(Error, sp+".id", "duplicate scenario id %q in module %q", sc.ID, m.Name)
				}
				seenScenario[sc.ID] = true
				if sc.When == "" || sc.Then == "" {
					c.add(Error, sp, "scenario needs at least `when` and `then`")
				}
				for k, g := range sc.Goals {
					if !x.goals[g] {
						c.add(Error, fmt.Sprintf("%s.goals[%d]", sp, k), "unknown goal %q", g)
					}
					covered[g] = true
				}
				for k, invRef := range sc.Invariants {
					if !seenInvariantID[invRef] {
						c.add(Warning, fmt.Sprintf("%s.invariants[%d]", sp, k), "references unknown invariant id %q in module %q", invRef, m.Name)
					}
				}
			}
			for j, op := range m.Interface {
				op_ := fmt.Sprintf("%s.interface[%d]", p, j)
				if op.Name == "" {
					c.add(Error, op_+".name", "is required")
				}
				if op.Signature == "" {
					c.add(Error, op_+".signature", "is required so dependents can be generated against a stable contract")
				}
			}
			for j, e := range m.Entities {
				owner, ok := x.entityDB[e]
				if !ok {
					c.add(Error, fmt.Sprintf("%s.entities[%d]", p, j), "unknown entity %q", e)
					continue
				}
				if owner == External {
					c.add(Error, fmt.Sprintf("%s.entities[%d]", p, j), "entity %q lives in an external database; no module can own it", e)
					continue
				}
				if owner != t.Name {
					c.add(Error, fmt.Sprintf("%s.entities[%d]", p, j), "entity %q belongs to the database of tier %q, not %q", e, owner, t.Name)
					continue
				}
				entityOwner[e] = append(entityOwner[e], m.Name)
			}
			refs, errs := s.ResolveSurfaces(m.Surfaces)
			for _, err := range errs {
				c.add(Error, p+".surfaces", "%v", err)
			}
			for _, r := range refs {
				id := r.ID()
				if r.System != "" {
					c.add(Error, p+".surfaces", "surface %q belongs to system %q; consume it instead of implementing it", id, r.System)
					continue
				}
				switch provider := x.surfaceProvider[id]; {
				case provider == External:
					c.add(Error, p+".surfaces", "surface %q is served by an external service; consume it instead of implementing it", id)
				case provider != t.Name:
					c.add(Error, p+".surfaces", "surface %q is provided by tier %q; only that tier's modules may implement it", id, provider)
				default:
					surfaceOwner[id] = append(surfaceOwner[id], m.Name)
				}
			}
			refs, errs = s.ResolveSurfaces(m.Consumes)
			for _, err := range errs {
				c.add(Error, p+".consumes", "%v", err)
			}
			for _, r := range refs {
				id := r.ID()
				if r.System != "" {
					continue // served by another system; nothing to cross-check here
				}
				if provider := x.surfaceProvider[id]; provider != External && provider == t.Name {
					c.add(Error, p+".consumes", "surface %q is provided by this tier; depend on the implementing module instead of consuming", id)
					continue
				}
				surfaceConsumers[id] = append(surfaceConsumers[id], m.Name)
			}
		}
	}

	// Module dependencies stay inside a tier; cross-tier calls go through
	// interfaces.
	for _, t := range x.tiers {
		names := make([]string, 0, len(t.Modules))
		deps := map[string][]string{}
		for i := range t.Modules {
			m := &t.Modules[i]
			names = append(names, m.Name)
			for j, d := range m.DependsOn {
				p := fmt.Sprintf("%s.depends_on[%d]", x.modulePath(t, i), j)
				switch dt, ok := mods[d]; {
				case d == m.Name:
					c.add(Error, p, "module %q depends on itself", m.Name)
				case !ok:
					c.add(Error, p, "unknown module %q", d)
				case dt != t:
					c.add(Error, p, "module %q is in tier %q; cross-tier dependencies go through interfaces (consumes)", d, dt.Name)
				default:
					deps[m.Name] = append(deps[m.Name], d)
				}
			}
		}
		if cycle := findCycle(names, deps); cycle != nil {
			c.add(Error, "modules", "dependency cycle: %s", strings.Join(cycle, " -> "))
		}
	}

	// Goal coverage: a goal nobody owns can never be validated.
	for _, id := range sortedKeys(x.goals) {
		if id != "" && !covered[id] {
			c.add(Error, "system.goals", "goal %q is not owned by any module or scenario", id)
		}
	}
	// Entity ownership: exactly one module writes each entity of a
	// generated database.
	for _, e := range sortedKeys(mapKeys(x.entityDB)) {
		if x.entityDB[e] == External {
			continue
		}
		switch owners := entityOwner[e]; len(owners) {
		case 0:
			c.add(Error, "database.entities", "entity %q is not owned by any module", e)
		case 1:
		default:
			c.add(Error, "modules", "entity %q is owned by several modules (%s); pick one owner", e, strings.Join(owners, ", "))
		}
	}
	// Surface ownership: every provided surface is implemented once; every
	// external surface is consumed by someone, or it is dead weight.
	for _, sf := range sortedKeys(mapKeys(x.surfaceProvider)) {
		if x.surfaceProvider[sf] == External {
			if len(surfaceConsumers[sf]) == 0 {
				c.add(Warning, "system.interfaces", "external surface %q is consumed by no module", sf)
			}
			continue
		}
		switch owners := surfaceOwner[sf]; len(owners) {
		case 0:
			c.add(Error, "system.interfaces", "surface %q is not implemented by any module", sf)
		case 1:
		default:
			c.add(Error, "modules", "surface %q is implemented by several modules (%s)", sf, strings.Join(owners, ", "))
		}
	}
}

// validateTopology checks the declared topology against the spec's shape.
func validateTopology(c *collector, x *ctx) {
	s := x.s
	declared := s.System.Topology
	if declared == "" {
		return
	}
	external := 0
	for _, i := range s.System.Interfaces {
		if i.Provider == External {
			external++
		}
	}
	dbExternal := s.System.Database != nil && s.System.Database.Tier == External
	switch declared {
	case Monolith:
		if x.multi && len(s.Tiers) > 1 {
			c.add(Warning, "system.topology", "monolith declared but %d tiers exist", len(s.Tiers))
		}
		if external > 0 || dbExternal {
			c.add(Warning, "system.topology", "monolith declared but the spec consumes external services")
		}
	case APIBackend:
		if len(s.Tiers) < 2 {
			c.add(Error, "system.topology", "api_backend needs at least two tiers (a backend serving an API and a frontend consuming it)")
		} else if !hasCrossTierConsumer(s) {
			c.add(Error, "system.topology", "api_backend declared but no module consumes a surface provided by another tier")
		}
	case CloudService:
		if external == 0 && !dbExternal {
			c.add(Error, "system.topology", "cloud_service declared but no interface or database is provided by %q", External)
		}
	}
}

func hasCrossTierConsumer(s *Spec) bool {
	provider := map[string]string{}
	for _, i := range s.System.Interfaces {
		for _, sf := range i.Surfaces {
			provider[i.Name+"."+sf.Name] = i.Provider
		}
	}
	for _, t := range s.EffectiveTiers() {
		for _, m := range t.Modules {
			refs, _ := s.ResolveSurfaces(m.Consumes)
			for _, r := range refs {
				if p := provider[r.ID()]; p != "" && p != External && p != t.Name {
					return true
				}
			}
		}
	}
	return false
}

func mapKeys(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// findCycle returns one dependency cycle among names as a closed path, or nil.
func findCycle(names []string, deps map[string][]string) []string {
	const (
		white = iota
		gray
		black
	)
	color := map[string]int{}
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	var stack []string
	var cycle []string

	var visit func(name string) bool
	visit = func(name string) bool {
		color[name] = gray
		stack = append(stack, name)
		for _, d := range deps[name] {
			if !known[d] {
				continue
			}
			switch color[d] {
			case gray:
				for i, n := range stack {
					if n == d {
						cycle = append(append([]string{}, stack[i:]...), d)
						return true
					}
				}
			case white:
				if visit(d) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[name] = black
		return false
	}
	sorted := append([]string{}, names...)
	sort.Strings(sorted)
	for _, n := range sorted {
		if color[n] == white && visit(n) {
			return cycle
		}
	}
	return nil
}

// validateInvariants checks a list of invariants written at p (a module's or
// one of its topics'). seen holds the ids already used in the module: ids
// are unique across a module and its topics, since a scenario names one by
// id alone.
func validateInvariants(c *collector, p, module string, invs []Invariant, seen map[string]bool) {
	for j, inv := range invs {
		invPath := fmt.Sprintf("%s.invariants[%d]", p, j)
		stmt := strings.TrimSpace(inv.Statement)
		if stmt == "" || stmt == "{}" {
			c.add(Error, invPath, "invariant statement cannot be empty or '{}'")
		}
		if inv.ID != "" {
			if !invariantIDRe.MatchString(inv.ID) {
				c.add(Error, invPath+".id", "invalid invariant id %q; must contain only letters, digits, '_' or '-'", inv.ID)
			}
			if seen[inv.ID] {
				c.add(Error, invPath+".id", "duplicate invariant id %q in module %q", inv.ID, module)
			}
			seen[inv.ID] = true
		}
		if inv.Category != "" && !oneOf(inv.Category, invariantCategories) {
			c.add(Error, invPath+".category", "unknown category %q; must be one of: %s", inv.Category, strings.Join(invariantCategories, ", "))
		}
		if inv.Enforcement != "" && !oneOf(inv.Enforcement, invariantEnforcements) {
			c.add(Error, invPath+".enforcement", "unknown enforcement %q; must be test, ast, review, or invariant", inv.Enforcement)
		}
	}
}

// validateTopics checks a module's topics, written at p: each has a name
// unique in the module, at least one file pattern path.Match can parse, and
// something to bind those files to.
func validateTopics(c *collector, p string, m *Module, seenInvariantID map[string]bool) {
	names := map[string]bool{}
	for j := range m.Topics {
		t := &m.Topics[j]
		tp := fmt.Sprintf("%s.topics[%d]", p, j)
		switch {
		case t.Name == "":
			c.add(Error, tp+".name", "is required")
		case !identRe.MatchString(t.Name):
			c.add(Error, tp+".name", "%q must match %s", t.Name, identRe)
		case names[t.Name]:
			c.add(Error, tp+".name", "duplicate topic name %q in module %q", t.Name, m.Name)
		}
		names[t.Name] = true
		if len(t.Files) == 0 {
			c.add(Error, tp+".files", "is required: a topic binds the files its patterns match, and no file without one")
		}
		for k, f := range t.Files {
			if err := CheckPathPattern(f); err != nil {
				c.add(Error, fmt.Sprintf("%s.files[%d]", tp, k), "%v", err)
			}
		}
		if len(t.Constraints) == 0 && len(t.Invariants) == 0 && t.Brief.Path == "" {
			c.add(Warning, tp, "topic %q has no constraints, invariants or brief; it binds nothing", t.Name)
		}
		for k, con := range t.Constraints {
			if strings.TrimSpace(con) == "" {
				c.add(Error, fmt.Sprintf("%s.constraints[%d]", tp, k), "cannot be empty")
			}
		}
		validateInvariants(c, tp, m.Name, t.Invariants, seenInvariantID)
	}
}

func topicsHaveInvariants(m *Module) bool {
	for _, t := range m.Topics {
		if len(t.Invariants) > 0 {
			return true
		}
	}
	return false
}
