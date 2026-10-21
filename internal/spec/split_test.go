package spec

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// splitInto splits src into dir (plus the brief files given) and loads the
// result.
func splitInto(t *testing.T, dir, src string, briefs map[string]string) (*Layout, *Spec) {
	t.Helper()
	l, err := Split([]byte(src), "aspect.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.CheckTarget(dir); err != nil {
		t.Fatal(err)
	}
	if err := l.Write(dir); err != nil {
		t.Fatal(err)
	}
	for p, text := range briefs {
		abs := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Load(filepath.Join(dir, "aspect.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return l, s
}

func TestSplitExamplesRoundTrip(t *testing.T) {
	for _, name := range []string{"inventory", "shop_two_tier", "warehouse_admin", "cloud_service_minimal"} {
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("..", "..", "examples", name, "aspect.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			want, err := Parse(src)
			if err != nil {
				t.Fatal(err)
			}
			l, got := splitInto(t, t.TempDir(), string(src), nil)
			if issues := Validate(got); issues.HasErrors() {
				t.Fatalf("split spec must validate:\n%s", join(issues))
			}
			for _, m := range got.AllModules() {
				var p string
				if tier := got.TierOf(m.Name); len(got.Tiers) > 0 {
					p = "tiers/" + tier.Name + "/modules/" + m.Name + ".yaml"
				} else {
					p = "modules/" + m.Name + ".yaml"
				}
				if _, ok := l.Files[p]; !ok {
					t.Errorf("module %s: no %s in %v", m.Name, p, l.Paths())
				}
			}
			got.Dir, got.Path, got.Root, got.Deps, got.Sources = "", "", "", nil, nil
			normalize(want)
			normalize(got)
			if !reflect.DeepEqual(want, got) {
				t.Fatal("split spec loads differently from the original")
			}
		})
	}
}

func TestSplitLayout(t *testing.T) {
	src := `# Header comment survives.
aspect: 1
system:
  name: shop
  intent: Sell.
  module_path: example.com/shop
  brief: briefs/system.md
  goals: [{id: G1, statement: sells, verify: test}]
  database:
    engine: postgres
    entities:
      - {name: Product, fields: [{name: id, type: uuid, key: primary}]}
      - {name: Note, fields: [{name: id, type: uuid, key: primary}]}
      - {name: Order, fields: [{name: id, type: uuid, key: primary}]}
  interfaces:
    - name: api
      kind: http
      intent: Buy things.
      surfaces: [{name: buy, method: POST, route: /buy, entity: Order}]
modules:
  # catalog owns products.
  - name: catalog
    intent: Own products.
    brief: briefs/catalog.md
    goals: [G1]
    entities: [Product, Note]
    scenarios: [{id: S1, when: list, then: products}]
  - name: orders
    intent: Take orders.
    goals: [G1]
    depends_on: [catalog]
    entities: [Order]
    surfaces: [api.buy]
    scenarios: [{id: S1, when: buy, then: order}]
`
	dir := t.TempDir()
	l, s := splitInto(t, dir, src, map[string]string{"briefs/system.md": "system", "briefs/catalog.md": "catalog brief"})

	want := []string{"aspect.yaml", "entities/catalog.yaml", "entities/orders.yaml", "interfaces/api.yaml", "modules/catalog.yaml", "modules/orders.yaml"}
	if got := l.Paths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	if got := l.Dirs(); !reflect.DeepEqual(got, []string{"entities", "interfaces", "modules"}) {
		t.Fatalf("dirs = %v", got)
	}
	root := string(l.Files["aspect.yaml"])
	for _, w := range []string{"# Header comment survives.", "- dir: modules", "- dir: interfaces", "- dir: entities", "engine: postgres", "brief: briefs/system.md"} {
		if !strings.Contains(root, w) {
			t.Errorf("entry point lacks %q:\n%s", w, root)
		}
	}
	catalog := string(l.Files["modules/catalog.yaml"])
	if !strings.Contains(catalog, "brief: ../briefs/catalog.md") || !strings.Contains(catalog, "# catalog owns products.") {
		t.Errorf("module fragment must keep its comment and rebase its brief:\n%s", catalog)
	}
	if e := string(l.Files["entities/catalog.yaml"]); !strings.Contains(e, "name: Product") || !strings.Contains(e, "name: Note") || strings.Contains(e, "Order") {
		t.Errorf("entities are grouped by owner:\n%s", e)
	}
	if b := s.Module("catalog").Brief; b.Path != "briefs/catalog.md" || b.Text != "catalog brief" {
		t.Errorf("brief after reload = %+v", b)
	}
	if issues := Validate(s); issues.HasErrors() {
		t.Fatalf("split spec must validate:\n%s", join(issues))
	}

	// A second split into the same directory would be pulled in twice.
	if err := l.CheckTarget(dir); err == nil || !strings.Contains(err.Error(), "already holds YAML") {
		t.Fatalf("want occupied-directory error, got %v", err)
	}
}

func TestSplitUnownedEntities(t *testing.T) {
	src := `aspect: 1
system:
  name: s
  intent: i
  module_path: m
  database: {engine: postgres, entities: [{name: Orphan, fields: [{name: id, type: uuid}]}]}
modules: [{name: a, intent: i}]
`
	l, err := Split([]byte(src), "aspect.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Files["entities/_unowned.yaml"]; !ok {
		t.Fatalf("an entity no module owns goes to _unowned.yaml: %v", l.Paths())
	}
}

func TestSplitRefuses(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"already split", "aspect: 1\nsystem: {name: s, intent: i}\nmodules:\n  - dir: modules\n", "already uses includes"},
		{"included mapping", "aspect: 1\nsystem: {name: s, intent: i, database: {file: db.yaml}}\n", "already uses includes"},
		{"unnamed", "aspect: 1\nsystem: {name: s, intent: i}\nmodules: [{intent: i}]\n", "modules[0] has no name"},
		{"escaping name", "aspect: 1\nsystem: {name: s, intent: i}\nmodules: [{name: ../x, intent: i}]\n", "cannot be a file name"},
		{"hidden name", "aspect: 1\nsystem: {name: s, intent: i}\nmodules: [{name: .x, intent: i}]\n", "cannot be a file name"},
		{"duplicate", "aspect: 1\nsystem: {name: s, intent: i}\nmodules: [{name: a, intent: i}, {name: a, intent: j}]\n", "another entry already uses"},
		{"case", "aspect: 1\nsystem: {name: s, intent: i}\nmodules: [{name: Cart, intent: i}, {name: cart, intent: j}]\n", "differ only in case"},
		{"empty", "", "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Split([]byte(tc.src), "aspect.yaml"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLocateThroughIncludes(t *testing.T) {
	s, err := Load("testdata/split/aspect.yaml")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("testdata", "split")
	for path, want := range map[string]string{
		"modules[1].depends_on":            filepath.Join(root, "modules", "20_orders.yaml") + ":4",
		"modules[2]":                       filepath.Join(root, "modules", "20_orders.yaml") + ":8",
		"modules[0].scenarios[0].then":     filepath.Join(root, "modules", "10_catalog.yaml") + ":4",
		"system.interfaces[0].surfaces[0]": filepath.Join(root, "interfaces", "api.yaml") + ":5",
		"module orders.consumes":           filepath.Join(root, "modules", "20_orders.yaml") + ":6",
		"system.goals[0]":                  filepath.Join(root, "aspect.yaml") + ":12",
		"modules[9]":                       filepath.Join(root, "aspect.yaml") + ":15", // out of range: the list itself
		"database.entities":                "",
		"module ghost.brief":               "",
	} {
		if got := s.Sources.Locate(path); got != want {
			t.Errorf("Locate(%q) = %q, want %q", path, got, want)
		}
	}

	// Validation issues carry the location; specs parsed from memory do not.
	s.Module("audit").Goals = []string{"G9"}
	var found bool
	for _, i := range Validate(s) {
		if strings.Contains(i.Message, "G9") {
			found = true
			if !strings.HasPrefix(i.String(), filepath.Join(root, "modules", "20_orders.yaml")+":") {
				t.Errorf("issue = %s, want it to start with the fragment's location", i)
			}
		}
	}
	if !found {
		t.Fatal("expected an unknown-goal issue")
	}
	if loc := (*Sources)(nil).Locate("modules[0]"); loc != "" {
		t.Fatalf("nil sources must locate nothing, got %q", loc)
	}
}
