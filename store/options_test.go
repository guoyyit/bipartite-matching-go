package store

import "testing"

func TestHistoryOptionPersistedAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, WithHistory(7))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.History(); got != 7 {
		t.Fatalf("history = %d want 7", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	for v := int64(0); v < 20; v++ {
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.History(); got != 7 {
			t.Fatalf("history after reopen #%d = %d want 7", v, got)
		}
		if err := s.Append(sampleRecord(v, v)); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.Latest().Version; got != 19 {
		t.Fatalf("latest = %d want 19", got)
	}
	if _, ok := s.At(12); ok {
		t.Fatal("v12 should have been pruned with keep=7")
	}
	if _, ok := s.At(13); !ok {
		t.Fatal("v13 must remain readable with keep=7")
	}
}

func TestHistoryOptionZeroUnlimited(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, WithHistory(0))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for v := int64(0); v < 300; v++ {
		if err := s.Append(sampleRecord(v, v)); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := s.At(0); !ok {
		t.Fatal("v0 must remain with unlimited history")
	}
	if got := s.History(); got != 0 {
		t.Fatalf("history = %d want 0", got)
	}
}

func TestHistoryOptionNegativeRejected(t *testing.T) {
	if _, err := Open(t.TempDir(), WithHistory(-1)); err == nil {
		t.Fatal("negative history must be rejected")
	}
}
