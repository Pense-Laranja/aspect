package importer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eideroliveira/aspect/internal/agents"
	"github.com/eideroliveira/aspect/internal/llm"
	"github.com/eideroliveira/aspect/internal/spec"
)

const fixture = "../analyze/testdata/app"

// routingLLM answers Describer calls per package and Synthesizer calls once,
// by looking at the prompt.
type routingLLM struct {
	t         *testing.T
	fragments map[string]agents.Fragment
	synthesis agents.Synthesis
	calls     atomic.Int32
}

func (r *routingLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	r.calls.Add(1)
	var out any
	switch {
	case strings.HasPrefix(req.System, "You are the Describer"):
		for dir, f := range r.fragments {
			if strings.Contains(req.Prompt, "(dir "+dir+",") {
				out = f
			}
		}
		if out == nil {
			r.t.Fatalf("no fragment for prompt:\n%s", req.Prompt[:200])
		}
	case strings.HasPrefix(req.System, "You are the Synthesizer"):
		out = r.synthesis
	default:
		r.t.Fatalf("unexpected system prompt: %s", req.System[:60])
	}
	b, _ := json.Marshal(out)
	return llm.Response{Text: string(b), InputTokens: 100, OutputTokens: 50}, nil
}

func fragments() map[string]agents.Fragment {
	return map[string]agents.Fragment{
		"store": {
			Intent: "Persist products.",
			Goals:  []agents.FragmentGoal{{Label: "store-1", Statement: "Products persist", Verify: "test"}},
			Entities: []spec.Entity{{Name: "Product", Intent: "a sellable item", Fields: []spec.Field{
				{Name: "ID", Type: "int", Key: "true"}, {Name: "SKU", Type: "", Unique: true, Key: "part of the identity"}}, Relations: []spec.Relation{{Kind: "has_many", Entity: "Ghost"}}}},
			Operations: []spec.Operation{{Name: "Find", Signature: "Find(sku: string) -> Product | error"}},
			Scenarios:  []spec.Scenario{{ID: "S1", When: "Find(A)", Then: "returns A"}, {ID: "S1", When: "Find(Z)", Then: "error"}},
		},
		"web": {
			Intent: "Serve the HTTP API.",
			Interfaces: []spec.Interface{{Name: "API", Kind: "http", Intent: "programmatic access", Surfaces: []spec.Surface{
				{Name: "product", Route: "/products/{sku}", Method: "GET", Entity: "Product"},
				{Name: "reserve", Route: "/reservations", Method: "", Entity: "Nope"},
			}}},
			Operations: []spec.Operation{{Name: "New", Signature: ""}},
		},
	}
}

