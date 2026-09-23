package studio

import (
	"bytes"
	"golang.org/x/net/html"
	"strings"
)

// Keep display markup only. The sandbox/CSP is an additional boundary, not a sanitizer.
func SafeHTML(markup string) string {
	doc, e := html.Parse(strings.NewReader(markup))
	if e != nil {
		return ""
	}
	allowed := map[string]bool{}
	for _, tag := range strings.Fields("div span p strong b em i small h1 h2 h3 h4 h5 h6 header footer section article main aside label ul ol li table thead tbody tr th td br hr pre code svg g path polyline polygon line rect circle ellipse text tspan defs linearGradient stop") {
		allowed[strings.ToLower(tag)] = true
	}
	attrs := map[string]bool{}
	for _, a := range strings.Fields("class id style title role aria-label width height viewbox d points x y x1 y1 x2 y2 cx cy r rx ry fill stroke stroke-width stroke-linecap stroke-linejoin opacity transform font-size font-family text-anchor offset stop-color stop-opacity") {
		attrs[a] = true
	}
	var clean func(*html.Node)
	clean = func(parent *html.Node) {
		for n := parent.FirstChild; n != nil; {
			next := n.NextSibling
			if n.Type == html.ElementNode && !allowed[strings.ToLower(n.Data)] {
				parent.RemoveChild(n)
			} else if n.Type == html.CommentNode {
				parent.RemoveChild(n)
			} else {
				safe := []html.Attribute{}
				for _, a := range n.Attr {
					if a.Namespace == "" && attrs[strings.ToLower(a.Key)] {
						safe = append(safe, a)
					}
				}
				n.Attr = safe
				clean(n)
			}
			n = next
		}
	}
	var body *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "body" {
			body = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if body == nil {
		return ""
	}
	clean(body)
	var out bytes.Buffer
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		_ = html.Render(&out, c)
	}
	return out.String()
}
