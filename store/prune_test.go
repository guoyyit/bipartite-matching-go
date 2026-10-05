package store

import "testing"

func TestVersionPruning(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const total = 300
	for v := int64(0); v < total; v++ {
		if err := s.Append(sampleRecord(v, v)); err != nil {
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

	if got := s2.Latest().Version; got != total-1 {
		t.Fatalf("latest = %d, want %d", got, total-1)
	}
	if _, ok := s2.At(0); ok {
		t.Fatal("version 0 should have been pruned")
	}
	latestRec, ok := s2.At(total - 1)
	if !ok {
		t.Fatal("latest version must still be readable")
	}
	if latestRec.Version != total-1 {
		t.Fatalf("read latest version = %d", latestRec.Version)
	}
}

func TestConfigurableHistoryLimit(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenWithHistoryLimit(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	// The 9th append pushes retained records past 2*4 and compacts to 4.
	for v := int64(0); v < 9; v++ {
		if err := s.Append(sampleRecord(v, v)); err != nil {
			t.Fatal(err)
		}
	}
	base, ok := s.Base()
	if !ok || base != 5 {
		t.Fatalf("base = %d/%v, want 5/true", base, ok)
	}
	if _, ok := s.At(4); ok {
		t.Fatal("version 4 should have been compacted away")
	}
	if got := len(s.Records()); got != 4 {
		t.Fatalf("retained records = %d, want 4", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening with the same limit must expose the identical window.
	s2, err := OpenWithHistoryLimit(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	base, ok = s2.Base()
	if !ok || base != 5 {
		t.Fatalf("base after reopen = %d/%v, want 5/true", base, ok)
	}
	if _, ok := s2.At(4); ok {
		t.Fatal("version 4 must stay pruned after reopen")
	}
	if _, ok := s2.At(8); !ok {
		t.Fatal("version 8 must be readable after reopen")
	}
}
