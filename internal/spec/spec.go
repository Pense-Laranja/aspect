// Package spec defines the Aspect specification format: a formal, machine-checkable
// description of a system, its modules, their intents, and the goals that decide
// whether the generated implementation is acceptable.
//
// The spec is the single source of truth for every agent in the pipeline. Agents
// never invent requirements; they read them from here.
package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version is the only spec format version this build understands.
const Version = 1

// VerifyMethod tells the pipeline how a goal is checked.
type VerifyMethod string

const (
	// VerifyTest means the goal is demonstrated by scenarios that become tests.
	VerifyTest VerifyMethod = "test"
	// VerifyInvariant means the goal is a property that must hold in every state;
	// it becomes property-style tests plus a Validator review.
	VerifyInvariant VerifyMethod = "invariant"
	// VerifyReview means the goal cannot be executed and is judged by the
	// Validator agent reading the code (for example, "the API is idiomatic").
	VerifyReview VerifyMethod = "review"
)

// Spec is the root document. A single-tier spec puts language, module path,
// stack and modules directly under system and at the top level; a
// multi-tier spec puts them under tiers instead.
type Spec struct {
	Aspect  int      `yaml:"aspect" json:"aspect"`
	System  System   `yaml:"system" json:"system"`
	Tiers   []Tier   `yaml:"tiers,omitempty" json:"tiers,omitempty"`
	Modules []Module `yaml:"modules,omitempty" json:"modules,omitempty"`

	// Dir is the directory the spec was loaded from; brief paths resolve
	// against it. Empty for specs parsed from memory.
	Dir string `yaml:"-" json:"-"`
	// Path is the file the spec was loaded from.
	Path string `yaml:"-" json:"-"`
	// Root is the repository the spec belongs to (RepositoryRoot): the
	// nearest directory at or above Dir that holds .git, else one holding a
	// project manifest, else Dir itself. Briefs and includes may lead
	// anywhere inside it and nowhere outside. Absolute; empty for specs
	// parsed from memory.
	Root string `yaml:"-" json:"-"`
	// Deps holds the loaded specs of System.Dependencies, by name.
	Deps map[string]*Spec `yaml:"-" json:"-"`
	// Sources maps the loaded YAML back to the files it was written in;
	// nil for specs parsed from memory. See Locate.
	Sources *Sources `yaml:"-" json:"-"`
}

// Dependency is another Aspect system this one consumes interfaces of. It
// is described by its own spec, loaded and validated with this one, and
// never built by this system's run.
type Dependency struct {
	Name string `yaml:"name" json:"name"`
	// Spec is the path of the dependency's spec, relative to this spec.
	Spec   string `yaml:"spec" json:"spec"`
	Intent string `yaml:"intent,omitempty" json:"intent,omitempty"`
}

// PackageBrief is the document a package keeps its spec-side rules in (the
// domain rules, the seams, the history behind them), read in place as its
// module's brief: by the agents, by `aspect context`, and by people. It is
// not a coding assistant's instructions file (CLAUDE.md, AGENTS.md), which
// an assistant loads whatever the task; a project keeps that one short and
// points it at `aspect context`.
const PackageBrief = "ASPECT.md"

// Brief is a sidecar document: a longer, free-form description of what a
// system, tier or module must be, kept in its own file so it can run to
// pages without crowding the YAML. Every agent working on the owner sees the
// whole text. The file may be anywhere in the repository that holds the
// spec, so a package's own ASPECT.md can be its module's brief in place,
// read by people, `aspect context` and the agents alike, instead of a copy
// beside the spec that drifts from it.
type Brief struct {
	// Path is relative to the spec file's directory. It may climb out of
	// that directory (../modules/order/ASPECT.md) but not out of the
	// repository (Spec.Root).
	Path string `yaml:"brief,omitempty" json:"brief,omitempty"`
	// Text is the file's content, filled by Load.
	Text string `yaml:"-" json:"-"`
}

// Topology names the shape of the solution.
type Topology string

const (
	// Monolith is one tier that owns the database and serves every interface.
	Monolith Topology = "monolith"
	// APIBackend is a frontend tier (web or mobile app) consuming an API
	// served by a backend tier that owns the database.
	APIBackend Topology = "api_backend"
	// CloudService is one generated app tier consuming interfaces and a
	// database provided by an external, hosted service.
	CloudService Topology = "cloud_service"
)

