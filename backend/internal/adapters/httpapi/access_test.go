package httpapi

import "testing"

func TestRouteHeadNormalisesSpelling(t *testing.T) {
	for path, want := range map[string]string{
		"/api/v1/commands": "commands", "/API/V1/Commands": "commands", "/api/v1//COMMANDS/": "commands",
		"/api/v1/members/x/access": "members", "/api/v1/": "", "/": "", "/Api/v1/Alerts/1": "alerts",
	} {
		if got := routeHead(path); got != want {
			t.Fatalf("%s: %q", path, got)
		}
		if want == "commands" && moduleFor(path, "POST") != "control" {
			t.Fatalf("%s: module %q", path, moduleFor(path, "POST"))
		}
	}
}
