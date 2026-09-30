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