// Tier is one deployable unit with its own language and toolchain: a
// backend service, a web frontend, a mobile app. Tiers talk to each other
// only through interfaces.
type Tier struct {
	Name       string `yaml:"name" json:"name"`
	Intent     string `yaml:"intent" json:"intent"`
	Language   string `yaml:"language,omitempty" json:"language,omitempty"`
	ModulePath string `yaml:"module_path,omitempty" json:"module_path,omitempty"`
	// DependsOn orders tiers: a consumer is built after its provider.
	DependsOn []string `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	Stack     Stack    `yaml:"stack,omitempty" json:"stack,omitempty"`
	// Database local to this tier (an app's cache). The system database is
	// owned by the tier named in Database.Tier.
	Database *Database `yaml:"database,omitempty" json:"database,omitempty"`
	Modules  []Module  `yaml:"modules" json:"modules"`
	Brief    `yaml:",inline" json:",inline"`
}

// System describes the whole program the agents must produce.
type System struct {
	Name   string `yaml:"name" json:"name"`
	Intent string `yaml:"intent" json:"intent"`
	// Topology is monolith, api_backend or cloud_service. Derived when empty.
	Topology    Topology `yaml:"topology,omitempty" json:"topology,omitempty"`
	Language    string   `yaml:"language,omitempty" json:"language,omitempty"`
	ModulePath  string   `yaml:"module_path,omitempty" json:"module_path,omitempty"`
	Goals       []Goal   `yaml:"goals,omitempty" json:"goals,omitempty"`
	Constraints []string `yaml:"constraints,omitempty" json:"constraints,omitempty"`
	// Stack holds language-specific configuration keyed by language name
	// ("go", "python", ...). Only the entry for System.Language is used.
	Stack Stack `yaml:"stack,omitempty" json:"stack,omitempty"`
	// Database, when present, declares the persistent data model.
	Database *Database `yaml:"database,omitempty" json:"database,omitempty"`
	// Interfaces declare how the system is exposed: HTTP APIs, web UIs,
	// command lines, gRPC services, native app screens.
	Interfaces []Interface `yaml:"interfaces,omitempty" json:"interfaces,omitempty"`
	// Source records where an imported spec came from. Absent for specs
	// written by hand.
	Source *Source `yaml:"source,omitempty" json:"source,omitempty"`
	// Dependencies are other Aspect systems whose interfaces this one
	// consumes, referenced in surface notation as "<name>/<interface>.<surface>".
	Dependencies []Dependency `yaml:"dependencies,omitempty" json:"dependencies,omitempty"`
	Brief        `yaml:",inline" json:",inline"`
}

// Source is the provenance of a spec produced by `aspect import`.
type Source struct {
	Language   string `yaml:"language,omitempty" json:"language,omitempty"`
	Repository string `yaml:"repository,omitempty" json:"repository,omitempty"`
	Commit     string `yaml:"commit,omitempty" json:"commit,omitempty"`
	ImportedAt string `yaml:"imported_at,omitempty" json:"imported_at,omitempty"`
	// Frameworks detected in the source, kept for reference when the target
	// language differs and the stack section cannot carry them.
	Frameworks []string `yaml:"frameworks,omitempty" json:"frameworks,omitempty"`
	// Exclude are the directories the import was told to skip (names or
	// relative paths, prefix match). Drift does not report code there as
	// unclaimed: the spec never meant to describe it.
	Exclude []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
}

// Goal is an outcome the finished system must achieve. Every goal is owned by at
// least one module, and the Validator issues a verdict per goal.
type Goal struct {
	ID        string       `yaml:"id" json:"id"`
	Statement string       `yaml:"statement" json:"statement"`
	Verify    VerifyMethod `yaml:"verify" json:"verify"`
}

// Stack maps a language name to its configuration.
type Stack map[string]LanguageStack

// LanguageStack is the language-specific configuration: which frameworks and
// libraries the generated code may use and how. The shape is the same for
// every language; "module" means an import path in Go, a package in Python.
type LanguageStack struct {
	// Version of the language toolchain, e.g. "1.26".
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	// Frameworks the system is built on, e.g. qor5 for a Go admin UI.
	Frameworks []Framework `yaml:"frameworks,omitempty" json:"frameworks,omitempty"`
	// ORM, when the database is accessed through one.
	ORM *Framework `yaml:"orm,omitempty" json:"orm,omitempty"`
	// AllowedModules are import-path prefixes generated code may import in
	// addition to the standard library, the system's own module, and the
	// modules of Frameworks and ORM. The pipeline enforces this list.
	AllowedModules []string `yaml:"allowed_modules,omitempty" json:"allowed_modules,omitempty"`
	// Guidance is free-text advice for the Coder and Tester: conventions,
	// project layout, idioms to follow or avoid.
	Guidance string `yaml:"guidance,omitempty" json:"guidance,omitempty"`
}

// Framework is a library the generated code builds on.
type Framework struct {
	Name    string `yaml:"name" json:"name"`
	Module  string `yaml:"module,omitempty" json:"module,omitempty"`
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	Purpose string `yaml:"purpose,omitempty" json:"purpose,omitempty"`
	// Guidance tells agents how this framework is meant to be used here.
	Guidance string `yaml:"guidance,omitempty" json:"guidance,omitempty"`
}

// Database declares persistence.
type Database struct {
	// Engine is postgres, mysql or sqlite.
	Engine string `yaml:"engine" json:"engine"`
	// Tier that owns the system database in a multi-tier spec, or
	// "external" when a hosted service owns it (cloud_service topology).
	Tier string `yaml:"tier,omitempty" json:"tier,omitempty"`
	// Migrations is "auto" (the ORM migrates the schema at startup) or
	// "files" (versioned migration files are generated).
	Migrations string `yaml:"migrations,omitempty" json:"migrations,omitempty"`
	// Test says which database the generated tests run against.
	Test DBTest `yaml:"test,omitempty" json:"test,omitempty"`
	// Entities are the persistent types.
	Entities []Entity `yaml:"entities,omitempty" json:"entities,omitempty"`
}

// DBTest configures the database used by generated tests.
type DBTest struct {
	// Engine used by tests; sqlite means an in-memory database needing no
	// external service. Defaults to sqlite unless Database.Engine is sqlite.
	Engine string `yaml:"engine" json:"engine"`
	// DSNEnv names an environment variable; when it is set at test time the
	// tests connect to that DSN instead of the test engine default.
	DSNEnv string `yaml:"dsn_env,omitempty" json:"dsn_env,omitempty"`
}

// Entity is one persistent type (a table, a collection).
type Entity struct {
	Name        string     `yaml:"name" json:"name"`
	Intent      string     `yaml:"intent,omitempty" json:"intent,omitempty"`
	Fields      []Field    `yaml:"fields" json:"fields"`
	Relations   []Relation `yaml:"relations,omitempty" json:"relations,omitempty"`
	Constraints []string   `yaml:"constraints,omitempty" json:"constraints,omitempty"`
}

// Field is one attribute of an entity.
type Field struct {
	Name string `yaml:"name" json:"name"`
	// Type is neutral (string, int, int64, float, bool, time, decimal, uuid,
	// bytes, json) or the name of an entity. Agents map it to the target
	// language and the database column type.
	Type string `yaml:"type" json:"type"`
	// Key is "primary" for the primary key, empty otherwise.
	Key      string `yaml:"key,omitempty" json:"key,omitempty"`
	Required bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Unique   bool   `yaml:"unique,omitempty" json:"unique,omitempty"`
	Default  string `yaml:"default,omitempty" json:"default,omitempty"`
	Intent   string `yaml:"intent,omitempty" json:"intent,omitempty"`
}

// Relation links two entities.
type Relation struct {
	// Kind is has_one, has_many, belongs_to or many_to_many.
	Kind   string `yaml:"kind" json:"kind"`
	Entity string `yaml:"entity,omitempty" json:"entity,omitempty"`
	// Via names the field or join table carrying the relation.
	Via    string `yaml:"via,omitempty" json:"via,omitempty"`
	Intent string `yaml:"intent,omitempty" json:"intent,omitempty"`
}

// Interface is one way the system is exposed to users or other systems.
type Interface struct {
	Name string `yaml:"name" json:"name"`
	// Kind is http, web, cli, grpc, app (native screens), job (background
	// jobs a queue or scheduler runs) or mcp (tools and resources an AI
	// client calls).
	Kind   string `yaml:"kind" json:"kind"`
	Intent string `yaml:"intent,omitempty" json:"intent,omitempty"`
	// Provider is the tier that serves this interface, or "external" when a
	// service outside the spec does (a hosted backend, a third-party API).
	// Empty means the only tier of a single-tier spec. Other tiers reach the
	// interface through modules that list it under consumes.
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	// Service names the external provider when Provider is "external"
	// (for example "Supabase" or "Firebase").
	Service string `yaml:"service,omitempty" json:"service,omitempty"`
	// Framework names an entry in Stack[language].Frameworks used to build
	// this interface (for example "qor5" for a web admin).
	Framework string `yaml:"framework,omitempty" json:"framework,omitempty"`
	// Auth describes who may use this interface; surfaces can override it.
	Auth     string    `yaml:"auth,omitempty" json:"auth,omitempty"`
	Surfaces []Surface `yaml:"surfaces,omitempty" json:"surfaces,omitempty"`
}

// Surface is one endpoint, page, command or RPC of an interface.
type Surface struct {
	Name   string `yaml:"name" json:"name"`
	Intent string `yaml:"intent,omitempty" json:"intent,omitempty"`
	// Route is the path (http, web), command line (cli), RPC name (grpc),
	// navigation path (app), registered job name (job) or tool or resource
	// name (mcp).
	Route string `yaml:"route" json:"route"`
	// Method is the HTTP method for http surfaces.
	Method string `yaml:"method,omitempty" json:"method,omitempty"`
	// Event names a page event a web surface handles (a QOR5 web event,
	// an htmx or form action) rather than a page of its own; Route is then
	// the page that dispatches it.
	Event string `yaml:"event,omitempty" json:"event,omitempty"`
	// Entity the surface operates on, when it is a CRUD surface.
	Entity string `yaml:"entity,omitempty" json:"entity,omitempty"`
	// Operations for entity-bound surfaces: list, create, read, update, delete
	// or a custom verb.
	Operations []string `yaml:"operations,omitempty" json:"operations,omitempty"`
	Request    string   `yaml:"request,omitempty" json:"request,omitempty"`
	Response   string   `yaml:"response,omitempty" json:"response,omitempty"`
	Errors     []string `yaml:"errors,omitempty" json:"errors,omitempty"`
	Auth       string   `yaml:"auth,omitempty" json:"auth,omitempty"`
}

// Invariant is a property that must hold in every state of a module.
// In specs it can be written as a plain string or as an object with ID, category,
// statement, and enforcement method.
type Invariant struct {
	ID          string `yaml:"id,omitempty" json:"id,omitempty"`
	Category    string `yaml:"category,omitempty" json:"category,omitempty"` // correctness, performance, quality, readability, security, etc.
	Statement   string `yaml:"statement" json:"statement"`
	Enforcement string `yaml:"enforcement,omitempty" json:"enforcement,omitempty"` // test, ast, review
}

func (inv *Invariant) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		inv.Statement = value.Value
		return nil
	}
	type raw Invariant
	var r raw
	if err := value.Decode(&r); err != nil {
		return err
	}
	*inv = Invariant(r)
	return nil
}

func (inv Invariant) MarshalYAML() (any, error) {
	if inv.ID == "" && inv.Category == "" && inv.Enforcement == "" {
		return inv.Statement, nil
	}
	type raw Invariant
	return raw(inv), nil
}

func (inv *Invariant) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	if data[0] == '"' {
		return json.Unmarshal(data, &inv.Statement)
	}
	type raw Invariant
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	*inv = Invariant(r)
	return nil
}

func (inv Invariant) MarshalJSON() ([]byte, error) {
	if inv.ID == "" && inv.Category == "" && inv.Enforcement == "" {
		return json.Marshal(inv.Statement)
	}
	type raw Invariant
	return json.Marshal(raw(inv))
}

func (inv Invariant) String() string {
	if inv.ID != "" {
		return fmt.Sprintf("[%s] %s", inv.ID, inv.Statement)
	}
	return inv.Statement
}

// Module is a unit of implementation with its own intent. Modules are generated
// in dependency order, each one seeing the interfaces of its dependencies.
type Module struct {
	Name string `yaml:"name" json:"name"`
	// Path is the directory holding the module's code, relative to the
	// tier's code root, as a slash path ("." for the root). Empty means the
	// language's default layout (<name>/ in Go). aspect import writes it,
	// because a package directory cannot be recovered from a module name:
	// modules/api and modules_api both become the module modules_api. A
	// module with a path owns that one directory, not its subdirectories.
	Path        string      `yaml:"path,omitempty" json:"path,omitempty"`
	Intent      string      `yaml:"intent" json:"intent"`
	Goals       []string    `yaml:"goals,omitempty" json:"goals,omitempty"`
	DependsOn   []string    `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	Interface   []Operation `yaml:"interface,omitempty" json:"interface,omitempty"`
	Invariants  []Invariant `yaml:"invariants,omitempty" json:"invariants,omitempty"`
	Scenarios   []Scenario  `yaml:"scenarios,omitempty" json:"scenarios,omitempty"`
	Constraints []string    `yaml:"constraints,omitempty" json:"constraints,omitempty"`
	// Entities this module owns: it defines the persistent type and is the
	// only module that writes it.
	Entities []string `yaml:"entities,omitempty" json:"entities,omitempty"`
	// Surfaces this module implements, as "interface.surface" or "interface"
	// for every surface of that interface. Only modules in the providing
	// tier may implement.
	Surfaces []string `yaml:"surfaces,omitempty" json:"surfaces,omitempty"`
	// Consumes lists surfaces this module is a client of, served by another
	// tier or an external service, in the same notation.
	Consumes []string `yaml:"consumes,omitempty" json:"consumes,omitempty"`
	Brief    `yaml:",inline" json:",inline"`
}

