package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustLoad(t *testing.T, path string) *Spec {
	t.Helper()
	s, err := Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return s
}

func TestLoadExample(t *testing.T) {
	s := mustLoad(t, "testdata/inventory.yaml")
	if s.System.Name != "inventory" {
		t.Fatalf("system.name = %q", s.System.Name)
	}
	if got := len(s.Modules); got != 2 {
		t.Fatalf("modules = %d, want 2", got)
	}
	if s.System.Language != "go" {
		t.Fatalf("default language not applied: %q", s.System.Language)
	}
	if issues := Validate(s); issues.HasErrors() {
		t.Fatalf("example spec must validate cleanly, got:\n%s", join(issues))
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte("aspect: 1\nsystem:\n  name: x\n  intents: nope\n"))
	if err == nil || !strings.Contains(err.Error(), "intents") {
		t.Fatalf("want unknown-field error naming `intents`, got %v", err)
	}
}

func TestValidateFindsEveryProblemAtOnce(t *testing.T) {
	src := `
aspect: 2
system:
  name: Bad-Name
  intent: ""
  module_path: ""
  goals:
    - id: G1
      statement: dup
    - id: G1
      statement: dup again
      verify: magic
    - id: G9
      statement: nobody owns me
modules:
  - name: a
    intent: a
    goals: [G1, GX]
    depends_on: [b]
  - name: b
    intent: b
    goals: [G1]
    depends_on: [a]
    scenarios:
      - id: S1
        when: x
      - id: S1
        when: x
        then: y
`
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	issues := Validate(s)
	want := []string{
		"unsupported spec version 2",
		`"Bad-Name" must match`,
		"system.intent: is required",
		"system.module_path: is required",
		"duplicate goal id \"G1\"",
		`"magic" is not one of`,
		`unknown goal "GX"`,
		"dependency cycle: a -> b -> a",
		`goal "G9" is not owned`,
		"scenario needs at least `when` and `then`",
		`duplicate scenario id "S1"`,
	}
	text := join(issues)
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("missing issue containing %q\nall issues:\n%s", w, text)
		}
	}
	if !issues.HasErrors() {
		t.Error("HasErrors() = false")
	}
}

func TestValidateWarnsWithoutBlocking(t *testing.T) {
	src := `
aspect: 1
system:
  name: sys
  intent: do things
  module_path: example.com/sys
  goals:
    - id: G1
      statement: works
modules:
  - name: core
    intent: core
    scenarios:
      - id: S1
        when: call
        then: ok
        goals: [G1]
`
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	issues := Validate(s)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", join(issues))
	}
	if len(issues) == 0 || issues[0].Severity != Warning {
		t.Fatalf("want a warning about the module owning no goal, got:\n%s", join(issues))
	}
}

func join(is Issues) string {
	var b strings.Builder
	for _, i := range is {
		b.WriteString(i.String())
		b.WriteByte('\n')
	}
	return b.String()
}

