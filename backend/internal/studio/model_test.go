package studio

import (
	"encoding/json"
	"testing"
)

func TestCatalogValidation(t *testing.T) {
	for _, i := range Official() {
		if e := Validate(i); e != nil {
			t.Fatal(e)
		}
	}
	i := Item{Kind: "dashboard", Name: "Floor", Brand: "Any", Model: "Any", Version: 1, Visibility: "community", Definition: json.RawMessage(`{"panels":[]}`)}
	if Validate(i) == nil {
		t.Fatal("public dashboard allowed")
	}
	i.Visibility = "private"
	i.Definition = json.RawMessage(`{"panels":[{"id":"a","title":"A","widget_id":"official-environment-v1","gateway_id":"bad","external_id":"xxxxxxxxxxxx","width":2,"height":1}]}`)
	if Validate(i) == nil {
		t.Fatal("invalid source accepted")
	}
}

// A controls panel names registered devices instead of one stream; nothing else may borrow its shape.
func TestControlsPanelValidation(t *testing.T) {
	item := func(panel string) Item {
		return Item{Kind: "dashboard", Name: "Controls", Brand: "Any", Model: "Any", Version: 1, Visibility: "private", Definition: json.RawMessage(`{"panels":[` + panel + `]}`)}
	}
	ok := `{"id":"a","title":"ICU","widget_id":"aether:controls","devices":["6f1c1f6e-8d0b-4c55-9a55-0b8f2d6c1a01","6f1c1f6e-8d0b-4c55-9a55-0b8f2d6c1a02"],"width":2,"height":2}`
	if e := Validate(item(ok)); e != nil {
		t.Fatal(e)
	}
	for name, bad := range map[string]string{
		"empty":               `{"id":"a","title":"ICU","widget_id":"aether:controls","devices":[],"width":2,"height":2}`,
		"not uuid":            `{"id":"a","title":"ICU","widget_id":"aether:controls","devices":["x"],"width":2,"height":2}`,
		"duplicate":           `{"id":"a","title":"ICU","widget_id":"aether:controls","devices":["6f1c1f6e-8d0b-4c55-9a55-0b8f2d6c1a01","6f1c1f6e-8d0b-4c55-9a55-0b8f2d6c1a01"],"width":2,"height":2}`,
		"and source":          `{"id":"a","title":"ICU","widget_id":"aether:controls","devices":["6f1c1f6e-8d0b-4c55-9a55-0b8f2d6c1a01"],"gateway_id":"6f1c1f6e-8d0b-4c55-9a55-0b8f2d6c1a09","external_id":"f10000000001","width":2,"height":2}`,
		"widget with devices": `{"id":"a","title":"A","widget_id":"official-environment-v1","gateway_id":"6f1c1f6e-8d0b-4c55-9a55-0b8f2d6c1a09","external_id":"f10000000001","devices":["6f1c1f6e-8d0b-4c55-9a55-0b8f2d6c1a01"],"width":2,"height":1}`,
	} {
		if Validate(item(bad)) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