// Operation is one exported function or method the module must expose, with
// optional design-by-contract clauses.
type Operation struct {
	Name      string   `yaml:"name" json:"name"`
	Signature string   `yaml:"signature" json:"signature"`
	Intent    string   `yaml:"intent,omitempty" json:"intent,omitempty"`
	Pre       []string `yaml:"pre,omitempty" json:"pre,omitempty"`
	Post      []string `yaml:"post,omitempty" json:"post,omitempty"`
}

// Scenario is a Given/When/Then example. The Tester agent turns each scenario
// into at least one test, and the Validator checks that mapping exists.
type Scenario struct {
	ID         string   `yaml:"id" json:"id"`
	Given      string   `yaml:"given,omitempty" json:"given,omitempty"`
	When       string   `yaml:"when" json:"when"`
	Then       string   `yaml:"then" json:"then"`
	Goals      []string `yaml:"goals,omitempty" json:"goals,omitempty"`
	Invariants []string `yaml:"invariants,omitempty" json:"invariants,omitempty"`
}

// Load reads a spec file, resolves its includes, parses it, and loads its
// brief sidecars and system dependencies. It does not validate semantics;
// call Validate for that.
func Load(path string) (*Spec, error) {
	return load(path, map[string]bool{})
}

func load(path string, loading map[string]bool) (*Spec, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if loading[abs] {
		return nil, fmt.Errorf("dependency cycle through %s", path)
	}
	loading[abs] = true
	defer delete(loading, abs)

	data, src, err := expand(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(data)
	if err != nil {
		return nil, err
	}
	s.Dir = filepath.Dir(path)
	s.Path = path
	s.Sources = src
	if s.Root, err = RepositoryRoot(s.Dir); err != nil {
		return nil, err
	}
	if err := s.LoadBriefs(); err != nil {
		return nil, err
	}
	if err := s.loadDependencies(loading); err != nil {
		return nil, err
	}
	return s, nil
}

// loadDependencies loads and validates every system dependency. A
// dependency with validation errors is an error here: this spec cannot
// consume interfaces of a system that does not hold together.
func (s *Spec) loadDependencies(loading map[string]bool) error {
	for i := range s.System.Dependencies {
		d := &s.System.Dependencies[i]
		if d.Spec == "" {
			continue
		}
		dep, err := load(filepath.Join(s.Dir, d.Spec), loading)
		if err != nil {
			return fmt.Errorf("dependency %s: %w", d.Name, err)
		}
		if issues := Validate(dep); issues.HasErrors() {
			var lines []string
			for _, i := range issues {
				if i.Severity == Error {
					lines = append(lines, i.String())
				}
			}
			return fmt.Errorf("dependency %s (%s) has errors:\n  %s", d.Name, d.Spec, strings.Join(lines, "\n  "))
		}
		if s.Deps == nil {
			s.Deps = map[string]*Spec{}
		}
		s.Deps[d.Name] = dep
	}
	return nil
}

// Briefs returns every brief in the spec (system, tiers, modules) with the
// name of its owner, for loading and validation.
func (s *Spec) Briefs() map[string]*Brief {
	out := map[string]*Brief{"system": &s.System.Brief}
	for i := range s.Tiers {
		out["tier "+s.Tiers[i].Name] = &s.Tiers[i].Brief
	}
	for _, m := range s.AllModules() {
		out["module "+m.Name] = &m.Brief
	}
	return out
}

// LoadBriefs reads every brief file relative to Dir. Missing files, and
// paths that lead out of the repository, are reported together so the user
// fixes them in one pass; a file outside the repository is never read, since
// a brief's text is sent to a model.
func (s *Spec) LoadBriefs() error {
	var missing []string
	for owner, b := range s.Briefs() {
		if b.Path == "" {
			continue
		}
		file, err := s.BriefFile(b.Path)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s: %v", owner, err))
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s: %s (%v)", owner, b.Path, err))
			continue
		}
		b.Text = string(data)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("brief files not found:\n  %s", strings.Join(missing, "\n  "))
	}
	return nil
}