func TestValidateStackDatabaseInterfaces(t *testing.T) {
	src := `
aspect: 1
system:
  name: shop
  intent: sell things
  module_path: example.com/shop
  goals:
    - {id: G1, statement: products persist}
  stack:
    go:
      frameworks:
        - {name: qor5, module: github.com/qor5/admin/v3, purpose: admin UI}
      orm: {name: gorm, module: gorm.io/gorm}
      allowed_modules: [github.com/theplant/]
    python:
      frameworks: []
  database:
    engine: postgres
    entities:
      - name: Product
        intent: a sellable item
        fields:
          - {name: ID, type: uint, key: primary}
          - {name: SKU, type: string, unique: true, required: true}
        relations:
          - {kind: has_many, entity: Movement}
      - name: Movement
        fields:
          - {name: ID, type: uint, key: primary}
          - {name: Qty, type: int}
  interfaces:
    - name: admin
      kind: web
      intent: back office
      framework: qor5
      surfaces:
        - {name: products, route: /admin/products, entity: Product, operations: [list, create]}
    - name: api
      kind: http
      intent: integrations
      surfaces:
        - {name: reserve, route: /api/reserve, method: POST}
        - {name: broken}
modules:
  - name: catalog
    intent: own products
    goals: [G1]
    entities: [Product]
    surfaces: [admin]
    scenarios:
      - {id: S1, when: create, then: exists}
  - name: stock
    intent: own movements
    entities: [Movement]
    surfaces: [api.reserve, api.nope]
    scenarios:
      - {id: S1, when: reserve, then: ok}
`
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if s.System.Database.Migrations != "auto" || s.System.Database.Test.Engine != "sqlite" {
		t.Fatalf("database defaults not applied: %+v", s.System.Database)
	}
	issues := Validate(s)
	text := join(issues)
	for _, want := range []string{
		`system.stack.python: configured for "python"`,
		`entity "Movement" has no intent`,
		`http surfaces need route and method`,
		`surface "api.broken" is not implemented`,
		`unknown surface "nope" in interface "api"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing issue containing %q\nall issues:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{`unknown entity`, `"Product" is not owned`, `framework`} {
		if strings.Contains(text, unwanted) {
			t.Errorf("unexpected issue containing %q\nall issues:\n%s", unwanted, text)
		}
	}
	allowed := s.AllowedImports()
	want := []string{"example.com/shop", "github.com/qor5/admin/v3", "gorm.io/gorm", "github.com/theplant/"}
	if strings.Join(allowed, ",") != strings.Join(want, ",") {
		t.Fatalf("AllowedImports = %v, want %v", allowed, want)
	}
	refs, errs := s.ResolveSurfaces([]string{"admin", "api.reserve"})
	if len(errs) != 0 || len(refs) != 2 || refs[0].ID() != "admin.products" || refs[1].ID() != "api.reserve" {
		t.Fatalf("ResolveSurfaces = %+v, %v", refs, errs)
	}
}

func TestValidateOwnershipConflicts(t *testing.T) {
	src := `
aspect: 1
system:
  name: shop
  intent: sell things
  module_path: example.com/shop
  goals: [{id: G1, statement: x}]
  database:
    engine: mysql
    test: {engine: mysql}
    entities:
      - {name: A, intent: a, fields: [{name: ID, type: int, key: primary}]}
      - {name: B, intent: b, fields: [{name: ID, type: int, key: primary}]}
  interfaces:
    - name: api
      kind: http
      intent: i
      framework: ghost
      surfaces: [{name: x, route: /x, method: GET}]
modules:
  - {name: m1, intent: i, goals: [G1], entities: [A], surfaces: [api.x]}
  - {name: m2, intent: i, entities: [A], surfaces: [api.x]}
`
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	text := join(Validate(s))
	for _, want := range []string{
		`entity "A" is owned by several modules (m1, m2)`,
		`entity "B" is not owned by any module`,
		`surface "api.x" is implemented by several modules`,
		`"ghost" is not declared in the providing tier's stack`,
		`tests run against mysql but no dsn_env is set`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing issue containing %q\nall issues:\n%s", want, text)
		}
	}
}

const twoTier = `
aspect: 1
system:
  name: shop
  intent: sell things to members on the web and on their phones
  topology: api_backend
  goals:
    - {id: G1, statement: members can reserve products, verify: test}
    - {id: G2, statement: the catalog is one source of truth, verify: invariant}
  database:
    engine: postgres
    tier: backend
    entities:
      - {name: Product, intent: a sellable item, fields: [{name: ID, type: int, key: primary}, {name: SKU, type: string, unique: true}]}
  interfaces:
    - name: api
      kind: http
      intent: what the app calls
      provider: backend
      surfaces:
        - {name: reserve, route: /api/reservations, method: POST, entity: Product}
    - name: app
      kind: app
      intent: member screens
      provider: mobile
      surfaces:
        - {name: catalog, route: /catalog, entity: Product}
    - name: push
      kind: http
      intent: notifications
      provider: external
      service: Firebase Cloud Messaging
      surfaces:
        - {name: send, route: /v1/send, method: POST}
tiers:
  - name: backend
    intent: own the data and serve the API
    language: go
    module_path: example.com/shop
    modules:
      - name: catalog
        intent: own products
        goals: [G2]
        entities: [Product]
        scenarios: [{id: S1, when: create, then: exists}]
      - name: api
        intent: serve the app
        goals: [G1]
        depends_on: [catalog]
        surfaces: [api]
        scenarios: [{id: S1, when: POST, then: 201}]
  - name: mobile
    intent: the members' iOS app
    language: swift
    module_path: com.example.shop
    depends_on: [backend]
    database:
      engine: sqlite
      entities:
        - {name: CachedProduct, intent: offline copy, fields: [{name: id, type: int, key: primary}]}
    modules:
      - name: api_client
        intent: talk to the backend
        goals: [G1]
        consumes: [api.reserve, push.send]
        scenarios: [{id: S1, when: reserve, then: ok}]
      - name: catalog_feature
        intent: browse
        goals: [G1]
        depends_on: [api_client]
        entities: [CachedProduct]
        surfaces: [app.catalog]
        scenarios: [{id: S1, when: open, then: list}]
`

