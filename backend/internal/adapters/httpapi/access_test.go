package httpapi

import (
	"strings"
	"testing"
)

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

func TestReportReadPostsAreOnlyExportAndSummary(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/v1/reports/export": true, "/api/v1/reports/summary": true, "/api/v1/reports/summary/": true,
		"/api/v1/reports/definitions": false, "/api/v1/reports/definitions/x/update": false, "/api/v1/reports/export/x": false,
		"/api/v1/reports": false, "/api/v1/alerts/export": false, "/api/v1/export": false,
	} {
		if got := readPost(path); got != want {
			t.Fatalf("%s: readPost %v", path, got)
		}
		if strings.HasPrefix(path, "/api/v1/reports") && moduleFor(path, "POST") != "reports" {
			t.Fatalf("%s: module %q", path, moduleFor(path, "POST"))
		}
	}
}