func TestRunMirrorsPackages(t *testing.T) {
	fake := &routingLLM{t: t, fragments: fragments(), synthesis: agents.Synthesis{
		Name: "App", Intent: "Sell products.",
		Goals: []agents.SynthesisGoal{
			{ID: "G1", Statement: "Products persist", Verify: "test", Modules: []string{"store", "web"}},
			{ID: "G2", Statement: "Orphan", Verify: "test", Modules: []string{"nowhere"}},
		},
		Stack:      spec.LanguageStack{Frameworks: []spec.Framework{{Name: "chi", Module: "github.com/go-chi/chi/v5"}}, ORM: &spec.Framework{Name: "gorm", Module: "gorm.io/gorm"}},
		Interfaces: []spec.Interface{{Name: "api", Kind: "http", Intent: "what clients call", Framework: "chi", Auth: "token", Provider: "chi + qor5 presets"}},
	}}
	cache := t.TempDir()
	res, err := Run(context.Background(), fake, Options{Root: fixture, CacheDir: cache, Repository: "git@example.com:app.git", Commit: "abc123", Exclude: []string{"cmd"}})
	if err != nil {
		t.Fatal(err)
	}
	s := res.Spec
	if s.System.Name != "app" || s.System.Language != "go" || s.System.ModulePath != "example.com/app" {
		t.Fatalf("system = %+v", s.System)
	}
	if s.System.Source == nil || s.System.Source.Commit != "abc123" || strings.Join(s.System.Source.Frameworks, ",") != "chi,gorm" {
		t.Fatalf("source = %+v", s.System.Source)
	}
	if len(s.Modules) != 2 || s.Modules[0].Name != "store" || s.Modules[1].Name != "web" || strings.Join(s.Modules[1].DependsOn, ",") != "store" {
		t.Fatalf("modules = %+v", s.Modules)
	}
	if s.Modules[0].Path != "store" || s.Modules[1].Path != "web" || strings.Join(s.System.Source.Exclude, ",") != "cmd" {
		t.Fatalf("every mirrored module records its package directory, the source its exclusions: %q %q %v", s.Modules[0].Path, s.Modules[1].Path, s.System.Source.Exclude)
	}
	if s.System.Database == nil || s.System.Database.Engine != "postgres" || len(s.System.Database.Entities) != 1 || strings.Join(s.Modules[0].Entities, ",") != "Product" {
		t.Fatalf("database = %+v, store entities = %v", s.System.Database, s.Modules[0].Entities)
	}
	if len(s.System.Interfaces) != 1 || s.System.Interfaces[0].Name != "api" || s.System.Interfaces[0].Framework != "chi" || len(s.System.Interfaces[0].Surfaces) != 2 {
		t.Fatalf("interfaces = %+v", s.System.Interfaces)
	}
	if got := strings.Join(s.Modules[1].Surfaces, ","); got != "api.product,api.reserve" {
		t.Fatalf("web surfaces = %s", got)
	}
	if p := s.System.Interfaces[0].Provider; p != "" {
		t.Fatalf("a free-text provider in a single-tier spec must be cleared, got %q", p)
	}
	if f := s.System.Database.Entities[0].Fields; f[0].Key != "primary" || f[1].Key != "" {
		t.Fatalf("keys = %q, %q; want a truthy key read as primary and a descriptive one cleared", f[0].Key, f[1].Key)
	}
	if len(s.System.Goals) != 1 || strings.Join(s.Modules[0].Goals, ",") != "G1" {
		t.Fatalf("goals = %+v / %v", s.System.Goals, s.Modules[0].Goals)
	}
	text := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"goal G2", "field SKU had no type", "relation to unknown entity Ghost", `unknown entity "Nope"`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing warning %q in:\n%s", want, text)
		}
	}
	if res.Issues.HasErrors() {
		t.Fatalf("assembled spec must validate; issues:\n%v", res.Issues)
	}
	if res.Usage.Calls != 3 || fake.calls.Load() != 3 {
		t.Fatalf("calls = %d / %d", res.Usage.Calls, fake.calls.Load())
	}

	// The YAML round-trips through the loader.
	// The default output, _aspect/aspect.yaml, lives in a directory that
	// does not exist yet; Write creates it.
	path := filepath.Join(t.TempDir(), "_aspect", "aspect.yaml")
	if err := Write(path, s, res.Warnings, res.Briefs); err != nil {
		t.Fatal(err)
	}
	back, err := spec.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if issues := spec.Validate(back); issues.HasErrors() {
		t.Fatalf("round-tripped spec invalid:\n%v", issues)
	}
	if got := back.Module("store").Brief; got.Path != "briefs/store.md" || !strings.Contains(got.Text, "SKU is immutable") {
		t.Fatalf("the store package's CLAUDE.md must become the module brief: %+v", got)
	}
	if back.Module("web").Brief.Path != "" {
		t.Fatal("a package without documents gets no brief")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "# Assembly warnings:") || strings.Contains(string(b), "key: \"\"") {
		t.Fatalf("yaml should carry warnings and omit empty fields:\n%s", b)
	}

	// A second run hits the cache and makes no calls.
	fake.calls.Store(0)
	if _, err := Run(context.Background(), fake, Options{Root: fixture, CacheDir: cache}); err != nil {
		t.Fatal(err)
	}
	if n := fake.calls.Load(); n != 0 {
		t.Fatalf("cache miss: %d calls", n)
	}
}