func TestValidateTwoTierSpec(t *testing.T) {
	s, err := Parse([]byte(twoTier))
	if err != nil {
		t.Fatal(err)
	}
	issues := Validate(s)
	if issues.HasErrors() {
		t.Fatalf("two-tier spec must validate:\n%s", join(issues))
	}
	if s.EffectiveTopology() != APIBackend || len(s.EffectiveTiers()) != 2 || s.TierOf("api_client").Name != "mobile" {
		t.Fatalf("shape: topology=%s tiers=%d", s.EffectiveTopology(), len(s.EffectiveTiers()))
	}
	if s.Entity("CachedProduct") == nil || s.Module("catalog_feature") == nil {
		t.Fatal("lookups must span tiers and tier-local databases")
	}
	if got := s.Tier("mobile").AllowedImports(); len(got) != 1 || got[0] != "com.example.shop" {
		t.Fatalf("mobile imports = %v", got)
	}
}

func TestValidateTierRules(t *testing.T) {
	src := strings.NewReplacer(
		"depends_on: [catalog]\n        surfaces: [api]", "depends_on: [api_client]\n        surfaces: [api, app.catalog]",
		"consumes: [api.reserve, push.send]", "consumes: [api.reserve, push.send, app.catalog]\n        surfaces: [push.send]",
		"provider: backend\n      surfaces:\n        - {name: reserve", "surfaces:\n        - {name: reserve",
		"tier: backend", "tier: nowhere",
	).Replace(twoTier)
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	text := join(Validate(s))
	for _, want := range []string{
		`system.database.tier: unknown tier "nowhere"`,
		`system.interfaces[0].provider: is required in a multi-tier spec`,
		`module "api_client" is in tier "mobile"; cross-tier dependencies go through interfaces`,
		`surface "app.catalog" is provided by tier "mobile"; only that tier's modules may implement it`,
		`surface "push.send" is served by an external service; consume it instead`,
		`surface "app.catalog" is provided by this tier; depend on the implementing module`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing issue containing %q\nall issues:\n%s", want, text)
		}
	}
}

func TestSingleTierSpecIsAnImplicitTier(t *testing.T) {
	s := mustLoad(t, "testdata/inventory.yaml")
	tiers := s.EffectiveTiers()
	if len(tiers) != 1 || tiers[0].Name != "" || tiers[0].Language != "go" || len(tiers[0].Modules) != 2 {
		t.Fatalf("implicit tier = %+v", tiers[0])
	}
	if s.EffectiveTopology() != Monolith {
		t.Fatalf("topology = %s", s.EffectiveTopology())
	}
	if s.TierOf("stock") == nil || s.TierOf("stock").Name != "" {
		t.Fatal("TierOf must find modules of the implicit tier")
	}
}

