package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/Pense-Laranja/aspect/internal/analyze"
	"github.com/Pense-Laranja/aspect/internal/llm"
	"github.com/Pense-Laranja/aspect/internal/spec"
)

// Fragment is what the Describer extracts from one package: the module spec
// it implies, plus the entities and surfaces it defines. Goals are proposed
// with local labels; the Synthesizer consolidates them into system goals.
type Fragment struct {
	Package     string           `json:"package"`
	Intent      string           `json:"intent"`
	Goals       []FragmentGoal   `json:"goals"`
	Entities    []spec.Entity    `json:"entities"`
	Interfaces  []spec.Interface `json:"interfaces"`
	Operations  []spec.Operation `json:"operations"`
	Invariants  []string         `json:"invariants"`
	Scenarios   []spec.Scenario  `json:"scenarios"`
	Constraints []string         `json:"constraints"`
	Notes       string           `json:"notes"`
}

// FragmentGoal is a goal proposed for one package.
type FragmentGoal struct {
	Label     string `json:"label"`
	Statement string `json:"statement"`
	Verify    string `json:"verify"`
}

// Describer reads one package's inventory and writes its fragment.
type Describer struct {
	LLM llm.Client
}

const describerSystem = `You are the Describer agent in Aspect, a pipeline that recovers a formal specification from an existing codebase.

You receive the inventory of one package: its doc comment, exported types (with struct fields and tags), exported functions and methods, route registrations, tests, and dependencies. From that evidence you write the module fragment of an Aspect spec.

Write intent as the reason the package exists, in the owner's words, not a paraphrase of its API. Propose goals that a user of the system would recognise as outcomes, each with how it can be verified: "test" for behaviour a scenario can demonstrate, "invariant" for a property that must always hold, "review" for qualities no test can execute.

Entities: only structs that are persisted (marked [persistent], or clearly stored). Field types are notation, not Go: string, int, int64, float, bool, time, decimal, uuid, bytes, json (a list or structured value stored as JSON; say its shape in the field's intent), or another entity's name. Never uint, slices, maps or a non-persisted struct's name. Mark the primary key, required and unique fields, and relations you can see from foreign keys or slices of other entities.

Interfaces: group routes into named interfaces using these canonical names: "api" for JSON or form endpoints called by programs, "web" for server-rendered pages for end users, "admin" for back-office pages, "cli" for commands, "grpc" for RPC services, "jobs" (kind job) for background jobs a queue or scheduler runs, "mcp" (kind mcp) for tools and resources an AI assistant calls. Each route becomes a surface with a short snake_case name, its route and method, the entity it operates on when obvious (with operations only for such CRUD surfaces: list, create, read, update, delete or a lowercase custom verb, never the handler's name), and the errors you can infer. A route is always real: an http or web route is the path the code registers, with any prefix resolved; a page event (a QOR5 web event, a form action) is a web surface with "event" set and the page that dispatches it as route; a job's route is its registered name; an mcp route is the tool or resource name; middleware takes the path pattern it wraps (/*). A function other packages call in-process is an operation, never a surface.

Operations: the package's exported API that other packages call, as language-neutral signatures: Name(arg: type, ...) -> result | error. Include pre and post conditions when the code makes them evident.

Scenarios: derive from the tests (test names are listed) and from obvious happy and error paths. Use concrete values. Ids are S1, S2, ...

When the package carries documents (README, CLAUDE.md, design notes), they are the owner's own words: take intent, invariants and constraints from them before inferring from code, and quote their rules as invariants.

Do not invent behaviour the evidence does not support; put uncertainties in notes. Answer with JSON matching the schema.`

