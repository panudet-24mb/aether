package domain

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

func glbHead(magic string, version, total, jsonLen, kind uint32) []byte {
	b := make([]byte, 20)
	copy(b, magic)
	binary.LittleEndian.PutUint32(b[4:], version)
	binary.LittleEndian.PutUint32(b[8:], total)
	binary.LittleEndian.PutUint32(b[12:], jsonLen)
	binary.LittleEndian.PutUint32(b[16:], kind)
	return b
}

func TestValidGLBHeader(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		size int
		ok   bool
	}{
		{"valid", glbHead("glTF", 2, 100, 40, 0x4E4F534A), 100, true},
		{"json fills the file", glbHead("glTF", 2, 100, 80, 0x4E4F534A), 100, true},
		{"magic", glbHead("gLTF", 2, 100, 40, 0x4E4F534A), 100, false},
		{"version 1", glbHead("glTF", 1, 100, 40, 0x4E4F534A), 100, false},
		{"length differs from size", glbHead("glTF", 2, 99, 40, 0x4E4F534A), 100, false},
		{"first chunk BIN", glbHead("glTF", 2, 100, 40, 0x004E4942), 100, false},
		{"json chunk past the end", glbHead("glTF", 2, 100, 81, 0x4E4F534A), 100, false},
		{"empty json chunk", glbHead("glTF", 2, 100, 0, 0x4E4F534A), 100, false},
		{"huge json length", glbHead("glTF", 2, 100, math.MaxUint32, 0x4E4F534A), 100, false},
		{"short", []byte("glTF"), 100, false},
	}
	for _, c := range cases {
		if got := ValidGLBHeader(c.head, c.size); got != c.ok {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestModelTransformValidate(t *testing.T) {
	if e := DefaultModelTransform().Validate(); e != nil {
		t.Fatal("default transform must be valid")
	}
	edit := func(f func(*ModelTransform)) ModelTransform {
		m := DefaultModelTransform()
		f(&m)
		return m
	}
	valid := []ModelTransform{
		edit(func(m *ModelTransform) { m.Scale = 0.0001 }),
		edit(func(m *ModelTransform) { m.Scale = 10000 }),
		edit(func(m *ModelTransform) { m.Up = "z"; m.Mode = "replace"; m.Visible = false; m.ClipToFloor = false }),
		edit(func(m *ModelTransform) { m.RotationDeg = []float64{-360, 90, 360} }),
		edit(func(m *ModelTransform) { m.OffsetM = []float64{-100000, 0, 100000} }),
		edit(func(m *ModelTransform) { m.Opacity = 0.05 }),
	}
	for i, m := range valid {
		if e := m.Validate(); e != nil {
			t.Errorf("valid %d refused: %+v", i, m)
		}
	}
	invalid := []ModelTransform{
		edit(func(m *ModelTransform) { m.Scale = 0 }),
		edit(func(m *ModelTransform) { m.Scale = 10001 }),
		edit(func(m *ModelTransform) { m.Scale = math.NaN() }),
		edit(func(m *ModelTransform) { m.Scale = math.Inf(1) }),
		edit(func(m *ModelTransform) { m.Up = "x" }),
		edit(func(m *ModelTransform) { m.Mode = "hide" }),
		edit(func(m *ModelTransform) { m.Opacity = 0.01 }),
		edit(func(m *ModelTransform) { m.Opacity = 1.5 }),
		edit(func(m *ModelTransform) { m.RotationDeg = []float64{0, 0} }),
		edit(func(m *ModelTransform) { m.RotationDeg = []float64{0, 0, 0, 0} }),
		edit(func(m *ModelTransform) { m.RotationDeg = []float64{0, 361, 0} }),
		edit(func(m *ModelTransform) { m.OffsetM = nil }),
		edit(func(m *ModelTransform) { m.OffsetM = []float64{0, math.NaN(), 0} }),
		edit(func(m *ModelTransform) { m.OffsetM = []float64{0, 0, -100001} }),
	}
	for i, m := range invalid {
		if e := m.Validate(); e != ErrInvalid {
			t.Errorf("invalid %d accepted: %+v", i, m)
		}
	}
}

func TestCleanModelName(t *testing.T) {
	cases := []struct {
		in, out string
		ok      bool
	}{
		{"Building A.ifc", "Building A.ifc", true},
		{"  C:\\Users\\me\\อาคาร 1.3dm ", "อาคาร 1.3dm", true},
		{"../../etc/passwd", "passwd", true},
		{"a\u202Egnp.exe", "agnp.exe", true},
		{"tab\there\x00", "tab here", true},
		{"   ", "", false},
		{"dir/", "", false},
		{strings.Repeat("ก", 120), strings.Repeat("ก", 120), true},
		{strings.Repeat("ก", 121), strings.Repeat("ก", 121), false},
	}
	for _, c := range cases {
		got, ok := CleanModelName(c.in)
		if got != c.out || ok != c.ok {
			t.Errorf("%q: got %q %v", c.in, got, ok)
		}
	}
	if SiteModelChunks(SiteModelMaxSize) != 205 || SiteModelChunks(1) != 1 || SiteModelChunks(SiteModelChunkSize) != 1 || SiteModelChunks(SiteModelChunkSize+1) != 2 {
		t.Fatal("chunk count")
	}
}
