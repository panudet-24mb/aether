// Package edgeinstall renders the Aether Edge installer script served at /edge/install.sh. A piped script cannot
// know where it was downloaded from, so the server fills in its own public origin and the image it pins.
package edgeinstall

import (
	_ "embed"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

//go:embed install.sh.tmpl
var script string

// imagePattern: a pinned repository:tag, optionally also pinned by digest (repository:tag@sha256:…).
var imagePattern = regexp.MustCompile(`^[a-z0-9./_-]+:[A-Za-z0-9._-]+(@sha256:[0-9a-f]{64})?$`)

// Script returns the installer for this server. origin must be an http(s) origin without a path; image and z2mImage
// pinned references. All end up inside single quotes in the script, so they must not contain a quote.
func Script(origin, image, z2mImage string) (string, error) {
	u, e := url.Parse(origin)
	if e != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" ||
		strings.ContainsAny(origin, "'\\\n") {
		return "", errors.New("edgeinstall: bad origin")
	}
	if !imagePattern.MatchString(image) || !imagePattern.MatchString(z2mImage) {
		return "", errors.New("edgeinstall: bad image reference")
	}
	return strings.NewReplacer("__AETHER_ORIGIN__", origin, "__EDGE_IMAGE__", image, "__Z2M_IMAGE__", z2mImage).Replace(script), nil
}