func fragmentSchema() map[string]any {
	str := map[string]any{"type": "string"}
	obj := func(required []string, props map[string]any) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": props}
	}
	arr := func(items map[string]any) map[string]any { return map[string]any{"type": "array", "items": items} }
	// Kept small on purpose: the server compiles this into a grammar with a
	// size limit. Only keys the validator needs are required; enums are
	// kept only where a free string would produce an invalid spec.
	boolean := map[string]any{"type": "boolean"}
	field := obj([]string{"name", "type"}, map[string]any{
		"name": str, "type": str, "key": str, "required": boolean, "unique": boolean, "default": str, "intent": str,
	})
	relation := obj([]string{"kind", "entity"}, map[string]any{
		"kind": map[string]any{"type": "string", "enum": []string{"has_one", "has_many", "belongs_to", "many_to_many"}}, "entity": str, "via": str, "intent": str,
	})
	entity := obj([]string{"name", "intent", "fields"}, map[string]any{
		"name": str, "intent": str, "fields": arr(field), "relations": arr(relation), "constraints": stringList(),
	})
	surface := obj([]string{"name", "route"}, map[string]any{
		"name": str, "intent": str, "route": str, "method": str, "event": str, "entity": str, "operations": stringList(), "request": str, "response": str, "errors": stringList(), "auth": str,
	})
	iface := obj([]string{"name", "kind", "intent", "surfaces"}, map[string]any{
		"name": str, "kind": map[string]any{"type": "string", "enum": []string{"http", "web", "cli", "grpc", "app", "job", "mcp"}}, "intent": str, "framework": str, "auth": str,
		"surfaces": arr(surface),
	})
	operation := obj([]string{"name", "signature"}, map[string]any{
		"name": str, "signature": str, "intent": str, "pre": stringList(), "post": stringList(),
	})
	scenario := obj([]string{"id", "when", "then"}, map[string]any{"id": str, "given": str, "when": str, "then": str})
	goal := obj([]string{"label", "statement", "verify"}, map[string]any{
		"label": str, "statement": str, "verify": map[string]any{"type": "string", "enum": []string{"test", "invariant", "review"}},
	})
	return obj([]string{"package", "intent", "goals", "entities", "interfaces", "operations", "invariants", "scenarios", "constraints", "notes"}, map[string]any{
		"package": str, "intent": str, "goals": arr(goal), "entities": arr(entity), "interfaces": arr(iface), "operations": arr(operation),
		"invariants": stringList(), "scenarios": arr(scenario), "constraints": stringList(), "notes": str,
	})
}

// Describe writes the fragment for one package.
func (d *Describer) Describe(ctx context.Context, inv *analyze.Inventory, pkg *analyze.Package) (Fragment, llm.Response, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Codebase\n\nModule: %s\nLanguage: %s\nFrameworks in use: %s\nPackages: %d\n\n", inv.ModulePath, inv.Language, strings.Join(inv.Frameworks, ", "), len(inv.Packages))
	b.WriteString(pkg.Render(60000))
	b.WriteString("\nWrite this package's fragment.\n")
	var out Fragment
	resp, err := complete(ctx, d.LLM, describerSystem, b.String(), fragmentSchema(), &out)
	if err != nil {
		return out, resp, fmt.Errorf("describer %s: %w", pkg.Dir, err)
	}
	out.Package = pkg.Dir
	return out, resp, nil
}

// Synthesis is the system-level result. In retarget mode (target language
// differs from the source) it also carries the module list, entities and
// interfaces re-expressed for the target; otherwise those come from the
// fragments deterministically.
type Synthesis struct {
	Name        string             `json:"name"`
	Intent      string             `json:"intent"`
	Constraints []string           `json:"constraints"`
	Goals       []SynthesisGoal    `json:"goals"`
	Stack       spec.LanguageStack `json:"stack"`
	Interfaces  []spec.Interface   `json:"interfaces"`
	Database    *spec.Database     `json:"database"`
	Modules     []spec.Module      `json:"modules"`
	// Tiers is set in tiered modes (api_backend): one entry per tier. For a
	// backend tier that mirrors the source, Modules holds only additions.
	Tiers []spec.Tier `json:"tiers"`
	Notes string      `json:"notes"`
}

