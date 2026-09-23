package studio

import (
	"strings"
	"testing"
)

func TestDisplayMarkupCannotNavigateOrExecute(t *testing.T) {
	s := SafeHTML(`<meta http-equiv="refresh" content="0;url=https://example.com"><script>alert(1)</script><iframe src="https://example.com"></iframe><a href="https://example.com">link</a><div onclick="alert(1)">metric</div><svg><foreignObject><iframe></iframe></foreignObject><polyline points="0,0 1,1"/></svg>`)
	for _, bad := range []string{"meta", "script", "iframe", "href", "onclick", "foreignObject"} {
		if strings.Contains(s, bad) {
			t.Fatal(s)
		}
	}
	if !strings.Contains(s, "metric") || !strings.Contains(s, "polyline") {
		t.Fatal("lost useful markup", s)
	}
}
