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

// Groups are named sections a panel may join; a panel cannot point at a group the dashboard does not have.
func TestDashboardGroupsValidation(t *testing.T) {
	dashboard := func(definition string) Item {
		return Item{Kind: "dashboard", Name: "Restrooms", Brand: "Any", Model: "Any", Version: 1, Visibility: "private", Definition: json.RawMessage(definition)}
	}
	panel := func(group string) string {
		return `{"id":"p` + group + `","title":"Odour","widget_id":"official-milesight-gs301-v1","gateway_id":"6f1c1f6e-8d0b-4c55-9a55-0b8f2d6c1a09","external_id":"24e124798e210951","width":2,"height":1,"group_id":"` + group + `"}`
	}
	ok := `{"groups":[{"id":"m","name":"ห้องน้ำชั้น M"},{"id":"f1","name":"ห้องน้ำชั้น 1"}],"panels":[` + panel("m") + `,` + panel("") + `]}`
	if e := Validate(dashboard(ok)); e != nil {
		t.Fatal(e)
	}
	many := `{"groups":[`
	for i := 0; i <= MaxGroups; i++ {
		if i > 0 {
			many += ","
		}
		many += `{"id":"g` + string(rune('a'+i)) + `","name":"G"}`
	}
	many += `],"panels":[]}`
	for name, bad := range map[string]string{
		"unknown group":   `{"groups":[{"id":"m","name":"M"}],"panels":[` + panel("x") + `]}`,
		"no groups":       `{"panels":[` + panel("m") + `]}`,
		"duplicate group": `{"groups":[{"id":"m","name":"M"},{"id":"m","name":"M2"}],"panels":[]}`,
		"blank name":      `{"groups":[{"id":"m","name":"  "}],"panels":[]}`,
		"blank id":        `{"groups":[{"id":"","name":"M"}],"panels":[]}`,
		"too many":        many,
	} {
		if Validate(dashboard(bad)) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// Groups nest up to MaxGroupDepth levels under parents that exist, without cycles.
func TestDashboardNestedGroupsValidation(t *testing.T) {
	dashboard := func(groups string) Item {
		return Item{Kind: "dashboard", Name: "Building", Brand: "Any", Model: "Any", Version: 1, Visibility: "private", Definition: json.RawMessage(`{"groups":[` + groups + `],"panels":[]}`)}
	}
	ok := `{"id":"b","name":"อาคาร"},{"id":"m","name":"ชั้น M","parent_id":"b","width":2,"height":3},{"id":"w","name":"ห้องน้ำหญิง","parent_id":"m","width":1},{"id":"o","name":"ส่วนกลาง","height":1}`
	if e := Validate(dashboard(ok)); e != nil {
		t.Fatal(e)
	}
	// A child may come before its parent in the list: order is only the sibling order.
	if e := Validate(dashboard(`{"id":"m","name":"ชั้น M","parent_id":"b"},{"id":"b","name":"อาคาร"}`)); e != nil {
		t.Fatal(e)
	}
	for name, bad := range map[string]string{
		"too deep":       `{"id":"a","name":"A"},{"id":"b","name":"B","parent_id":"a"},{"id":"c","name":"C","parent_id":"b"},{"id":"d","name":"D","parent_id":"c"}`,
		"unknown parent": `{"id":"a","name":"A","parent_id":"x"}`,
		"own parent":     `{"id":"a","name":"A","parent_id":"a"}`,
		"cycle":          `{"id":"a","name":"A","parent_id":"b"},{"id":"b","name":"B","parent_id":"a"}`,
		"cycle above":    `{"id":"a","name":"A","parent_id":"b"},{"id":"b","name":"B","parent_id":"c"},{"id":"c","name":"C","parent_id":"b"}`,
		"too wide":       `{"id":"a","name":"A","width":5}`,
		"too tall":       `{"id":"a","name":"A","height":5}`,
		"negative":       `{"id":"a","name":"A","width":-1}`,
	} {
		if Validate(dashboard(bad)) == nil {
			t.Fatalf("%s accepted", name)
		}
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