// BriefFile is the file a brief path names, checked to stay inside the
// repository: lexically, and again once symbolic links are resolved, so a
// link committed in the repository cannot lead a brief to ~/.ssh. A file that
// does not exist yet passes the second check (os.ReadFile then reports it).
func (s *Spec) BriefFile(p string) (string, error) {
	if err := s.checkBriefPath(p); err != nil {
		return "", err
	}
	file := filepath.Join(s.Dir, filepath.FromSlash(p))
	if s.Root == "" {
		return file, nil
	}
	if err := inRepository(s.Root, file); err != nil {
		return "", fmt.Errorf("%q %v", p, err)
	}
	return file, nil
}

// inRepository checks that file (absolute or relative to the working
// directory) is inside the repository at root and not under its .git, as
// written and once symbolic links are resolved. The fragments a spec
// includes and its briefs go through it: they are sent to a model by
// `aspect review`, so a spec under review must not be able to name the
// reviewer's own files, nor .git/config, which can hold a CI job's token.
// Dependency specs do not: another system's spec may live elsewhere, and
// it is never put in a prompt. A file that does not exist yet passes the second check (the
// read then reports it).
func inRepository(root, file string) error {
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	if !Inside(root, abs) {
		return fmt.Errorf("must stay inside the repository %s", root)
	}
	if underGit(root, abs) {
		return errors.New("is under .git, which holds no spec or brief and may hold credentials")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	if !Inside(realRoot, resolved) {
		return fmt.Errorf("resolves outside the repository %s through a symbolic link", root)
	}
	if underGit(realRoot, resolved) {
		return errors.New("resolves under .git through a symbolic link; .git holds no spec or brief and may hold credentials")
	}
	return nil
}

// underGit reports whether path, inside root, is in a .git directory. The
// name is compared without regard to case: on a case-insensitive file
// system .GIT is .git.
func underGit(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.EqualFold(seg, ".git") {
			return true
		}
	}
	return false
}

