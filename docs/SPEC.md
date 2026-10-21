# The Aspect specification language

An Aspect spec is a YAML document that states what a system is for, what it
must achieve, and how the pieces are cut, precisely enough that agents can
build it, test it, and judge whether the result honours the intent. This is
the reference for format version 1.

Contents

1. [Concepts](#1-concepts)
2. [Document structure](#2-document-structure)
3. [Notation](#3-notation)
4. [The `system` section](#4-the-system-section)
5. [Goals](#5-goals)
6. [Stack](#6-stack)
7. [Database](#7-database)
8. [Interfaces](#8-interfaces)
9. [Tiers and topology](#9-tiers-and-topology)
10. [Modules](#10-modules)
11. [Source (provenance)](#11-source-provenance)
12. [Validation rules](#12-validation-rules)
13. [How agents read the spec](#13-how-agents-read-the-spec)
14. [Language profiles](#14-language-profiles)
15. [Splitting a spec across files](#15-splitting-a-spec-across-files)
16. [System dependencies](#16-system-dependencies)
17. [Writing good specs](#17-writing-good-specs)
18. [Complete skeleton](#18-complete-skeleton)

---

## 1. Concepts

**Intent** is the reason something exists, written the way its owner would
explain it. The system has one, every tier has one, every module has one.
Intent is not used to generate code; it is used to judge it. The Validator
agent compares what was built against the intent to detect code that works
but solves a narrower or different problem.

**Goals** are the outcomes the finished system must achieve. Each goal
declares how it can be verified, and every goal must be owned by at least
one module or scenario: an unowned goal could never be judged, so the
validator refuses it. The final report is a verdict per goal.

**Modules** are units of implementation with their own intent. A module
declares the operations it exposes, the invariants that must always hold,
Given/When/Then scenarios that become tests, the entities it owns and the
interface surfaces it implements or consumes.

**Tiers** are separate deployables in their own languages (a backend
service, a mobile app). They talk only through interfaces. A single-tier
spec omits the `tiers` key entirely.

**Everything that looks like code is notation.** Signatures and field types
are language-neutral so the same spec can be built in Go or Swift; the
language profile decides layout and toolchain.

---

## 2. Document structure

```yaml
aspect: 1            # format version, required
system:              # name, intent, goals, and the shared model
  ...
modules:             # single-tier spec: the modules live here
  - ...
# or
tiers:               # multi-tier spec: language, module_path, stack and
  - ...              # modules live under each tier
```

`modules` and `tiers` are exclusive. A spec with `tiers` declares the
system's shape with `system.topology` and puts `language`, `module_path`
and `stack` on each tier; a spec without `tiers` puts them under `system`.

Unknown keys anywhere are errors, so typos are caught at load time.

---

## 3. Notation

### Identifiers

Names of the system, tiers, modules, interfaces and surfaces match
`[a-z][a-z0-9_]*` (snake_case). Entity names match `[A-Za-z][A-Za-z0-9_]*`
(typically PascalCase). Goal ids are free strings, conventionally `G1`,
`G2`; scenario ids are unique per module, conventionally `S1`, `S2`.

### References

| Reference | Form | Example |
|---|---|---|
| goal | goal id | `goals: [G1, G3]` |
| module | module name | `depends_on: [catalog]` |
| tier | tier name, or `external` | `provider: backend`, `tier: external` |
| entity | entity name | `entities: [Product]` |
| surface | `interface.surface`, or `interface` for all of its surfaces; `system/…` for a dependency's | `surfaces: [api.reserve, app]`, `consumes: [identity/api.login]` |

### Signatures

Operation signatures are notation the Coder translates idiomatically to the
target language. Two forms are accepted:

```
Reserve(sku: string, qty: int, order: string) -> available: int | error   # neutral form
func (s *Stock) Reserve(sku string, qty int, order string) error          # a language's syntax
```

Prefer the neutral form in specs that may be retargeted; `aspect import`
writes it.

### Field types

Entity field types use neutral names: `string`, `int`, `int64`, `float`,
`bool`, `time`, `decimal`, `uuid`, `bytes`, `json`, or the name of another
entity. `json` is a list or structured value stored as one JSON column; say
its shape in the field's `intent` (`list of objections`, `{page, box}`).
The Coder maps them to the language and the database engine, so a
language's own types (`uint`, `time.Time`, `[]string`, a struct that is not
an entity) are warnings: they do not survive a retarget to another
language.

### Contract clauses

`pre`, `post`, `invariants` and `constraints` are free text, but the more
they read as checkable relations between observable values, the better the
tests. `old(x)` in a post-condition means the value of `x` before the call.

---

## 4. The `system` section

| Key | Type | Required | Meaning |
|---|---|---|---|
| `name` | identifier | yes | System name; also the output directory |
| `intent` | text | yes | Why the system exists, in the owner's words |
| `topology` | `monolith` \| `api_backend` \| `cloud_service` | no | Shape of the solution; derived when omitted (see §9) |
| `language` | `go` \| `swift` | single-tier only | Target language (default `go`) |
| `module_path` | string | single-tier only | Go module path, or a bundle-style id for Swift |
| `goals` | list of [Goal](#5-goals) | yes, at least one | Outcomes to achieve |
| `constraints` | list of text | no | Rules every module obeys |
| `stack` | [Stack](#6-stack) | single-tier only | Language-specific configuration |
| `database` | [Database](#7-database) | no | The system's persistent model |
| `interfaces` | list of [Interface](#8-interfaces) | no | How the system is exposed |
| `source` | [Source](#11-source-provenance) | no | Provenance of an imported spec |
| `brief` | path | no | Sidecar document describing the system at length (see [Briefs](#briefs)) |

```yaml
system:
  name: inventory
  intent: >
    Keep an accurate count of physical stock per SKU so that staff never
    promise goods that are not on the shelf.
  language: go
  module_path: example.com/inventory
  goals: [...]
  constraints:
    - Standard library only.
```

---

## 5. Goals

| Key | Type | Required | Meaning |
|---|---|---|---|
| `id` | string | yes | Unique id, referenced by modules and scenarios |
| `statement` | text | yes | The outcome, as a product owner would state it |
| `verify` | `test` \| `invariant` \| `review` | no (default `test`) | How the Validator judges it |

The verification method sets the standard of evidence:

| `verify` | Achieved when |
|---|---|
| `test` | a test genuinely exercising the goal exists and the real test run passed |
| `invariant` | a property-style test exists **and** the Validator's reading of the code finds no path (errors, concurrency, overflow) that breaks it |
| `review` | the Validator can cite code that supports the claim; for qualities no test can execute, such as "the API is idiomatic" |

A failed test run caps every `test` and `invariant` goal below `achieved`,
whatever the model would otherwise say. Goals a module never reached a
verdict on are reported as `unverifiable`, never silently omitted.

```yaml
goals:
  - id: G1
    statement: Stock levels never go negative, whatever sequence of operations is applied.
    verify: invariant
  - id: G2
    statement: A reservation holds goods for an order so they cannot be sold twice.
    verify: test
  - id: G3
    statement: The public API is small, idiomatic Go with no global mutable state.
    verify: review
```

---

## 6. Stack

The stack is keyed by language; only the entry matching the tier's language
is used. It tells agents which libraries they may use and how.

```yaml
stack:
  go:
    version: "1.26"
    frameworks:
      - name: qor5
        module: github.com/qor5/admin/v3
        version: v3.0.0
        purpose: Admin UI with CRUD presets over GORM models.
        guidance: |
          Build the admin with presets.New(). Register each entity with
          b.Model(&Entity{}) ...
    orm:
      name: gorm
      module: gorm.io/gorm
    allowed_modules:
      - gorm.io/driver/
      - github.com/go-chi/chi/v5
    guidance: |
      One *gorm.DB is opened in main and injected; no package-level
      database variables.
```

| Key | Type | Meaning |
|---|---|---|
| `version` | string | Toolchain version; Go writes it into `go.mod` |
| `frameworks[]` | list of Framework | Libraries the system is built on; interfaces reference them by `name` |
| `orm` | Framework | The ORM, when the database is accessed through one |
| `allowed_modules` | list of import prefixes | Extra imports permitted beyond the standard library, the tier's own module, and the frameworks' modules |
| `guidance` | text | Conventions for every module in this language |

Framework fields: `name` (required, unique), `module` (required: import
path in Go, package URL in Swift), `version`, `purpose`, `guidance`. A
framework's guidance is shown to the agents working on any surface that
names the framework.

**The allowlist is enforced mechanically.** For Go, generated files are
parsed and any import outside the standard library, the tier's own module
path and the stack is fed back to the Coder as a failed build. Swift relies
on the generated `Package.swift`, which only declares stack packages.

---

## 7. Database

```yaml
database:
  engine: postgres          # postgres | mysql | sqlite
  tier: backend             # multi-tier: owning tier, or external
  migrations: auto          # auto (ORM migrates at startup) | files
  test:
    engine: sqlite          # what generated tests run against; sqlite = in-memory
    dsn_env: APP_TEST_DSN   # when set at test time, tests use this DSN instead
  entities:
    - name: Product
      intent: A sellable item identified by its SKU.
      fields:
        - {name: ID, type: int64, key: primary}
        - {name: SKU, type: string, required: true, unique: true}
        - {name: OnHand, type: int, default: "0", intent: units physically on the shelf}
      relations:
        - {kind: has_many, entity: Movement, via: ProductID}
      constraints:
        - OnHand >= 0 at all times.
```

| Key | Type | Required | Meaning |
|---|---|---|---|
| `engine` | `postgres` \| `mysql` \| `sqlite` | yes | Production engine |
| `tier` | tier name or `external` | multi-tier: yes | Who owns the database. `external` means a hosted service owns it and the entities only document the remote model |
| `migrations` | `auto` \| `files` | no (default `auto`) | Schema management |
| `test.engine` | engine | no (default `sqlite`) | Engine the generated tests use |
| `test.dsn_env` | string | no | Environment variable carrying a DSN that overrides the test engine |
| `entities[]` | list of Entity | yes, at least one | Persistent types |

Entity: `name` (required, unique across every database in the spec),
`intent`, `fields` (at least one), `relations`, `constraints`.

Field: `name`, `type` (required, neutral names), `key: primary`, `required`,
`unique`, `default`, `intent`. An entity without a primary key gets a
surrogate id from the Coder, with a warning.

Relation: `kind` (`has_one`, `has_many`, `belongs_to`, `many_to_many`),
`entity` (must exist in the same database), `via` (the field or join table
carrying it), `intent`.

**Ownership.** Every entity of a generated database is owned by exactly one
module in the database's tier, listed under that module's `entities`. The
owner defines the persistent type, its schema or migrations, and every
write path; other modules read through the owner's interface. Entities of
an external database are owned by nobody.

**Tier-local databases.** A tier may carry its own `database` (an app's
offline cache). Its entities follow the same rules within that tier. Entity
names stay unique across the system database and every tier database.

---

## 8. Interfaces

An interface is one way the system is exposed to people or programs. Its
surfaces are the individual endpoints, pages, page events, commands, RPCs,
screens, background jobs or AI tools: things something outside a module can
reach. A function other packages call in-process is not a surface; it is an
operation in its module's `interface`.

```yaml
interfaces:
  - name: api
    kind: http
    intent: What the shop calls.
    provider: backend                 # multi-tier: serving tier, or external
    auth: bearer token from SHOP_API_TOKEN
    surfaces:
      - name: reserve
        method: POST
        route: /api/reservations
        entity: Reservation
        request: '{"sku": string, "qty": int, "order": string}'
        response: '{"available": int}'
        errors: ["404 unknown SKU", "409 when fewer than qty are available"]
  - name: admin
    kind: web
    intent: Back office for warehouse staff.
    provider: backend
    framework: qor5
    surfaces:
      - name: products
        route: /admin/products
        entity: Product
        operations: [list, create, update, delete]
      - name: restock                 # a page event, dispatched on its page
        route: /admin/products
        event: restockProduct
        entity: Product
        operations: [restock]
      - {name: require_staff, route: /admin/*}   # middleware: the pattern it wraps
  - name: jobs
    kind: job
    intent: Work the queue runs on its own.
    provider: backend
    surfaces:
      - {name: nightly_count, route: Nightly Stock Count}   # the registered job name
  - name: assistant
    kind: mcp
    intent: Tools the support assistant calls.
    provider: backend
    surfaces:
      - {name: stock_level, route: get_stock_level}         # the tool name
  - name: push
    kind: http
    intent: Notifications.
    provider: external
    service: Firebase Cloud Messaging
    surfaces:
      - {name: send, method: POST, route: /v1/send}
```

| Key | Type | Required | Meaning |
|---|---|---|---|
| `name` | identifier | yes | Unique interface name |
| `kind` | `http` \| `web` \| `cli` \| `grpc` \| `app` \| `job` \| `mcp` | yes | `http`: endpoints called by programs; `web`: server-rendered pages and their events; `cli`: commands; `grpc`: RPC services; `app`: native screens; `job`: background jobs a queue or scheduler runs; `mcp`: tools and resources an AI client calls |
| `intent` | text | yes | Who it serves and why |
| `provider` | tier name or `external` | multi-tier: yes | The tier that serves it; `external` for a service outside the spec. Empty in a single-tier spec means the tier itself |
| `service` | string | with `external` | Name of the hosted provider (Supabase, Firebase, an existing API) |
| `framework` | framework name | no | Must be declared in the providing tier's stack |
| `auth` | text | no | Who may use it; surfaces may override |
| `surfaces[]` | list of Surface | yes, at least one | The individual surfaces |

Surface fields:

| Key | Meaning | Required for |
|---|---|---|
| `name` | identifier, unique within the interface | all |
| `intent` | what the surface is for | |
| `route` | path starting with `/` (`http`, `web`; middleware takes the pattern it wraps, `/*`), command line (`cli`), RPC name (`grpc`), navigation path (`app`), registered job name (`job`), tool or resource name (`mcp`) | `http`, `web`, `grpc`, `app`, `job`, `mcp` |
| `method` | HTTP method | `http` |
| `event` | name of a page event the surface handles (a QOR5 web event, a form action); `route` is the page that dispatches it | only on `web` |
| `entity` | entity the surface operates on, when it is a CRUD surface | |
| `operations` | `list`, `create`, `read`, `update`, `delete` or a lowercase custom verb (`close`, `rearm`), for entity-bound surfaces; never the handler's name | |
| `request`, `response` | shapes, free text | |
| `errors` | error cases with their status or meaning | |
| `auth` | override of the interface's auth | |

**Implementing and consuming.** Modules in the providing tier implement
surfaces by listing them under `surfaces`. Modules in other tiers, and any
module for an external interface, list what they call under `consumes`.
Every provided surface is implemented by exactly one module; external
surfaces are never implemented, only consumed.

---

## 9. Tiers and topology

`system.topology` names the shape of the solution:

| Topology | Shape | Typical use |
|---|---|---|
| `monolith` | one tier owns the database and serves every interface | a web app connecting directly to its database |
| `api_backend` | a backend tier owns the database and serves an API; a frontend tier (web or mobile app) consumes it | a Go service plus an iOS app, or a web frontend over a backend service |
| `cloud_service` | one generated app tier; the database and server interfaces are provided by a hosted service | a mobile app over Supabase, Firebase or an existing hosted API |

When omitted, the topology is derived: several tiers mean `api_backend`,
an external provider or database means `cloud_service`, otherwise
`monolith`. When declared, it is checked against the spec's shape.

### Single-tier specs

`monolith` and `cloud_service` are single-tier: `language`, `module_path`,
`stack` and `modules` sit at the top. A `cloud_service` spec differs only in
that its server-side interfaces and its database carry `provider: external`
and `tier: external`. See
[examples/monolith_minimal](https://github.com/eideroliveira/aspect/blob/main/examples/monolith_minimal/aspect.yaml) and
[examples/cloud_service_minimal](https://github.com/eideroliveira/aspect/blob/main/examples/cloud_service_minimal/aspect.yaml).

### Multi-tier specs

```yaml
system:
  name: shop
  topology: api_backend
  database: {engine: postgres, tier: backend, entities: [...]}
  interfaces:
    - {name: api, kind: http, provider: backend, surfaces: [...]}
    - {name: app, kind: app, provider: mobile, surfaces: [...]}
tiers:
  - name: backend
    intent: Own the data and serve the API.
    language: go
    module_path: example.com/shop
    stack: {go: {...}}
    modules:
      - {name: catalog, entities: [Product]}
      - {name: api, depends_on: [catalog], surfaces: [api]}
  - name: mobile
    intent: The members' iOS app.
    language: swift
    module_path: com.example.shop
    depends_on: [backend]
    database: {engine: sqlite, entities: [{name: CachedProduct, ...}]}
    modules:
      - {name: api_client, consumes: [api]}
      - {name: catalog_feature, depends_on: [api_client], entities: [CachedProduct], surfaces: [app]}
```

| Tier key | Type | Required | Meaning |
|---|---|---|---|
| `name` | identifier | yes | Unique; `external` is reserved |
| `intent` | text | yes | What this deployable is for |
| `language` | `go` \| `swift` | no (default `go`) | Target language of this tier |
| `module_path` | string | yes | Module or package path for this tier |
| `depends_on` | list of tier names | no | Build order: a consumer after its provider |
| `stack` | Stack | no | This tier's language configuration |
| `database` | Database | no | A tier-local database (cache) |
| `modules` | list of Module | yes, at least one | The tier's modules |
| `brief` | path | no | Sidecar document for the tier (see [Briefs](#briefs)) |

Rules that hold across tiers: module names are unique across the whole
spec; `depends_on` between modules never crosses a tier (cross-tier calls go
through `consumes`); tier dependencies are acyclic. Each tier is generated
into its own workspace, `out/<system>/<tier>/`, in dependency order. A
single-tier spec keeps `out/<system>/`.

Full example: [examples/shop_two_tier](https://github.com/eideroliveira/aspect/blob/main/examples/shop_two_tier/aspect.yaml).

---

## 10. Modules

```yaml
modules:
  - name: stock
    intent: >
      Answer "how many of X can we promise right now?" and enforce that no
      movement can take a SKU below zero. Delegates the audit trail to ledger.
    goals: [G1, G3]
    depends_on: [ledger]
    entities: [Product]
    surfaces: [api.reserve, admin]
    consumes: [push.send]
    interface:
      - name: Reserve
        signature: "Reserve(sku: string, qty: int, order: string) -> error"
        intent: Hold goods for an order. Fails if fewer than qty are available.
        pre: ["qty > 0"]
        post: ["Available(sku) == old(Available(sku)) - qty"]
    invariants:
      - Available(sku) == on-hand(sku) - reserved(sku) at all times.
    scenarios:
      - id: S2
        given: Receive("A", 5)
        when: Reserve("A", 6, "order-1")
        then: an error is returned and Available("A") == 5
        goals: [G1]
    constraints:
      - Safe for concurrent use from multiple goroutines.
```

| Key | Type | Required | Meaning |
|---|---|---|---|
| `name` | identifier | yes | Unique across the spec; also the code directory unless `path` is set |
| `path` | slash path | no | Go only: the module's package directory relative to the tier's code root (`.` for the root). Written by `aspect import`; see [Module paths](#module-paths) |
| `intent` | text | yes | What the module is for, not how it works |
| `goals` | goal ids | no | Goals this module is accountable for |
| `depends_on` | module names | no | Same-tier modules whose code this one imports; acyclic |
| `entities` | entity names | no | Entities this module owns (defines and writes) |
| `surfaces` | surface refs | no | Surfaces this module implements; only for its own tier's interfaces |
| `consumes` | surface refs | no | Surfaces this module calls in other tiers, external services, or system dependencies (`system/interface.surface`) |
| `interface` | list of Operation | no | Exported operations other modules call |
| `invariants` | list of text | no | Properties that hold in every state |
| `scenarios` | list of Scenario | no | Given/When/Then examples |
| `constraints` | list of text | no | Module-specific rules |
| `brief` | path | no | Sidecar document for the module (see [Briefs](#briefs)) |

Operation: `name` and `signature` (both required), `intent`, `pre`, `post`.
Signatures are the contract dependents are generated against; keep them
stable.

Scenario: `id` (unique within the module), `given`, `when` and `then`
(required), `goals`. Every scenario becomes at least one test whose name
carries the scenario id, and the Validator checks that the test does what
the scenario says. Scenarios may pin goals directly, which is how a goal
can be owned by a scenario rather than a whole module.

A module with neither scenarios nor invariants is accepted with a warning:
its tests are inferred from the interface alone, which is weak evidence.

### Module paths

A module's code lives where the language profile says: `<name>/` in Go.
An imported module cannot always be found that way, because a package
directory does not survive its turn into an identifier: `modules/api`,
`modules_api` and `modules/api_` all become the module `modules_api`, and the
root package becomes `root`. So `aspect import` records each package's
directory as the module's `path`:

```yaml
modules:
  - name: modules_api
    path: modules/api
  - name: root
    path: .
```

A module with a `path` is exactly one package. It owns the Go files in that
directory and nothing below it (subpackages are other modules, or code no
module claims), and its build and test steps run on that package only
(`go test ./modules/api`, `go test .` for the root). A module without a
`path` owns its directory and everything beneath it, as before, so a
module with a `path` may not sit inside such a directory. Paths are clean
relative slash paths, unique within a tier; they are valid only in Go
tiers.

A spec imported before `path` existed can get the field back from the
repository with `aspect paths <spec> -repo <dir> -w`, which matches each
module name against the packages on disk and writes `path:` under the
module's `name:` in whichever file defines it. A module that no package
maps to, that several do, or whose package another module already holds,
is reported and nothing is written. Tiers in other languages are skipped.

### Briefs

A brief is a sidecar Markdown file holding a longer, free-form description
of what a system, tier or module must be: domain rules, seams with other
parts, the history behind a design, anything that would crowd the YAML. It
is referenced by path relative to the spec file and loaded with it; a
missing file fails the load, naming the owner.

```yaml
modules:
  - name: translation
    path: modules/translation
    intent: Orchestrate curriculum translation runs and their publication.
    brief: ../modules/translation/CLAUDE.md
```

The file may be anywhere in the repository that holds the spec (the nearest
directory at or above the spec's that has a `.git`; the spec's own directory
when there is none), so a package's own `CLAUDE.md` or `README.md` can be
its module's brief in place: one document that people, coding assistants
and the agents all read, instead of a copy beside the spec that drifts from
it. A path that leads out of the repository, directly or through a symbolic
link, fails the load and the file is never read: a brief is sent to a
model. In an included fragment the path is relative to the fragment, like
every path there.

Every agent working on the owner sees the whole text under a "brief"
heading: the Coder and Tester build to it, the Validator judges against it.
Where a brief is more specific than the spec it is binding; where the two
conflict, agents are told to raise the conflict in their concerns rather
than choose silently. `aspect import` turns Markdown documents found in a
package directory (README.md, CLAUDE.md, design notes) into
`briefs/<module>.md` next to the recovered spec and references them.
`aspect briefs <spec> -w` later points every placed module whose package has
a `CLAUDE.md` at that file instead of its copy, says which copies had
already drifted from it and which held other documents, and lists the
copies no longer referenced. A brief someone wrote is left alone.

### Reading the spec of the code at hand

`aspect context <spec> <path|module>...` prints what the spec says about
the modules owning the given files or directories (a directory stands for
the files directly in it), or the modules named: the system's constraints,
then each module's intent, goals, owned entities, constraints and
invariants, then its operations and scenarios, then its brief. `-budget N`
caps it at about N tokens, in that order of priority, and names what it left
out. It is the same text `aspect review` shows each reviewer about the
modules its files belong to (section 13), so a project can keep package
rules in the spec, have them enforced by the review, and still put them in
front of whoever edits the package: a coding assistant's instructions can
say "run `aspect context` before changing a package" rather than carry the
rules themselves.

---

## 11. Source (provenance)

Written by `aspect import`; informational.

```yaml
source:
  language: go
  repository: git@github.com:org/repo.git
  commit: 2a630a7d
  imported_at: 2026-09-09T20:00:00Z
  frameworks: [chi, gorm, qor5]
  exclude: [external, cmd]
```

`frameworks` keeps what the source used even when the target language's
stack cannot carry them, so a retargeted spec remembers where its shape
came from. `exclude` is the import's `-exclude` list (directory names, or
relative paths and everything below them): `aspect drift` does not report
code there as unclaimed, since the spec never meant to describe it.

---

## 12. Validation rules

`aspect validate` reports every issue in one pass. Errors block `run`;
warnings do not.

### Errors

Structure
- `aspect` is `1`; `system.name` and `system.intent` are set; at least one goal.
- `modules` and `tiers` are exclusive. Without tiers, `system.language` is supported and `system.module_path` is set, and there is at least one module.
- Every stack framework has `name` (unique) and `module`; the ORM has `module`; `allowed_modules` entries are non-empty.
- Every dependency has a unique identifier `name` (not a tier name, not `external`) and a relative `spec`; a dependency's spec must itself validate; dependency and include cycles fail the load.
- A surface of a dependency can be consumed, never implemented.

Identifiers and references
- Names match their pattern (§3); goal ids, entity names (across all databases), interface names, tier names, and module names (across all tiers) are unique; scenario ids are unique per module; surface names are unique per interface.
- Every goal, module, tier, entity and surface reference resolves.

Goals
- Every goal is owned by at least one module or scenario.

Database
- `engine`, `migrations`, `test.engine` and relation kinds use the listed values; every entity has fields; field `key` is empty or `primary`; relation targets exist in the same database.
- Multi-tier: `system.database.tier` is a tier name or `external`. Single-tier: it may only be empty or `external`.
- Every entity of a generated database is owned by exactly one module, in the database's tier. Entities of an external database are owned by nobody.

Interfaces
- `kind` is one of the listed kinds; `http` surfaces have `route` and `method`; `web`, `grpc`, `app`, `job` and `mcp` surfaces have `route`; only `web` surfaces have an `event`; every interface has at least one surface; surface entities exist; `framework` is declared in the providing tier's stack.
- Multi-tier: `provider` is a tier name or `external`. Single-tier: it may only be empty or `external`.
- Every provided surface is implemented by exactly one module of the providing tier. External surfaces are never implemented.
- `consumes` never names a surface of the module's own tier.

Tiers
- Every tier has `name`, `intent`, a supported `language`, `module_path` and modules; `external` is not a tier name; tier dependencies resolve and are acyclic.
- Module `depends_on` resolves, is not the module itself, stays inside the tier, and is acyclic.
- Module `path` is a clean relative slash path that stays inside the code root, is used by one module of the tier only, appears only in Go tiers, and does not lie inside the directory of a module laid out by name (`<name>/` owns everything below it).

Topology
- A declared `api_backend` has at least two tiers and at least one module consuming a surface provided by another tier.
- A declared `cloud_service` has an external interface or an external database.

### Warnings

- A module owns no goal (the Validator will only judge its intent).
- A module has neither scenarios nor invariants.
- A stack entry for a language other than the tier's.
- An entity without an intent or without a primary key.
- A non-sqlite test engine without `dsn_env`.
- An external interface without a `service` name; an external surface nobody consumes.
- `system.language`, `module_path` or `stack` set alongside `tiers`; a `tier` field on a tier-local database; a single tier written under `tiers`.
- A `brief` that was not loaded (spec parsed from memory) or is empty. A brief path that is absolute or leads out of the repository (out of the spec's directory for a spec parsed from memory) is an error.
- A dependency that was not loaded (spec parsed from memory).
- `operations` listed on a surface with no `entity`; an operation that is not a lowercase verb (a function name such as `ExportCSVHandler`).
- An `http` or `web` route that is not a path (`event:Save`, `tool:x`, `(in-process)`).
- An entity field type that is neither neutral nor an entity (`uint`, `time.Time`, `[string]`).
- A declared `monolith` with several tiers or with external providers.

---

## 13. How agents read the spec

| Section | Coder | Tester | Validator |
|---|---|---|---|
| system intent, goals, constraints | context | context | the standard to judge against |
| tiers (outline) | knows which tier it is in and who provides what | same | same |
| stack (own tier) | which libraries to use and how; enforced by the import guard | same | checks frameworks are used as their guidance says |
| database (outline) + owned entities (full) | defines types, schema, write paths | tests persistence against the test database | checks fields, keys, relations, and that no write path exists elsewhere |
| interfaces (outline) + implemented surfaces (full) | wires routes, pages, commands, screens exactly as specified | exercises them as a client would | checks routes, methods, shapes, errors, auth |
| consumed surfaces (full) | implements a client against the contract, base address from configuration | stubs the provider | checks the client honours every listed error |
| brief (system, tier, module) | builds to it; raises conflicts with the YAML in concerns | derives tests from its rules | judges against it |
| module interface, pre/post | implements the operations | derives tests from the clauses | cites the code that makes them true |
| invariants | must hold on every path | property-style tests | reads paths tests cannot reach |
| scenarios | happy and error paths to support | one test per scenario, named after its id | confirms each named test does what the scenario says |

The Validator is a different agent from the Coder and Tester, sees the real
test run, and must cite files and identifiers as evidence. Goals it forgets
become `unverifiable`; a goal owned by several modules rolls up to its
weakest verdict.

The review agents (`aspect review`) have no tools, so the spec reaches them
in their prompt: each is shown the system's constraints and, for the modules
its files belong to, their intent, goals, constraints, invariants,
operations, scenarios and brief, in that order of priority, within
`CODE_REVIEWER_SPEC_BUDGET` tokens (`aspect context` prints the same text).
A file belongs to a module when the module owns it as code (its `path`, or
its directory by name), or when it is the module's brief or the fragment the
module is written in. A rule stated in a module's constraints or invariants
is therefore checked on every change to that module; one kept only in a
document no module references is not.

---

## 14. Language profiles

The spec is language-agnostic; a profile decides the layout and toolchain
of each tier.

| Language | Layout | Manifest | Build and test |
|---|---|---|---|
| `go` | `<module>/` as package `<module>`, or the module's `path`; tests `*_test.go` beside the code | `go.mod` written once (version from the stack) | `go mod tidy`, `go vet`, `go test -race` (on `./<module>/...`, or on the one package at `path`) |
| `swift` | `Sources/<Module>/` and `Tests/<Module>Tests/` (PascalCase of the module name) | `Package.swift` regenerated after every module; platforms iOS 17 and macOS 14 so tests run on the host; stack frameworks become package dependencies | `swift build`, `swift test --filter <Module>Tests` |

---

## 15. Splitting a spec across files

A spec can be spread over a directory. Anywhere a list is expected, an
entry may pull in another file:

```yaml
system:
  database:
    file: database.yaml            # a mapping in place of a mapping
  interfaces:
    - file: interfaces/api.yaml    # one entry (a mapping) or several (a list)
    - file: interfaces/admin.yaml
modules:
  - dir: modules                   # every *.yaml in the directory, by name
  - file: extra/reporting.yaml
```

| Directive | Where | Effect |
|---|---|---|
| `- file: path` | in a list | the file's content is spliced in: a list contributes all its entries, a mapping one entry |
| `- dir: path/` | in a list | every `*.yaml` and `*.yml` in the directory, sorted by file name, each spliced as above |
| `key: {file: path}` | in place of a mapping | the file's mapping replaces the directive |

Paths are relative to the file that contains the directive; included files
may include further files. Names sort, so `10_catalog.yaml` comes before
`20_api.yaml` and dependency order inside a directory is under your
control. `brief` and dependency `spec` paths written inside an included
file are relative to that file and are rebased automatically. Cycles and
missing files fail the load. `aspect expand spec.yaml` prints the
assembled document, which is what the loader validates, so unknown keys
in fragments are still caught. See
[examples/modular_shop](https://github.com/eideroliveira/aspect/blob/main/examples/modular_shop/aspect.yaml).

### One file per module: `aspect split`

A spec that grew in one file (an imported one, typically) can be split
mechanically, so a change to one package touches one small file and a
review reads only that file:

```sh
aspect split _aspect/aspect.yaml -n   # list the files and their sizes, write nothing
aspect split _aspect/aspect.yaml      # write them
aspect import . -split                # or import straight into this layout
```

```
_aspect/
  aspect.yaml              system, goals, constraints, stack, database settings
  interfaces/<name>.yaml   one interface with its surfaces
  entities/<module>.yaml   the entities that module owns (_unowned.yaml: no owner)
  modules/<name>.yaml      one module
  tiers/<tier>/modules/, tiers/<tier>/entities/   the same, per tier
  briefs/                  unchanged; fragments point at them with ../briefs/
```

The entry point pulls each directory in with `- dir:`, so a new module is a
new file and nothing else. Comments and key order are kept. Modules,
interfaces and entities come back sorted by file name, which changes
nothing: build order comes from `depends_on`. Before writing, `split` loads
its own result and compares it with the original; it refuses a spec that
already uses includes, names that cannot be file names (or differ only in
case), and a target directory that already holds YAML.

Validation issues name the file and line they come from, through includes:

```
_aspect/modules/cart.yaml:41: error: modules[12].scenarios[3].goals[0]: unknown goal "G9"
```

## 16. System dependencies

A system may consume interfaces of other Aspect systems. They are declared
with the path of their own spec and referenced in surface notation with
the system name as prefix:

```yaml
system:
  dependencies:
    - name: identity
      spec: ../identity/aspect.yaml
      intent: The shared login service; the shop trusts its tokens.
modules:
  - name: api
    consumes: [identity/api.whoami, identity/api]   # one surface, or all of an interface
```

| Key | Type | Required | Meaning |
|---|---|---|---|
| `name` | identifier | yes | Prefix used in references; unique, not a tier name, not `external` |
| `spec` | relative path | yes | The dependency's spec, loaded and validated with this one |
| `intent` | text | no | Why this system relies on it |

The dependency's spec is loaded with this one (includes, briefs and its
own dependencies included) and must validate: a system cannot depend on
one that does not hold together. Dependency cycles fail the load. A
dependency is never built by this system's run; its surfaces can only be
consumed, and agents see their full contracts with the system named. This
is how a family of systems shares one identity service or one catalog
without any of them copying the other's interface definitions.

Compared with `provider: external`, a dependency has a checked contract:
references into it are validated against the real spec, and the contract
shown to agents is the one the other system is built from.

## 17. Writing good specs

- **Intent is for judgement, not generation.** Write it the way you would
  explain the module to a new colleague. Vague intent yields vague verdicts.
- **Scenarios are the tests.** Concrete values beat prose: `Reserve("A", 6)`
  after `Receive("A", 5)` is testable; "reserving too much fails" is not.
- **Invariants catch what scenarios miss.** State them as relations between
  observable values so a property test can check them after every step.
- **Use `review` sparingly.** It is for qualities no test can execute.
- **Signatures are the contract between modules.** Dependents are generated
  against them; keep them stable and neutral.
- **Give every entity and surface one owner.** The validator insists, and
  it is what makes drift detectable: a module touching an entity it does
  not own contradicts what it was shown.
- **Split tiers where the deployment splits.** A mobile app cannot open the
  database; make the API a provided interface and let the app consume it.
- **Put framework knowledge in `guidance`.** It is shown to every agent that
  touches a surface using the framework; it is the cheapest way to get
  idiomatic code.

---

## 18. Complete skeleton

Every key of the format in one place. Optional keys are marked `#opt`.

```yaml
aspect: 1
system:
  name: identifier
  intent: text
  topology: monolith | api_backend | cloud_service      #opt
  language: go | swift                                  # single-tier
  module_path: string                                   # single-tier
  goals:
    - {id: string, statement: text, verify: test | invariant | review}
  constraints: [text]                                   #opt
  stack:                                                #opt, single-tier
    <language>:
      version: string                                   #opt
      frameworks:                                       #opt
        - {name: string, module: string, version: string, purpose: text, guidance: text}
      orm: {name: string, module: string}               #opt
      allowed_modules: [import-prefix]                  #opt
      guidance: text                                    #opt
  database:                                             #opt
    engine: postgres | mysql | sqlite
    tier: tier-name | external                          # multi-tier
    migrations: auto | files                            #opt
    test: {engine: engine, dsn_env: string}             #opt
    entities:
      - name: Entity
        intent: text                                    #opt
        fields:
          - {name: string, type: neutral-type, key: primary, required: bool, unique: bool, default: string, intent: text}
        relations:                                      #opt
          - {kind: has_one | has_many | belongs_to | many_to_many, entity: Entity, via: string, intent: text}
        constraints: [text]                             #opt
  interfaces:                                           #opt
    - name: identifier
      kind: http | web | cli | grpc | app | job | mcp
      intent: text
      provider: tier-name | external                    # multi-tier, or external
      service: string                                   # with external
      framework: framework-name                         #opt
      auth: text                                        #opt
      surfaces:
        - name: identifier
          intent: text                                  #opt
          route: string
          method: GET | POST | ...                      # http
          entity: Entity                                #opt
          operations: [list, create, read, update, delete, ...]   #opt
          request: text                                 #opt
          response: text                                #opt
          errors: [text]                                #opt
          auth: text                                    #opt
  source:                                               #opt, written by import
    {language: string, repository: string, commit: string, imported_at: string, frameworks: [string], exclude: [string]}
  brief: relative/path.md                               #opt
  dependencies:                                         #opt, other Aspect systems
    - {name: identifier, spec: relative/aspect.yaml, intent: text}

modules:                                                # single-tier
  - &module
    name: identifier
    path: relative/dir                                  #opt, Go only; written by import
    intent: text
    goals: [goal-id]                                    #opt
    depends_on: [module-name]                           #opt, same tier
    entities: [Entity]                                  #opt
    surfaces: [interface.surface | interface]           #opt, own tier
    consumes: [interface.surface | interface | system/interface.surface]   #opt
    interface:                                          #opt
      - {name: string, signature: string, intent: text, pre: [text], post: [text]}
    invariants: [text]                                  #opt
    scenarios:                                          #opt
      - {id: string, given: text, when: text, then: text, goals: [goal-id]}
    constraints: [text]                                 #opt
    brief: relative/path.md                             #opt

tiers:                                                  # multi-tier, instead of modules
  - name: identifier
    intent: text
    language: go | swift
    module_path: string
    depends_on: [tier-name]                             #opt
    stack: {<language>: {...}}                          #opt
    database: {...}                                     #opt, tier-local
    brief: relative/path.md                             #opt
    modules: [*module]
```
