package pagegraph

import (
	"encoding/json"
	"testing"
)

func sample() Document {
	return Document{1, "root", map[string]Node{
		"root": {Component: "core.page-layout@1", Props: json.RawMessage(`{}`), Slots: map[string][]string{"main": {"hero", "grid"}}},
		"hero": {Component: "core.hero@1", Props: json.RawMessage(`{"title":"Sample"}`), Slots: map[string][]string{}},
		"grid": {Component: "catalog.product-grid@1", Props: json.RawMessage(`{"columns_desktop":4,"columns_mobile":2}`), Binding: &Binding{"catalog.featured-products@1", json.RawMessage(`{"limit":8}`)}, Slots: map[string][]string{}},
	}}
}
func TestValid(t *testing.T) {
	if err := Validate(sample()); err != nil {
		t.Fatal(err)
	}
}
func TestMissingNode(t *testing.T) {
	d := sample()
	delete(d.Nodes, "grid")
	if Validate(d) == nil {
		t.Fatal("should reject missing child")
	}
}
func TestDuplicateParent(t *testing.T) {
	d := sample()
	n := d.Nodes["root"]
	n.Slots["main"] = append(n.Slots["main"], "hero")
	d.Nodes["root"] = n
	if Validate(d) == nil {
		t.Fatal("should reject duplicate parent")
	}
}
func TestUnknownComponent(t *testing.T) {
	d := sample()
	n := d.Nodes["hero"]
	n.Component = "evil.js@1"
	d.Nodes["hero"] = n
	if Validate(d) == nil {
		t.Fatal("unregistered component accepted")
	}
}
func TestCycle(t *testing.T) {
	d := sample()
	n := d.Nodes["hero"]
	n.Component = "core.section@1"
	n.Props = json.RawMessage(`{}`)
	n.Slots = map[string][]string{"content": {"root"}}
	d.Nodes["hero"] = n
	if Validate(d) == nil {
		t.Fatal("cycle accepted")
	}
}
func TestInvalidGrid(t *testing.T) {
	d := sample()
	n := d.Nodes["grid"]
	n.Props = json.RawMessage(`{"columns_desktop":99,"columns_mobile":2}`)
	d.Nodes["grid"] = n
	if Validate(d) == nil {
		t.Fatal("invalid grid accepted")
	}
}
func TestUnknownProperty(t *testing.T) {
	d := sample()
	n := d.Nodes["hero"]
	n.Props = json.RawMessage(`{"title":"x","onClick":"alert(1)"}`)
	d.Nodes["hero"] = n
	if Validate(d) == nil {
		t.Fatal("unknown property accepted")
	}
}
func TestDisconnected(t *testing.T) {
	d := sample()
	d.Nodes["unused"] = Node{Component: "core.section@1", Props: json.RawMessage(`{}`), Slots: map[string][]string{}}
	if Validate(d) == nil {
		t.Fatal("disconnected accepted")
	}
}

func TestTrailingJSON(t *testing.T) {
	data := []byte(`{"schema_version":1,"root_id":"root","nodes":{"root":{"component":"core.page-layout@1","props":{},"slots":{"main":[]}}}} {"other":1}`)
	if ValidateJSON(data) == nil {
		t.Fatal("trailing JSON accepted")
	}
}
func TestCompanyParamsMustBeObject(t *testing.T) {
	for _, params := range []string{`{}`, `"not-an-object"`, `null`, `[]`, ``} {
		d := sample()
		n := d.Nodes["root"]
		n.Slots["main"] = append(n.Slots["main"], "company")
		d.Nodes["root"] = n
		d.Nodes["company"] = Node{Component: "company.profile@1", Props: json.RawMessage(`{}`), Binding: &Binding{"company.profile@1", json.RawMessage(params)}, Slots: map[string][]string{}}
		err := Validate(d)
		if (params == `{}`) != (err == nil) {
			t.Fatalf("params %q: err=%v", params, err)
		}
	}
}
func TestSlotsRequired(t *testing.T) {
	d := sample()
	n := d.Nodes["hero"]
	n.Slots = nil // omitted or "slots": null
	d.Nodes["hero"] = n
	if Validate(d) == nil {
		t.Fatal("should reject node without slots object")
	}
}
func TestExplicitNullRejected(t *testing.T) {
	doc := func(root string) string {
		return `{"schema_version":1,"root_id":"root","nodes":{"root":` + root + `}}`
	}
	if err := ValidateJSON([]byte(doc(`{"component":"core.page-layout@1","props":{},"slots":{"main":[]}}`))); err != nil {
		t.Fatalf("baseline should pass: %v", err)
	}
	for _, root := range []string{
		`{"component":"core.page-layout@1","props":{},"binding":null,"slots":{"main":[]}}`,
		`{"component":"core.page-layout@1","props":{},"slots":{"main":null}}`,
		`{"component":"core.page-layout@1","props":null,"slots":{"main":[]}}`,
		`{"component":"core.page-layout@1","props":{},"slots":{"main":[]},"extra":1}`,
	} {
		if ValidateJSON([]byte(doc(root))) == nil {
			t.Fatalf("should reject %s", root)
		}
	}
}
func TestCollectionBindingNeedsCollectionID(t *testing.T) {
	for params, ok := range map[string]bool{`{"limit":8,"collection_id":"c1"}`: true, `{"limit":8}`: false, `{"limit":8,"collection_id":""}`: false, `{"limit":8,"collection_id":5}`: false} {
		d := sample()
		g := d.Nodes["grid"]
		g.Binding = &Binding{"catalog.collection-products@1", json.RawMessage(params)}
		d.Nodes["grid"] = g
		if (Validate(d) == nil) != ok {
			t.Fatalf("params %s: want ok=%v", params, ok)
		}
	}
}
func TestHeroImageIDMustBeString(t *testing.T) {
	for img, ok := range map[string]bool{`"media_1"`: true, `5`: false, `true`: false, `[]`: false, `{}`: false} {
		d := sample()
		h := d.Nodes["hero"]
		h.Props = json.RawMessage(`{"title":"Sample","image_id":` + img + `}`)
		d.Nodes["hero"] = h
		if (Validate(d) == nil) != ok {
			t.Fatalf("image_id %s: want ok=%v", img, ok)
		}
	}
}
