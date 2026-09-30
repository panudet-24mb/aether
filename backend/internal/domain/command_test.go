package domain

import "testing"

// Manual commands follow the role and the member's "control" access, never AUTOMATION_COMMANDS (that flag only
// gates automations): owner, admin and operator may; a viewer never may, whatever its access says.
func TestMayControl(t *testing.T) {
	cases := []struct {
		role   string
		access map[string]string
		want   bool
	}{
		{"owner", map[string]string{"control": "none"}, true},
		{"admin", nil, true},
		{"operator", map[string]string{}, true},
		{"operator", map[string]string{"control": "read"}, false},
		{"admin", map[string]string{"control": "none"}, false},
		{"viewer", nil, false},
		{"viewer", map[string]string{"control": "write"}, false},
	}
	for _, c := range cases {
		if got := (Principal{Role: c.role}).MayControl(c.access); got != c.want {
			t.Fatalf("%s %v: %v, want %v", c.role, c.access, got, c.want)
		}
	}
}
