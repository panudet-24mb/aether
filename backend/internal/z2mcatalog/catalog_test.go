package z2mcatalog

import "testing"

func TestEmbeddedCatalog(t *testing.T) {
	c, e := Load()
	if e != nil {
		t.Fatal(e)
	}
	if c.Source != "zigbee-herdsman-converters" || c.License != "MIT" || c.Version == "" || len(c.Devices) < 3000 || len(c.Vendors) < 100 {
		t.Fatalf("catalog: %s %s %s devices=%d vendors=%d", c.Source, c.Version, c.License, len(c.Devices), len(c.Vendors))
	}
	// Categories are derived exactly like live ingest does.
	want := map[string]string{"WSDCGQ11LM": "environment", "MCCGQ11LM": "door", "RTCGQ11LM": "occupancy", "SJCGQ11LM": "leak",
		"TS0215A_sos": "sos", "E1743": "remote", "JTYJ-GD-01LM/BW": "hazard", "TS0012": "switch", "LED1545G12": "lighting", "ZNCLDJ11LM": "cover", "BE468": "lock", "SPZB0001": "climate"}
	for model, category := range want {
		got, _ := c.Search(model, "", "", 5)
		if len(got) == 0 || got[0].Model != model {
			t.Fatalf("%s not first: %+v", model, got)
		}
		if got[0].Category != category {
			t.Fatalf("%s category %q, want %q", model, got[0].Category, category)
		}
	}
	if got, _ := c.Search("TS0215A_sos", "", "", 1); !got[0].SOS {
		t.Fatal("SOS button not flagged")
	}
	if got, _ := c.Search("E1743", "", "", 1); got[0].SOS {
		t.Fatal("ordinary remote flagged as SOS")
	}
}

func TestSearchFilters(t *testing.T) {
	c, _ := Load()
	// Every word must match; a Zigbee model id finds the device too.
	if got, total := c.Search("aqara door", "", "", 5); total == 0 || len(got) == 0 {
		t.Fatal("aqara door found nothing")
	}
	if got, _ := c.Search("lumi.weather", "", "", 5); len(got) == 0 || got[0].Model != "WSDCGQ11LM" {
		t.Fatalf("zigbee model id: %+v", got)
	}
	got, total := c.Search("", "IKEA", "lighting", 3)
	if len(got) != 3 || total < 10 {
		t.Fatalf("vendor+category: %d of %d", len(got), total)
	}
	for _, d := range got {
		if d.Vendor != "IKEA" || d.Category != "lighting" {
			t.Fatalf("filter leaked: %+v", d)
		}
	}
	if got, total := c.Search("definitely-not-a-zigbee-device", "", "", 10); len(got) != 0 || total != 0 {
		t.Fatal("nonsense matched")
	}
}

// Numeric "gas" (a gas meter or %LEL reading) is never a hazard detector in the catalog either.
func TestGasMetersAreNotHazards(t *testing.T) {
	c, _ := Load()
	got, _ := c.Search("ZHEMI101", "", "", 1)
	if len(got) == 0 || got[0].Model != "ZHEMI101" {
		t.Fatalf("ZHEMI101 missing: %+v", got)
	}
	if got[0].Category == "hazard" || got[0].SOS {
		t.Fatalf("gas/electricity meter categorised as %q", got[0].Category)
	}
	// Real gas detectors (binary gas) still are.
	if got, _ := c.Search("", "", "hazard", 200); len(got) < 20 {
		t.Fatalf("hazard detectors: %d", len(got))
	}
}