// checkBriefPath is the lexical rule: relative, inside the repository
// (inside the spec's directory for a spec parsed from memory, which has no
// repository), and not under a .git directory, whose config can hold a CI
// job's token.
func (s *Spec) checkBriefPath(p string) error {
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("%q must be a relative path", p)
	}
	if s.Root == "" {
		if c := filepath.Clean(filepath.FromSlash(p)); c == ".." || strings.HasPrefix(c, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%q must stay inside the spec's directory", p)
		}
		return nil
	}
	abs, err := filepath.Abs(filepath.Join(s.Dir, filepath.FromSlash(p)))
	if err != nil {
		return err
	}
	if !Inside(s.Root, abs) {
		return fmt.Errorf("%q must stay inside the repository %s", p, s.Root)
	}
	if underGit(s.Root, abs) {
		return fmt.Errorf("%q is under .git, which holds no brief and may hold credentials", p)
	}
	return nil
}

// Inside reports whether path is dir or below it; both are absolute and
// clean.
func Inside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// RepositoryRoot is the nearest directory at or above dir that holds a .git
// entry (a directory, or the file a worktree or submodule has). A tree with
// no .git anywhere above (a git archive export, a release tarball, a build
// context that leaves .git out) is rooted at the nearest directory holding a
// project manifest (projectManifests) instead, so a spec that loads in a
// clone loads in its export too; with neither, dir itself. The result is
// absolute.
func RepositoryRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if d, ok := nearestAbove(abs, ".git"); ok {
		return d, nil
	}
	if d, ok := nearestAbove(abs, projectManifests...); ok {
		return d, nil
	}
	return abs, nil
}

