package httpapi

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

func TestEtagMatches(t *testing.T) {
	etag := `"abc"`
	for header, want := range map[string]bool{
		`"abc"`:          true,
		`W/"abc"`:        true,
		`"x", "abc"`:     true,
		`*`:              true,
		`"abcd"`:         false,
		`abc`:            false,
		`"x",W/"y"`:      false,
		` "x" , W/"abc"`: true,
	} {
		if got := etagMatches(header, etag); got != want {
			t.Errorf("%q: got %v", header, got)
		}
	}
}

func TestDownloadGate(t *testing.T) {
	g := &downloadGate{perTenant: map[string]int{}}
	var held []func()
	for i := 0; i < modelDownloadsPerTeam; i++ {
		release, ok := g.acquire("a")
		if !ok {
			t.Fatalf("slot %d refused", i)
		}
		held = append(held, release)
	}
	if _, ok := g.acquire("a"); ok {
		t.Fatal("per-workspace cap not applied")
	}
	held[0]()
	held[0]() // a second release frees nothing more
	if g.total != modelDownloadsPerTeam-1 || g.perTenant["a"] != modelDownloadsPerTeam-1 {
		t.Fatalf("release counted twice: %d %v", g.total, g.perTenant)
	}
	if _, ok := g.acquire("a"); !ok {
		t.Fatal("a released slot was not reusable")
	}
	for i := 0; g.total < modelDownloads; i++ {
		if _, ok := g.acquire(string(rune('b' + i))); !ok {
			t.Fatalf("global slot refused at %d", g.total)
		}
	}
	if _, ok := g.acquire("z"); ok {
		t.Fatal("global cap not applied")
	}
}

func TestModelStream(t *testing.T) {
	file := bytes.Repeat([]byte("0123456789"), modelSlice/5+3) // a little over two slices
	released := 0
	fetches := 0
	stream := func(fail int) *modelStream {
		released, fetches = 0, 0
		start := time.Now()
		return &modelStream{size: len(file), started: start, now: func() time.Time { return start }, release: func() { released++ }, fetch: func(offset, n int) ([]byte, error) {
			fetches++
			if fetches == fail {
				return nil, errors.New("gone")
			}
			if n > modelSlice {
				t.Fatalf("slice of %d bytes", n)
			}
			return file[offset : offset+n], nil
		}}
	}
	got, e := io.ReadAll(stream(0))
	if e != nil || !bytes.Equal(got, file) || fetches != 3 || released != 1 {
		t.Fatalf("read %d bytes, %v, %d fetches, %d releases", len(got), e, fetches, released)
	}
	s := stream(2)
	got, e = io.ReadAll(s)
	if e != io.ErrUnexpectedEOF || len(got) != modelSlice || released != 1 {
		t.Fatalf("a failed slice: %d bytes, %v, %d releases", len(got), e, released)
	}
	_ = s.Close()
	if released != 2 { // release itself is idempotent (downloadGate); Close always calls it
		t.Fatalf("close: %d", released)
	}
}

// A client that averages under modelMinRate after modelGrace is dropped; a fast one is not.
func TestModelStreamMinimumRate(t *testing.T) {
	file := make([]byte, 4<<20)
	start := time.Now()
	clock := start
	released := 0
	m := &modelStream{size: len(file), started: start, now: func() time.Time { return clock }, release: func() { released++ },
		fetch: func(offset, n int) ([]byte, error) { return file[offset : offset+n], nil }}
	buf := make([]byte, 64<<10)
	// 64 KiB in the first 30 s is fine (the grace period); at 31 s it averages ~2 KiB/s and is dropped.
	if _, e := m.Read(buf); e != nil {
		t.Fatal(e)
	}
	clock = start.Add(modelGrace)
	if _, e := m.Read(buf); e != nil {
		t.Fatalf("dropped within the grace period: %v", e)
	}
	clock = start.Add(modelGrace + time.Second)
	if _, e := m.Read(buf); e != errTooSlow || released != 1 {
		t.Fatalf("slow client kept: %v %d", e, released)
	}
	// 2 MiB in 40 s (~51 KiB/s) is above the floor.
	fast := &modelStream{size: len(file), started: start, served: 2 << 20, now: func() time.Time { return start.Add(40 * time.Second) }, release: func() {},
		fetch: func(offset, n int) ([]byte, error) { return file[offset : offset+n], nil }}
	if _, e := fast.Read(buf); e != nil {
		t.Fatalf("fast client dropped: %v", e)
	}
}
