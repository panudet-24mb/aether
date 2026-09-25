// Package z2mcatalog is the list of devices Zigbee2MQTT supports, imported from zigbee-herdsman-converters (MIT,
// Copyright (c) Koen Kanters; licence text in LICENSE.zigbee-herdsman-converters) by infra/import-z2m-catalog.mjs.
// It answers "is this model supported, and what can it do?" before anything is bought or paired. Ingest never
// depends on it: a paired device is read through the definition its own bridge publishes.
package z2mcatalog

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"
)

//go:embed catalog.json.gz
var compressed []byte

// Device is one supported model.
type Device struct {
	Vendor      string   `json:"vendor"`
	Model       string   `json:"model"`
	Description string   `json:"description"`
	ZigbeeModel []string `json:"zigbee_model,omitempty"`
	WhiteLabel  []string `json:"white_label,omitempty"`
	// Category is the kind Aether shows the device as once paired (the same derivation as live ingest).
	Category string `json:"category"`
	// Features are the names of what it exposes (temperature, contact, state, brightness, action, ...).
	Features []string `json:"features,omitempty"`
	SOS      bool     `json:"sos,omitempty"`
	// Dynamic: the definition's exposes depend on the paired device, so Features may be incomplete.
	Dynamic bool   `json:"dynamic,omitempty"`
	search  string // lower-cased vendor, model, description, zigbee models and white labels
}

// Catalog is the whole list with where it came from.
type Catalog struct {
	Source   string
	Version  string
	License  string
	Homepage string
	Devices  []Device
	Vendors  []string
}

type document struct {
	Source   string `json:"source"`
	Version  string `json:"version"`
	License  string `json:"license"`
	Homepage string `json:"homepage"`
	Devices  []struct {
		V string   `json:"v"`
		M string   `json:"m"`
		D string   `json:"d"`
		Z []string `json:"z"`
		W []string `json:"w"`
		F []string `json:"f"`
		G []string `json:"g"`
		S int      `json:"s"`
		X int      `json:"x"`
	} `json:"devices"`
}

var (
	once    sync.Once
	loaded  *Catalog
	loadErr error
)

// Load decodes the embedded catalog once.
func Load() (*Catalog, error) {
	once.Do(func() { loaded, loadErr = decode(compressed) })
	return loaded, loadErr
}

func decode(gz []byte) (*Catalog, error) {
	r, e := gzip.NewReader(bytes.NewReader(gz))
	if e != nil {
		return nil, e
	}
	raw, e := io.ReadAll(io.LimitReader(r, 32<<20))
	if e != nil {
		return nil, e
	}
	var doc document
	if e := json.Unmarshal(raw, &doc); e != nil {
		return nil, e
	}
	c := &Catalog{Source: doc.Source, Version: doc.Version, License: doc.License, Homepage: doc.Homepage, Devices: make([]Device, 0, len(doc.Devices))}
	vendors := map[string]bool{}
	for _, d := range doc.Devices {
		p := zigbee2mqtt.Profile{Names: map[string]bool{}, Groups: map[string]bool{}, SOS: d.S == 1}
		for _, f := range d.F {
			p.Names[f] = true
		}
		for _, g := range d.G {
			p.Groups[g] = true
		}
		dev := Device{Vendor: d.V, Model: d.M, Description: d.D, ZigbeeModel: d.Z, WhiteLabel: d.W, Category: p.Category(), Features: d.F, SOS: p.SOS, Dynamic: d.X == 1}
		dev.search = strings.ToLower(strings.Join(append(append([]string{d.V, d.M, d.D}, d.Z...), d.W...), "\x00"))
		c.Devices = append(c.Devices, dev)
		vendors[d.V] = true
	}
	for v := range vendors {
		c.Vendors = append(c.Vendors, v)
	}
	sort.Strings(c.Vendors)
	return c, nil
}

// Search returns up to limit devices whose vendor, model, description, Zigbee model id or white label contains
// every word of q (case-insensitive), optionally of one vendor and one category, and the total number of matches.
// Exact model matches come first.
func (c *Catalog) Search(q, vendor, category string, limit int) ([]Device, int) {
	words := strings.Fields(strings.ToLower(q))
	exact := strings.ToLower(strings.TrimSpace(q))
	var first, rest []Device
	total := 0
	for _, d := range c.Devices {
		if vendor != "" && !strings.EqualFold(d.Vendor, vendor) {
			continue
		}
		if category != "" && d.Category != category {
			continue
		}
		match := true
		for _, w := range words {
			if !strings.Contains(d.search, w) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		total++
		isExact := exact != "" && (strings.EqualFold(d.Model, exact) || containsFold(d.ZigbeeModel, exact))
		switch {
		case isExact:
			first = append(first, d)
		case len(first)+len(rest) < limit:
			rest = append(rest, d)
		}
	}
	out := append(first, rest...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