func TestLoadReadsBriefSidecars(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "briefs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "briefs", "greet.md"), []byte("# Greet\n\nAlways greet in the person's language.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `
aspect: 1
system:
  name: g
  intent: greet
  brief: briefs/system.md
  module_path: example.com/g
  goals: [{id: G1, statement: x}]
modules:
  - name: greet
    intent: greet
    goals: [G1]
    brief: briefs/greet.md
    scenarios: [{id: S1, when: a, then: b}]
`
	path := filepath.Join(dir, "aspect.yaml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "system: briefs/system.md") {
		t.Fatalf("a missing brief must fail loading with its owner named, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "briefs", "system.md"), []byte("The whole system brief."), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Module("greet").Brief.Text, "person's language") || s.System.Brief.Text != "The whole system brief." {
		t.Fatalf("briefs not loaded: %+v / %+v", s.Module("greet").Brief, s.System.Brief)
	}
	if issues := Validate(s); issues.HasErrors() || len(issues) != 0 {
		t.Fatalf("loaded briefs must validate cleanly: %v", issues)
	}
	// Parsed from memory, the same spec warns that briefs are not loaded.
	mem, _ := Parse([]byte(src))
	if text := join(Validate(mem)); !strings.Contains(text, "module greet.brief") || !strings.Contains(text, "was not loaded") {
		t.Fatalf("want not-loaded warnings, got:\n%s", text)
	}
	mem.Modules[0].Brief.Path = "../escape.md"
	if text := join(Validate(mem)); !strings.Contains(text, "must stay inside the spec's directory") {
		t.Fatalf("want path escape error, got:\n%s", text)
	}
}

// A brief may be a document anywhere in the repository (a package's own
// ASPECT.md), so it is read in place rather than copied beside the spec, but
// never a file outside the repository, directly or through a link.
func TestBriefsInsideTheRepository(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("not for a model"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	for _, d := range []string{".git", "_aspect/modules", "pkg/order"} {
		if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "pkg", "order", "ASPECT.md"), []byte("Orders are never deleted."), 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("_aspect/aspect.yaml", `
aspect: 1
system:
  name: g
  intent: shop
  module_path: example.com/g
  goals: [{id: G1, statement: x}]
modules:
  - dir: modules/
`)
	module := func(brief string) string {
		return "name: order\npath: pkg/order\nintent: orders\ngoals: [G1]\nbrief: " + brief + "\nscenarios: [{id: S1, when: a, then: b}]\n"
	}
	// Written in an included fragment, the path is relative to the fragment.
	write("_aspect/modules/order.yaml", module("../../pkg/order/ASPECT.md"))
	s, err := Load(filepath.Join(repo, "_aspect", "aspect.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := s.Module("order")
	if m.Brief.Path != "../pkg/order/ASPECT.md" || m.Brief.Text != "Orders are never deleted." {
		t.Fatalf("brief = %q %q", m.Brief.Path, m.Brief.Text)
	}
	if issues := Validate(s); issues.HasErrors() {
		t.Fatalf("a brief inside the repository must validate: %v", issues)
	}
	if want, _ := filepath.EvalSymlinks(repo); s.Root != repo && s.Root != want {
		t.Fatalf("root = %q, want %q", s.Root, repo)
	}

	rel, err := filepath.Rel(filepath.Join(repo, "_aspect", "modules"), filepath.Join(outside, "secret.md"))
	if err != nil {
		t.Fatal(err)
	}
	write("_aspect/modules/order.yaml", module(filepath.ToSlash(rel)))
	if _, err := Load(filepath.Join(repo, "_aspect", "aspect.yaml")); err == nil || !strings.Contains(err.Error(), "must stay inside the repository") {
		t.Fatalf("a brief outside the repository must fail loading, got %v", err)
	}

	// .git is inside the repository but holds no brief, and its config can
	// hold a CI job's token.
	write("_aspect/modules/order.yaml", module("../../.git/config"))
	if _, err := Load(filepath.Join(repo, "_aspect", "aspect.yaml")); err == nil || !strings.Contains(err.Error(), "under .git") {
		t.Fatalf("a brief under .git must fail loading, got %v", err)
	}
	// On a case-insensitive file system .GIT is .git.
	write("_aspect/modules/order.yaml", module("../../.GIT/config"))
	if _, err := Load(filepath.Join(repo, "_aspect", "aspect.yaml")); err == nil || !strings.Contains(err.Error(), "under .git") {
		t.Fatalf("a brief under .GIT must fail loading, got %v", err)
	}

	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(repo, "pkg", "order", "LINK.md")); err != nil {
		t.Skip("symbolic links unavailable:", err)
	}
	write("_aspect/modules/order.yaml", module("../../pkg/order/LINK.md"))
	if _, err := Load(filepath.Join(repo, "_aspect", "aspect.yaml")); err == nil || !strings.Contains(err.Error(), "through a symbolic link") {
		t.Fatalf("a link out of the repository must fail loading, got %v", err)
	}
	// Nor through a link that stays in the repository but leads into .git.
	write(".git/config", "[http] extraheader = AUTHORIZATION: basic secret")
	if err := os.Symlink(filepath.Join("..", "..", ".git", "config"), filepath.Join(repo, "pkg", "order", "NOTES.md")); err != nil {
		t.Fatal(err)
	}
	write("_aspect/modules/order.yaml", module("../../pkg/order/NOTES.md"))
	if _, err := Load(filepath.Join(repo, "_aspect", "aspect.yaml")); err == nil || !strings.Contains(err.Error(), "resolves under .git") {
		t.Fatalf("a link into .git must fail loading, got %v", err)
	}
}

func TestValidateFieldTypesAndOperationVerbs(t *testing.T) {
	src := `
aspect: 1
system:
  name: shop
  intent: sell
  language: go
  module_path: example.com/shop
  goals: [{id: G1, statement: sells, verify: test}]
  database:
    engine: sqlite
    entities:
      - name: Category
        intent: groups products
        fields: [{name: ID, type: int64, key: primary}]
      - name: Product
        intent: what we sell
        fields:
          - {name: ID, type: int64, key: primary}
          - {name: Tags, type: json, intent: list of strings}
          - {name: Category, type: Category}
          - {name: Price, type: decimal}
          - {name: OwnerID, type: uint}
          - {name: Labels, type: "[string]"}
          - {name: At, type: time.Time}
  interfaces:
    - name: admin
      kind: web
      intent: back office
      surfaces:
        - {name: products, route: /admin/products, entity: Product, operations: [list, update, close]}
        - {name: export, route: /admin/export, entity: Product, operations: [ExportCSVHandler, Checker.Fix]}
modules:
  - name: catalog
    intent: owns products
    goals: [G1]
    entities: [Category, Product]
    surfaces: [admin.products, admin.export]
    scenarios: [{id: S1, when: list, then: products}]
`
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	issues := Validate(s)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", join(issues))
	}
	got := map[string]bool{}
	for _, i := range issues {
		got[i.Path] = true
	}
	for _, want := range []string{
		"system.database.entities[1].fields[4].type", // uint
		"system.database.entities[1].fields[5].type", // [string]
		"system.database.entities[1].fields[6].type", // time.Time
		"system.interfaces[0].surfaces[1].operations[0]",
		"system.interfaces[0].surfaces[1].operations[1]",
	} {
		if !got[want] {
			t.Errorf("missing warning at %s in:\n%s", want, join(issues))
		}
	}
	if len(issues) != 5 {
		t.Errorf("neutral types, json, entity names and verbs (custom ones included) must pass; got:\n%s", join(issues))
	}
}

func TestValidateInterfaceKindsEventsAndRoutes(t *testing.T) {
	src := `
aspect: 1
system:
  name: shop
  intent: sell
  language: go
  module_path: example.com/shop
  goals: [{id: G1, statement: sells, verify: test}]
  interfaces:
    - name: web
      kind: web
      intent: pages
      surfaces:
        - {name: cart, route: /cart, method: GET}
        - {name: update_cart, route: /cart, method: POST, event: UpdateCartQuantity}
        - {name: security_headers, route: /*}
        - {name: send_chat, route: "event:SendChatEvent"}
    - name: jobs
      kind: job
      intent: background work
      surfaces:
        - {name: renew, route: Process Next Billing}
        - {name: nightly, route: Nightly, event: Tick}
    - name: mcp
      kind: mcp
      intent: tools the assistant calls
      surfaces:
        - {name: get_cart, route: get_cart}
        - {name: no_route}
modules:
  - name: shop
    intent: sells
    goals: [G1]
    surfaces: [web.cart, web.update_cart, web.security_headers, web.send_chat, jobs.renew, jobs.nightly, mcp.get_cart, mcp.no_route]
    scenarios: [{id: S1, when: buy, then: sold}]
`
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Severity{}
	for _, i := range Validate(s) {
		got[i.Path] = i.Severity
	}
	want := map[string]Severity{
		"system.interfaces[0].surfaces[3].route": Warning, // event:SendChatEvent is not a path
		"system.interfaces[1].surfaces[1].event": Error,   // events belong to web surfaces
		"system.interfaces[2].surfaces[1].route": Error,   // mcp surfaces need their tool name
	}
	for p, sev := range want {
		if got[p] != sev {
			t.Errorf("%s: want %s, got %q", p, sev, got[p])
		}
	}
	if len(got) != len(want) {
		t.Errorf("job and mcp kinds, a web event on its page and middleware on /* must pass; got %v", got)
	}
}

func TestValidateInvariants(t *testing.T) {
	validSpec := `
aspect: 1
system:
  name: test
  intent: test
  language: go
  module_path: example.com/test
  goals:
    - {id: G1, statement: test goal, verify: test}
modules:
  - name: billing
    intent: billing intent
    goals: [G1]
    invariants:
      - id: INV-CHARGE-ONCE
        category: correctness
        statement: An order has at most one live charge.
        enforcement: test
      - Plain string invariant without an ID.
    scenarios:
      - id: S1
        when: pay
        then: charged
        invariants: [INV-CHARGE-ONCE]
`
	s, err := Parse([]byte(validSpec))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	issues := Validate(s)
	if issues.HasErrors() {
		t.Fatalf("valid invariants should pass validation: %v", issues)
	}
	if len(s.Modules[0].Invariants) != 2 {
		t.Fatalf("expected 2 invariants, got %d", len(s.Modules[0].Invariants))
	}
	if s.Modules[0].Invariants[0].ID != "INV-CHARGE-ONCE" || s.Modules[0].Invariants[0].Category != "correctness" {
		t.Fatalf("structured invariant not unmarshaled correctly: %+v", s.Modules[0].Invariants[0])
	}
	if s.Modules[0].Invariants[1].Statement != "Plain string invariant without an ID." {
		t.Fatalf("string invariant not unmarshaled correctly: %+v", s.Modules[0].Invariants[1])
	}

	invalidSpec := `
aspect: 1
system:
  name: test
  intent: test
  language: go
  module_path: example.com/test
  goals:
    - {id: G1, statement: test goal, verify: test}
modules:
  - name: billing
    intent: billing intent
    goals: [G1]
    invariants:
      - "{}"
      - id: "INVALID ID SPACES"
        category: bogus_category
        statement: "Something"
        enforcement: bogus_enforcement
      - id: "INV-DUP"
        statement: "first"
      - id: "INV-DUP"
        statement: "second"
    scenarios:
      - id: S1
        when: pay
        then: charged
        invariants: [UNKNOWN-INV]
`
	s2, err := Parse([]byte(invalidSpec))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	text := join(Validate(s2))
	for _, want := range []string{
		`invariant statement cannot be empty or '{}'`,
		`invalid invariant id "INVALID ID SPACES"`,
		`unknown category "bogus_category"`,
		`unknown enforcement "bogus_enforcement"`,
		`duplicate invariant id "INV-DUP"`,
		`references unknown invariant id "UNKNOWN-INV"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing expected validation message containing %q\ngot:\n%s", want, text)
		}
	}
}

// A tree with no .git (an export, a tarball) is rooted at its project
// manifest, so a spec reading its packages' ASPECT.md loads there as it
// does in a clone; with no manifest either, at the spec's own directory.
func TestRepositoryRootWithoutGit(t *testing.T) {
	tree := t.TempDir()
	for rel, content := range map[string]string{
		"go.mod":               "module example.com/g\n",
		"pkg/orders/ASPECT.md": "Orders are never deleted.",
		"_aspect/aspect.yaml":  "aspect: 1\nsystem:\n  name: g\n  intent: x\n  module_path: example.com/g\n  goals: [{id: G1, statement: x}]\nmodules:\n  - {name: orders, path: pkg/orders, intent: a, goals: [G1], brief: ../pkg/orders/ASPECT.md, scenarios: [{id: S1, when: a, then: b}]}\n",
	} {
		p := filepath.Join(tree, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Load(filepath.Join(tree, "_aspect", "aspect.yaml"))
	if err != nil {
		t.Fatalf("a spec in a tree with a go.mod and no .git must load: %v", err)
	}
	if s.Module("orders").Brief.Text != "Orders are never deleted." {
		t.Errorf("brief = %q", s.Module("orders").Brief.Text)
	}

	bare := t.TempDir()
	if got, err := RepositoryRoot(bare); err != nil || got != bare {
		if resolved, _ := filepath.EvalSymlinks(bare); got != resolved {
			t.Errorf("with neither .git nor a manifest the root is the directory itself: %q, %v", got, err)
		}
	}
}