// SynthesisGoal is a system goal with the modules accountable for it.
type SynthesisGoal struct {
	ID        string   `json:"id"`
	Statement string   `json:"statement"`
	Verify    string   `json:"verify"`
	Modules   []string `json:"modules"`
}

// Synthesizer consolidates fragments into the system level of the spec.
type Synthesizer struct {
	LLM llm.Client
}

const synthesizerSystem = `You are the Synthesizer agent in Aspect, a pipeline that recovers a formal specification from an existing codebase.

You receive the fragments the Describer wrote for every package, and the codebase inventory summary. You write the system level of the Aspect spec: the system's intent, its goals, global constraints, the language stack, and one consolidated entry per interface.

Goals: consolidate the packages' proposed goals into 5 to 15 system goals a product owner would state. Give each an id G1, G2, ..., a verification method, and the list of modules (package directories, exactly as given) accountable for it. Every goal must have at least one module. Every module should appear in at least one goal.

Stack: describe the frameworks the codebase actually uses in a way a Coder could follow: name, import path (module), purpose, and concrete guidance on how they are used in this codebase. Include the ORM when there is one.

Interfaces: one entry per interface name used by the fragments (api, web, admin, cli, grpc, jobs, mcp), with kind, intent, the framework it is built on (must be one of the stack frameworks, or empty), and auth. Do not list surfaces; they are merged from the fragments.

Leave database and modules empty: they are assembled from the fragments.

Answer with JSON matching the schema.`

const retargetSystem = `You are the Synthesizer agent in Aspect, a pipeline that recovers a formal specification from an existing codebase and re-expresses it for a different platform.

You receive the fragments the Describer wrote for every package of the SOURCE system, plus the inventory summary, and a TARGET language. Write a complete Aspect spec for a new single-tier system in the target language that serves the same users and achieves the same goals, connecting directly to its own database (monolith topology). This is a translation of intent, not of code:

- System intent and goals are those of the source, restated for the target. Every goal names the target modules accountable for it.
- Modules: design the module list a good engineer would choose for the target platform, not a mirror of the source packages. Each module has an intent, depends_on, the entities it owns, the surfaces it implements, language-neutral interface operations, invariants and concrete scenarios (ids S1, S2, ... per module). Module names are snake_case identifiers.
- Database: the entities the target persists, with fields in neutral types.
- Interfaces: the source's user-facing surfaces re-expressed for the target (web pages stay "web" for a web target; they become "app" screens for a mobile target, route = navigation path). Leave provider empty.
- Stack: frameworks and packages for the target.
- Every entity must be owned by exactly one module; every surface implemented by exactly one module.

Answer with JSON matching the schema.`

const tieredSystem = `You are the Synthesizer agent in Aspect, a pipeline that recovers a formal specification from an existing codebase and splits it into tiers.

You receive the fragments the Describer wrote for every package of the SOURCE system, the inventory summary, and the target topology. Write the system level of an Aspect spec with TIERS: separate deployables in their own languages that talk only through interfaces.

api_backend topology, two tiers:
- The BACKEND tier owns the system database and serves an "api" interface. When the backend keeps the source language, it MIRRORS the source: its existing modules are listed for you by name and you must NOT redesign them. Propose only the modules to ADD to the backend so the frontend can work: typically one API module (snake_case name, intent, depends_on naming existing modules, the api surfaces it implements, operations, scenarios). Put those additions under the backend tier's modules. Also list, under interfaces, the "api" surfaces the frontend needs with provider = backend tier name; surfaces that already exist in the source keep their names, new ones get new names.
- The FRONTEND tier (web app or mobile app, in the target language) consumes the api. Design its module list for the target platform (for an iOS app: an api client, models, feature modules with view models and SwiftUI views; for a web frontend: pages, an api client, state). Its user-facing surfaces are an "app" interface (mobile) or a "web" interface (web frontend) with provider = frontend tier name. Modules that call the backend list the api surfaces under consumes. A tier-local database (cache) is optional.
- Tier names are snake_case (e.g. backend, mobile, web). The frontend tier depends_on the backend tier. Each tier has an intent, language and module_path.

cloud_service topology, one tier:
- One APP tier in the target language. Everything it needs from a server is an interface with provider "external" and a named service (Supabase, Firebase, a hosted REST API of the source system); the system database is external too (tier "external") and only documents the remote data model. The app may own a tier-local cache database.

For both: system intent and goals are those of the source restated for the new shape; every goal names the modules accountable for it (existing backend module names or the modules you design). Every entity of a generated database is owned by exactly one module in its tier; every provided surface is implemented by exactly one module in the providing tier; external surfaces are consumed, never implemented. Module names are unique across tiers.

Answer with JSON matching the schema.`

