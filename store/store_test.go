package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/guoyyit/bipartite-matching-go/matching"
)

func sampleRecord(version int64, weight int64) VersionRecord {
	g := matching.NewGraph()
	g.Left = []string{"a"}
	g.Right = []string{"x"}
	g.Edges[matching.Pair{Left: "a", Right: "x"}] = weight
	sol := &matching.Solution{
		Scope:       matching.ScopeMaxWeight,
		Weight:      weight,
		Cardinality: 1,
		Feasible:    true,
		Matched:     []matching.Match{{Pair: matching.Pair{Left: "a", Right: "x"}, Weight: weight}},
	}
	return VersionRecord{
		Version:   version,
		Changes:   []matching.Change{{Kind: matching.ChangeSetWeight, Weight: weight}},
		Solutions: map[matching.Scope]*matching.Solution{matching.ScopeMaxWeight: sol},
		Graph:     g,
	}
}

func TestAppendAndReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for v := int64(0); v < 5; v++ {
		if err := s.Append(sampleRecord(v, v*10)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Latest().Version; got != 4 {
		t.Fatalf("latest version = %d, want 4", got)
	}
	rec, ok := s2.At(2)
	if !ok {
		t.Fatal("version 2 not found")
	}
	if weight := rec.Graph.Edges[matching.Pair{Left: "a", Right: "x"}]; weight != 20 {
		t.Fatalf("weight = %d, want 20", weight)
	}
	if rec.Solutions[matching.ScopeMaxWeight].Weight != 20 {
		t.Fatalf("solution weight mismatch: %v", rec.Solutions)
	}
}

func TestTornRecordRecovery(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(sampleRecord(0, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(sampleRecord(1, 2)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash mid-write: append garbage bytes after the valid WAL.
	path := filepath.Join(dir, walFileName)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 'g', 'a', 'r'}); err != nil {
		t.Fatal(err)
	}
	file.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Latest().Version; got != 1 {
		t.Fatalf("latest after recovery = %d, want 1", got)
	}

	// The store must still accept clean commits after truncating the tail.
	if err := s2.Append(sampleRecord(2, 3)); err != nil {
		t.Fatal(err)
	}
	s2.Close()

	s3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	if got := s3.Latest().Version; got != 2 {
		t.Fatalf("latest after new commit = %d, want 2", got)
	}
}

func TestVersionNotFound(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Append(sampleRecord(0, 1)); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.At(99); ok {
		t.Fatal("expected version 99 to be absent")
	}
}
