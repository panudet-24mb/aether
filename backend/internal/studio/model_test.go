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