func TestRunRetargetsToSwift(t *testing.T) {
	fake := &routingLLM{t: t, fragments: fragments(), synthesis: agents.Synthesis{
		Name: "Shop App", Intent: "Let members browse and reserve products from their phone.",
		Goals: []agents.SynthesisGoal{{ID: "G1", Statement: "Members can reserve", Verify: "test", Modules: []string{"Api Client", "catalog_feature"}}},
		Interfaces: []spec.Interface{
			{Name: "api", Kind: "http", Provider: "external", Service: "the existing backend", Intent: "the existing backend", Surfaces: []spec.Surface{{Name: "reserve", Route: "/reservations", Method: "POST"}}},
			{Name: "app", Kind: "app", Intent: "screens", Surfaces: []spec.Surface{{Name: "Product List", Route: "/products"}}},
		},
		Database: &spec.Database{Engine: "sqlite", Migrations: "auto", Test: spec.DBTest{Engine: "sqlite"}, Entities: []spec.Entity{{Name: "Product", Intent: "cached product", Fields: []spec.Field{{Name: "id", Type: "int", Key: "primary"}}}}},
		Modules: []spec.Module{
			{Name: "Api Client", Intent: "talk to the backend", Surfaces: []string{"api.reserve"}, Scenarios: []spec.Scenario{{ID: "S1", When: "POST", Then: "ok"}}},
			{Name: "catalog_feature", Intent: "browse", DependsOn: []string{"Api Client"}, Entities: []string{"Product"}, Surfaces: []string{"app.Product List"}, Scenarios: []spec.Scenario{{ID: "S1", When: "open", Then: "list"}}},
		},
	}}
	res, err := Run(context.Background(), fake, Options{Root: fixture, Target: "swift", Topology: spec.Monolith, TargetHint: "an iOS app for members"})
	if err != nil {
		t.Fatal(err)
	}
	s := res.Spec
	if s.System.Language != "swift" || s.System.ModulePath != "com.example.shop_app" || s.System.Source.Language != "go" || len(s.Tiers) != 0 {
		t.Fatalf("system = %+v", s.System)
	}
	if len(s.Modules) != 2 || s.Modules[0].Name != "api_client" || s.Modules[1].DependsOn[0] != "api_client" || s.Modules[1].Surfaces[0] != "app.product_list" {
		t.Fatalf("modules = %+v", s.Modules)
	}
	if s.System.Interfaces[0].Provider != "external" || s.System.Interfaces[1].Surfaces[0].Name != "product_list" {
		t.Fatalf("interfaces = %+v", s.System.Interfaces)
	}
	if client := s.Module("api_client"); len(client.Surfaces) != 0 || strings.Join(client.Consumes, ",") != "api.reserve" {
		t.Fatalf("an external surface listed under surfaces must move to consumes: %+v", client)
	}
	if res.Issues.HasErrors() {
		t.Fatalf("retargeted spec must validate; issues:\n%v", res.Issues)
	}
}