func synthesisSchema(mode Mode) map[string]any {
	str := map[string]any{"type": "string"}
	obj := func(required []string, props map[string]any) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": props}
	}
	arr := func(items map[string]any) map[string]any { return map[string]any{"type": "array", "items": items} }
	nullable := func(schema map[string]any) map[string]any {
		return map[string]any{"anyOf": []any{schema, map[string]any{"type": "null"}}}
	}
	framework := obj([]string{"name", "module"}, map[string]any{
		"name": str, "module": str, "version": str, "purpose": str, "guidance": str,
	})
	stack := obj([]string{"frameworks"}, map[string]any{
		"version": str, "frameworks": arr(framework), "orm": nullable(framework), "allowed_modules": stringList(), "guidance": str,
	})
	goal := obj([]string{"id", "statement", "verify", "modules"}, map[string]any{
		"id": str, "statement": str, "verify": map[string]any{"type": "string", "enum": []string{"test", "invariant", "review"}}, "modules": stringList(),
	})
	frag := fragmentSchema()["properties"].(map[string]any)
	surface := frag["interfaces"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["surfaces"].(map[string]any)
	iface := obj([]string{"name", "kind", "intent", "provider", "surfaces"}, map[string]any{
		"name": str, "kind": map[string]any{"type": "string", "enum": []string{"http", "web", "cli", "grpc", "app", "job", "mcp"}}, "intent": str, "framework": str, "auth": str,
		"provider": str, "service": str, "surfaces": surface,
	})
	entity := frag["entities"].(map[string]any)["items"].(map[string]any)
	operation := frag["operations"].(map[string]any)["items"].(map[string]any)
	scenario := obj([]string{"id", "when", "then"}, map[string]any{"id": str, "given": str, "when": str, "then": str, "goals": stringList()})
	module := obj([]string{"name", "intent"}, map[string]any{
		"name": str, "intent": str, "goals": stringList(), "depends_on": stringList(), "interface": arr(operation), "invariants": stringList(),
		"scenarios": arr(scenario), "constraints": stringList(), "entities": stringList(), "surfaces": stringList(), "consumes": stringList(),
	})
	dbTest := obj([]string{"engine"}, map[string]any{"engine": str, "dsn_env": str})
	database := obj([]string{"engine", "entities"}, map[string]any{
		"engine":     map[string]any{"type": "string", "enum": []string{"postgres", "mysql", "sqlite"}},
		"tier":       str,
		"migrations": map[string]any{"type": "string", "enum": []string{"auto", "files"}}, "test": dbTest, "entities": arr(entity),
	})
	props := map[string]any{
		"name": str, "intent": str, "constraints": stringList(), "goals": arr(goal), "stack": stack,
		"interfaces": arr(iface), "notes": str,
	}
	required := []string{"name", "intent", "constraints", "goals", "stack", "interfaces", "notes"}
	switch mode {
	case ModeRetarget:
		props["database"] = nullable(database)
		props["modules"] = arr(module)
		required = append(required, "database", "modules")
	case ModeTiered:
		tier := obj([]string{"name", "intent", "language", "modules"}, map[string]any{
			"name": str, "intent": str, "language": str, "module_path": str, "depends_on": stringList(), "stack": stack,
			"database": nullable(database), "modules": arr(module),
		})
		props["database"] = nullable(database)
		props["tiers"] = arr(tier)
		required = append(required, "database", "tiers")
	}
	return obj(required, props)
}

