package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type put struct{ key, ctype, body string }

type fakeStore struct {
	puts []put
	fail error
}

func (s *fakeStore) Put(_ context.Context, key, ctype string, body []byte) error {
	if s.fail != nil {
		return s.fail
	}
	s.puts = append(s.puts, put{key, ctype, string(body)})
	return nil
}

func (s *fakeStore) keys() []string {
	var k []string
	for _, p := range s.puts {
		k = append(k, p.key)
	}
	slices.Sort(k)
	return k
}

func write(t *testing.T, dir, rel, body string, age time.Duration) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
}

func newCollector(dir string, store Putter) *Collector {
	return &Collector{
		Dir: dir, Prefix: "tap/", Match: "*.trace.json", Settle: 2 * time.Second, MaxSize: 1 << 10,
		Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestSweepUploadsChangedFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "echo/a.trace.json", "a1", time.Minute)
	write(t, dir, "weather/b.trace.json", "b1", time.Minute)
	write(t, dir, "echo/notes.txt", "no", time.Minute)
	write(t, dir, "echo/.c.trace.json", "partial", time.Minute)
	write(t, dir, "echo/d.trace.json.tmp", "partial", time.Minute)
	write(t, dir, ".hidden/e.trace.json", "no", time.Minute)
	store := &fakeStore{}
	c := newCollector(dir, store)

	if n, err := c.Sweep(context.Background(), false); err != nil || n != 2 {
		t.Fatalf("first sweep: %d %v", n, err)
	}
	if got := store.keys(); !slices.Equal(got, []string{"tap/echo/a.trace.json", "tap/weather/b.trace.json"}) {
		t.Fatalf("keys %v", got)
	}
	if store.puts[0].ctype != "application/json" {
		t.Errorf("content type %s", store.puts[0].ctype)
	}

	if n, _ := c.Sweep(context.Background(), false); n != 0 {
		t.Fatalf("unchanged files re-uploaded: %d", n)
	}

	write(t, dir, "echo/a.trace.json", "a2 longer", 30*time.Second)
	if n, _ := c.Sweep(context.Background(), false); n != 1 || store.puts[2] != (put{"tap/echo/a.trace.json", "application/json", "a2 longer"}) {
		t.Fatalf("changed file: %d %+v", n, store.puts)
	}
}

func TestSettle(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "echo/a.trace.json", "fresh", 0)
	store := &fakeStore{}
	c := newCollector(dir, store)
	if n, _ := c.Sweep(context.Background(), false); n != 0 {
		t.Fatal("uploaded a file still being written")
	}
	if n, _ := c.Sweep(context.Background(), true); n != 1 {
		t.Fatal("final sweep skipped a fresh file")
	}
}

func TestSymlinksAreNotFollowed(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	write(t, outside, "secret-access-key", "s3cr3t", time.Minute)
	if err := os.MkdirAll(filepath.Join(dir, "echo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret-access-key"), filepath.Join(dir, "echo", "x.trace.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{}
	c := newCollector(dir, store)
	c.Match = "*"
	if _, err := c.Sweep(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(store.puts) != 0 {
		t.Fatalf("followed a symlink: %+v", store.puts)
	}
}

func TestFailedUploadIsRetried(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "echo/a.trace.json", "a", time.Minute)
	store := &fakeStore{fail: errors.New("503")}
	c := newCollector(dir, store)
	if _, err := c.Sweep(context.Background(), false); err == nil {
		t.Fatal("want the upload error")
	}
	store.fail = nil
	if n, err := c.Sweep(context.Background(), false); err != nil || n != 1 {
		t.Fatalf("retry: %d %v", n, err)
	}
}

func TestTooLargeIsSkippedOnce(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "echo/big.trace.json", string(make([]byte, 2<<10)), time.Minute)
	store := &fakeStore{}
	c := newCollector(dir, store)
	for range 2 {
		if n, err := c.Sweep(context.Background(), false); err != nil || n != 0 {
			t.Fatalf("%d %v", n, err)
		}
	}
	if len(store.puts) != 0 {
		t.Fatal("uploaded an oversized file")
	}
}

func TestStateFileSurvivesRestart(t *testing.T) {
	dir, state := t.TempDir(), filepath.Join(t.TempDir(), "state.json")
	write(t, dir, "echo/a.trace.json", "a", time.Minute)
	store := &fakeStore{}
	c := newCollector(dir, store)
	c.StateFile = state
	if err := c.Load(); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Sweep(context.Background(), false); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}

	again := newCollector(dir, store)
	again.StateFile = state
	if err := again.Load(); err != nil {
		t.Fatal(err)
	}
	if n, _ := again.Sweep(context.Background(), false); n != 0 {
		t.Fatal("re-uploaded after restart")
	}
	write(t, dir, "echo/b.trace.json", "b", time.Minute)
	if n, _ := again.Sweep(context.Background(), false); n != 1 {
		t.Fatal("missed a new file after restart")
	}
}