func TestIdent(t *testing.T) {
	for in, want := range map[string]string{"modules/translation": "modules_translation", "Api Client": "api_client", "3d": "p_3d", "": "x", ".": "root"} {
		got := Ident(in)
		if in == "." {
			got = ModuleName(in)
		}
		if got != want {
			t.Errorf("Ident(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunSplitsIntoApiBackendTiers(t *testing.T) {
	fake := &routingLLM{t: t, fragments: fragments(), synthesis: agents.Synthesis{
		Name: "Shop", Intent: "Sell products on the web and on phones.",
		Goals: []agents.SynthesisGoal{
			{ID: "G1", Statement: "Members can reserve from the app", Verify: "test", Modules: []string{"mobile_api", "catalog_feature"}},
			{ID: "G2", Statement: "Products persist", Verify: "test", Modules: []string{"store"}},
		},
		Stack: spec.LanguageStack{ORM: &spec.Framework{Name: "gorm", Module: "gorm.io/gorm"}},
		Interfaces: []spec.Interface{
			{Name: "api", Kind: "http", Intent: "what the app calls", Provider: "backend", Surfaces: []spec.Surface{
				{Name: "reserve", Route: "/reservations", Method: "POST"}, // exists in the mirrored api
				{Name: "me", Route: "/me", Method: "GET"},                 // new
			}},
			{Name: "app", Kind: "app", Intent: "screens", Provider: "mobile", Surfaces: []spec.Surface{{Name: "Catalog", Route: "/catalog"}}},
		},
		Tiers: []spec.Tier{
			{Name: "backend", Intent: "serve", Language: "go", Modules: []spec.Module{
				{Name: "mobile_api", Intent: "endpoints for the app", DependsOn: []string{"store"}, Surfaces: []string{"api.me"}, Scenarios: []spec.Scenario{{ID: "S1", When: "GET /me", Then: "200"}}},
			}},
			{Name: "mobile", Intent: "the app", Language: "swift", ModulePath: "com.example.shop", Modules: []spec.Module{
				{Name: "api_client", Intent: "talk to the backend", Consumes: []string{"api"}, Scenarios: []spec.Scenario{{ID: "S1", When: "GET", Then: "ok"}}},
				{Name: "catalog_feature", Intent: "browse", DependsOn: []string{"api_client"}, Surfaces: []string{"app.Catalog", "api.reserve"}, Scenarios: []spec.Scenario{{ID: "S1", When: "open", Then: "list"}}},
			}},
		},
	}}
	res, err := Run(context.Background(), fake, Options{Root: fixture, Target: "swift", TargetHint: "an iOS app"})
	if err != nil {
		t.Fatal(err)
	}
	s := res.Spec
	if s.EffectiveTopology() != spec.APIBackend || len(s.Tiers) != 2 {
		t.Fatalf("topology=%s tiers=%d", s.EffectiveTopology(), len(s.Tiers))
	}
	backend, mobile := s.Tiers[0], s.Tiers[1]
	if backend.Name != "backend" || backend.Language != "go" || backend.ModulePath != "example.com/app" || backend.Stack["go"].ORM == nil {
		t.Fatalf("backend = %+v", backend)
	}
	if got := moduleNames(backend.Modules); got != "store,web,mobile_api" {
		t.Fatalf("backend modules = %s (mirrored packages plus the addition)", got)
	}
	if mobile.Language != "swift" || mobile.DependsOn[0] != "backend" || moduleNames(mobile.Modules) != "api_client,catalog_feature" {
		t.Fatalf("mobile = %+v", mobile)
	}
	if s.System.Database == nil || s.System.Database.Tier != "backend" {
		t.Fatalf("database = %+v", s.System.Database)
	}
	api := s.Interface("api")
	if api == nil || api.Provider != "backend" || len(api.Surfaces) != 3 {
		t.Fatalf("api = %+v (mirrored product+reserve plus the new me)", api)
	}
	if app := s.Interface("app"); app == nil || app.Provider != "mobile" || app.Surfaces[0].Name != "catalog" {
		t.Fatalf("app = %+v", s.Interface("app"))
	}
	feature := s.Module("catalog_feature")
	if strings.Join(feature.Surfaces, ",") != "app.catalog" || strings.Join(feature.Consumes, ",") != "api.reserve" {
		t.Fatalf("catalog_feature surfaces=%v consumes=%v (a backend surface must move to consumes)", feature.Surfaces, feature.Consumes)
	}
	if res.Issues.HasErrors() {
		t.Fatalf("api_backend spec must validate; issues:\n%v", res.Issues)
	}
	path := filepath.Join(t.TempDir(), "two.yaml")
	if err := Write(path, s, res.Warnings, res.Briefs); err != nil {
		t.Fatal(err)
	}
	back, err := spec.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if issues := spec.Validate(back); issues.HasErrors() || len(back.Tiers) != 2 {
		t.Fatalf("round trip: tiers=%d issues=%v", len(back.Tiers), issues)
	}
}

func TestRunCloudServiceTopology(t *testing.T) {
	fake := &routingLLM{t: t, fragments: fragments(), synthesis: agents.Synthesis{
		Name: "Shop", Intent: "Sell from phones over a hosted backend.",
		Goals: []agents.SynthesisGoal{{ID: "G1", Statement: "Members can reserve", Verify: "test", Modules: []string{"catalog_feature"}}},
		Interfaces: []spec.Interface{
			{Name: "api", Kind: "http", Intent: "the hosted backend", Provider: "external", Service: "Supabase", Surfaces: []spec.Surface{{Name: "reserve", Route: "/rest/v1/reservations", Method: "POST"}}},
			{Name: "app", Kind: "app", Intent: "screens", Surfaces: []spec.Surface{{Name: "catalog", Route: "/catalog"}}},
		},
		Database: &spec.Database{Engine: "postgres", Entities: []spec.Entity{{Name: "Product", Intent: "remote", Fields: []spec.Field{{Name: "id", Type: "int", Key: "primary"}}}}},
		Tiers: []spec.Tier{{Name: "app", Intent: "the app", Language: "swift", Modules: []spec.Module{
			{Name: "catalog_feature", Intent: "browse", Consumes: []string{"api.reserve"}, Surfaces: []string{"app.catalog"}, Scenarios: []spec.Scenario{{ID: "S1", When: "open", Then: "list"}}},
		}}},
	}}
	res, err := Run(context.Background(), fake, Options{Root: fixture, Target: "swift", Topology: spec.CloudService})
	if err != nil {
		t.Fatal(err)
	}
	s := res.Spec
	if s.EffectiveTopology() != spec.CloudService || len(s.Tiers) != 0 || s.System.Language != "swift" {
		t.Fatalf("shape: %s tiers=%d lang=%s", s.EffectiveTopology(), len(s.Tiers), s.System.Language)
	}
	if s.System.Database == nil || s.System.Database.Tier != "external" || s.Interface("api").Provider != "external" || s.Interface("api").Service != "Supabase" {
		t.Fatalf("external wiring: db=%+v api=%+v", s.System.Database, s.Interface("api"))
	}
	if res.Issues.HasErrors() {
		t.Fatalf("cloud_service spec must validate; issues:\n%v", res.Issues)
	}
}

func moduleNames(mods []spec.Module) string {
	var names []string
	for _, m := range mods {
		names = append(names, m.Name)
	}
	return strings.Join(names, ",")
}