// Mode is how the Synthesizer is asked to shape the spec.
type Mode string

const (
	// ModeMirror keeps the source language and shape; one module per package.
	ModeMirror Mode = "mirror"
	// ModeRetarget re-expresses the whole system for another language as a
	// single tier (monolith).
	ModeRetarget Mode = "retarget"
	// ModeTiered splits the system into tiers (api_backend) or designs a
	// single app tier over external services (cloud_service).
	ModeTiered Mode = "tiered"
)

// SynthesisInput is what the Synthesizer sees.
type SynthesisInput struct {
	Inventory *analyze.Inventory
	Fragments []Fragment
	Mode      Mode
	Topology  spec.Topology
	// Target is the language of the new system (retarget) or of the
	// frontend tier (tiered).
	Target string
	// BackendLanguage is the backend tier's language in api_backend mode.
	BackendLanguage string
	// MirroredModules lists the backend modules that already exist when the
	// backend mirrors the source, so additions can depend on them.
	MirroredModules []string
	// TargetHint is free text from the user about the target (for example
	// "an iOS app for members; the admin stays on the web").
	TargetHint string
}

// Synthesize writes the system level (and, in retarget and tiered modes,
// the modules or tiers).
func (s *Synthesizer) Synthesize(ctx context.Context, in SynthesisInput) (Synthesis, llm.Response, error) {
	mode := in.Mode
	if mode == "" {
		mode = ModeMirror
	}
	var b strings.Builder
	var summary strings.Builder
	in.Inventory.Summary(&summary)
	fmt.Fprintf(&b, "# Source inventory\n\n```\n%s```\n\n", summary.String())
	switch mode {
	case ModeRetarget:
		fmt.Fprintf(&b, "# Target\n\nLanguage: %s\nTopology: monolith (single tier with its own database)\n", in.Target)
	case ModeTiered:
		fmt.Fprintf(&b, "# Target\n\nTopology: %s\n", in.Topology)
		if in.Topology == spec.APIBackend {
			fmt.Fprintf(&b, "Backend tier language: %s\nFrontend tier language: %s\n", in.BackendLanguage, in.Target)
			if len(in.MirroredModules) > 0 {
				fmt.Fprintf(&b, "The backend tier mirrors the source. Its existing modules (do not redesign; propose only additions): %s\n", strings.Join(in.MirroredModules, ", "))
			}
		} else {
			fmt.Fprintf(&b, "App tier language: %s\n", in.Target)
		}
	}
	if in.TargetHint != "" {
		fmt.Fprintf(&b, "Owner's guidance: %s\n", in.TargetHint)
	}
	b.WriteString("\n# Fragments\n\n")
	for _, f := range in.Fragments {
		fmt.Fprintf(&b, "```yaml\n%s```\n\n", renderYAML(f))
	}
	system := synthesizerSystem
	switch mode {
	case ModeRetarget:
		system = retargetSystem
		fmt.Fprintf(&b, "Write the complete spec for the %s version.\n", in.Target)
	case ModeTiered:
		system = tieredSystem
		fmt.Fprintf(&b, "Write the %s spec.\n", in.Topology)
	default:
		b.WriteString("Write the system level of the spec.\n")
	}
	var out Synthesis
	resp, err := complete(ctx, s.LLM, system, b.String(), synthesisSchema(mode), &out)
	if err != nil {
		return out, resp, fmt.Errorf("synthesizer: %w", err)
	}
	return out, resp, nil
}