// projectManifests mark a project's root where there is no .git: one per
// language profile.
var projectManifests = []string{"go.mod", "Package.swift"}

// nearestAbove is the nearest directory at or above dir holding any of
// names.
func nearestAbove(dir string, names ...string) (string, bool) {
	for d := dir; ; {
		for _, n := range names {
			if _, err := os.Lstat(filepath.Join(d, n)); err == nil {
				return d, true
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}

// Parse decodes YAML into a Spec and applies defaults.
func Parse(data []byte) (*Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}
	s.applyDefaults()
	return &s, nil
}

func (s *Spec) applyDefaults() {
	if s.System.Language == "" {
		s.System.Language = "go"
	}
	for i := range s.System.Goals {
		if s.System.Goals[i].Verify == "" {
			s.System.Goals[i].Verify = VerifyTest
		}
	}
	for i := range s.Tiers {
		if s.Tiers[i].Language == "" {
			s.Tiers[i].Language = "go"
		}
	}
	for _, db := range s.Databases() {
		if db.Migrations == "" {
			db.Migrations = "auto"
		}
		if db.Test.Engine == "" {
			db.Test.Engine = "sqlite"
		}
	}
}

// EffectiveTiers returns the tiers of the spec. A single-tier spec yields
// one tier with an empty name carrying the system's language, module path,
// stack and modules, so callers never branch on the spec's shape.
func (s *Spec) EffectiveTiers() []*Tier {
	if len(s.Tiers) > 0 {
		out := make([]*Tier, len(s.Tiers))
		for i := range s.Tiers {
			out[i] = &s.Tiers[i]
		}
		return out
	}
	return []*Tier{s.implicitTier()}
}

// implicitTier views a single-tier spec as a tier. The returned tier shares
// the spec's module slice, so edits through it reach the spec.
func (s *Spec) implicitTier() *Tier {
	return &Tier{
		Name:       "",
		Intent:     s.System.Intent,
		Language:   s.System.Language,
		ModulePath: s.System.ModulePath,
		Stack:      s.System.Stack,
		Modules:    s.Modules,
	}
}

// Tier returns the tier with the given name, or nil. The empty name returns
// the implicit tier of a single-tier spec.
func (s *Spec) Tier(name string) *Tier {
	for _, t := range s.EffectiveTiers() {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// AllModules returns every module across tiers, in tier order.
func (s *Spec) AllModules() []*Module {
	var out []*Module
	for _, t := range s.EffectiveTiers() {
		for i := range t.Modules {
			out = append(out, &t.Modules[i])
		}
	}
	return out
}

// Module returns the module with the given name in any tier, or nil.
func (s *Spec) Module(name string) *Module {
	for _, m := range s.AllModules() {
		if m.Name == name {
			return m
		}
	}
	return nil
}

// TierOf returns the tier containing the named module, or nil.
func (s *Spec) TierOf(module string) *Tier {
	for _, t := range s.EffectiveTiers() {
		for i := range t.Modules {
			if t.Modules[i].Name == module {
				return t
			}
		}
	}
	return nil
}

// EffectiveTopology returns the declared topology, or derives it: one tier
// with an external provider is cloud_service, several tiers are
// api_backend, otherwise monolith.
func (s *Spec) EffectiveTopology() Topology {
	if s.System.Topology != "" {
		return s.System.Topology
	}
	if len(s.Tiers) > 1 {
		return APIBackend
	}
	for _, i := range s.System.Interfaces {
		if i.Provider == "external" {
			return CloudService
		}
	}
	if s.System.Database != nil && s.System.Database.Tier == "external" {
		return CloudService
	}
	return Monolith
}

// Goal returns the system goal with the given id, or nil.
func (s *Spec) Goal(id string) *Goal {
	for i := range s.System.Goals {
		if s.System.Goals[i].ID == id {
			return &s.System.Goals[i]
		}
	}
	return nil
}

// LanguageStack returns the stack configuration of the first tier, or nil.
// Multi-tier callers use Tier.LanguageStack.
func (s *Spec) LanguageStack() *LanguageStack {
	return s.EffectiveTiers()[0].LanguageStack()
}

// LanguageStack returns the tier's stack for its language, or nil.
func (t *Tier) LanguageStack() *LanguageStack {
	if ls, ok := t.Stack[t.Language]; ok {
		return &ls
	}
	return nil
}

// CodeDir is where the tier's code lives, relative to the repository the
// spec describes: <tier>/ for a tier of a multi-tier spec, the root ("")
// for the implicit tier of a single-tier spec, whose Name is empty. It is
// the layout `aspect drift -repo` checks.
func (t *Tier) CodeDir() string { return t.Name }

// ModuleRefs returns pointers to the tier's modules, in order.
func (t *Tier) ModuleRefs() []*Module {
	out := make([]*Module, len(t.Modules))
	for i := range t.Modules {
		out[i] = &t.Modules[i]
	}
	return out
}

// Placed reports whether the module's directory is its explicit Path (one
// package, that directory only) rather than the language's layout by name
// (the directory and everything below it).
func (m *Module) Placed() bool { return m.Path != "" }

// Module returns the tier's module with the given name, or nil.
func (t *Tier) Module(name string) *Module {
	for i := range t.Modules {
		if t.Modules[i].Name == name {
			return &t.Modules[i]
		}
	}
	return nil
}

// AllowedImports lists the import-path prefixes generated code in this tier
// may use beyond the standard library: the tier's own module, every
// framework and ORM module, and the explicit allowlist.
func (t *Tier) AllowedImports() []string {
	out := []string{t.ModulePath}
	if ls := t.LanguageStack(); ls != nil {
		for _, f := range ls.Frameworks {
			if f.Module != "" {
				out = append(out, f.Module)
			}
		}
		if ls.ORM != nil && ls.ORM.Module != "" {
			out = append(out, ls.ORM.Module)
		}
		out = append(out, ls.AllowedModules...)
	}
	return out
}

// Databases returns the system database and every tier-local database.
func (s *Spec) Databases() []*Database {
	var out []*Database
	if s.System.Database != nil {
		out = append(out, s.System.Database)
	}
	for _, t := range s.EffectiveTiers() {
		if t.Database != nil {
			out = append(out, t.Database)
		}
	}
	return out
}

// Entity returns the entity with the given name from any database, or nil.
func (s *Spec) Entity(name string) *Entity {
	for _, db := range s.Databases() {
		for i := range db.Entities {
			if db.Entities[i].Name == name {
				return &db.Entities[i]
			}
		}
	}
	return nil
}

// Interface returns the interface with the given name, or nil.
func (s *Spec) Interface(name string) *Interface {
	for i := range s.System.Interfaces {
		if s.System.Interfaces[i].Name == name {
			return &s.System.Interfaces[i]
		}
	}
	return nil
}

// SurfaceRef is a resolved "interface.surface" reference, possibly into a
// system dependency ("<system>/interface.surface").
type SurfaceRef struct {
	// System is the dependency name, empty for this spec's own interfaces.
	System    string
	Interface *Interface
	Surface   *Surface
}

// ID is the canonical form: "interface.surface", or
// "system/interface.surface" for a dependency's surface.
func (r SurfaceRef) ID() string {
	id := r.Interface.Name + "." + r.Surface.Name
	if r.System != "" {
		return r.System + "/" + id
	}
	return id
}

// Dependency returns a declared dependency by name, or nil.
func (s *Spec) Dependency(name string) *Dependency {
	for i := range s.System.Dependencies {
		if s.System.Dependencies[i].Name == name {
			return &s.System.Dependencies[i]
		}
	}
	return nil
}

// ResolveSurfaces expands a module's Surfaces or Consumes list. A bare
// interface name expands to all of its surfaces; "<system>/..." resolves in
// a loaded dependency. Unknown references are returned as errors so the
// validator can report them by position.
func (s *Spec) ResolveSurfaces(refs []string) ([]SurfaceRef, []error) {
	var out []SurfaceRef
	var errs []error
	for _, ref := range refs {
		target, local := s, ref
		system := ""
		if sysName, rest, ok := strings.Cut(ref, "/"); ok {
			if s.Dependency(sysName) == nil {
				errs = append(errs, fmt.Errorf("unknown system %q in %q (declare it under system.dependencies)", sysName, ref))
				continue
			}
			dep, loaded := s.Deps[sysName]
			if !loaded {
				errs = append(errs, fmt.Errorf("dependency %q is not loaded, cannot resolve %q", sysName, ref))
				continue
			}
			target, local, system = dep, rest, sysName
		}
		ifaceName, surfName, hasSurface := strings.Cut(local, ".")
		iface := target.Interface(ifaceName)
		if iface == nil {
			errs = append(errs, fmt.Errorf("unknown interface %q in %q", ifaceName, ref))
			continue
		}
		if !hasSurface {
			for i := range iface.Surfaces {
				out = append(out, SurfaceRef{System: system, Interface: iface, Surface: &iface.Surfaces[i]})
			}
			continue
		}
		found := false
		for i := range iface.Surfaces {
			if iface.Surfaces[i].Name == surfName {
				out = append(out, SurfaceRef{System: system, Interface: iface, Surface: &iface.Surfaces[i]})
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, fmt.Errorf("unknown surface %q in interface %q", surfName, ifaceName))
		}
	}
	return out, errs
}

// AllowedImports is Tier.AllowedImports for the first tier.
func (s *Spec) AllowedImports() []string {
	return s.EffectiveTiers()[0].AllowedImports()
}
