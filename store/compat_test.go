package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/guoyyit/bipartite-matching-go/matching"
)

// A data directory produced by an older build has only wal.log and no
// meta.json. It must open directly, without manual migration, and expose the
// same history the old WAL contained.
func TestOpenLegacyDirectoryWithoutMeta(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for v := int64(1); v <= 5; v++ {
		if err := s.Append(sampleRecord(v, v*10)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a pre-fix directory: remove the lazily-created meta file.
	if err := os.Remove(filepath.Join(dir, metaFileName)); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("legacy dir must open without migration: %v", err)
	}
	defer reopened.Close()
	if got := reopened.Latest().Version; got != 5 {
		t.Fatalf("latest = %d want 5", got)
	}
	for v := int64(1); v <= 5; v++ {
		rec, ok := reopened.At(v)
		if !ok {
			t.Fatalf("legacy version %d missing", v)
		}
		pair := matching.Pair{Left: "a", Right: "x"}
		if rec.Graph.Edges[pair] != v*10 {
			t.Fatalf("legacy version %d weight = %d want %d", v, rec.Graph.Edges[pair], v*10)
		}
	}

	// Commits keep working and remain readable after another restart.
	if err := reopened.Append(sampleRecord(6, 60)); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got := again.Latest().Version; got != 6 {
		t.Fatalf("latest after further commit = %d want 6", got)
	}
}
