package bipartite

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guoyyit/bipartite-matching-go/matching"
)

// versionFingerprint captures the committed graph and every scope's result
// of one version as a comparable string.
func versionFingerprint(t *testing.T, svc *Service, version int64) string {
	t.Helper()
	g, err := svc.GraphAt(version)
	if err != nil {
		t.Fatalf("graph at %d: %v", version, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "g=%v|%v|%v|%v|%v|%v", g.Left, g.Right, g.Edges, g.Forbidden, g.LeftCap, g.RightCap)
	for _, scope := range allScopes() {
		sol, err := svc.Query(scope, version)
		if err != nil {
			var infeasible *matching.InfeasibleError
			if errors.As(err, &infeasible) {
				fmt.Fprintf(&b, "|%s=ERR:%s", scope, infeasible.Code)
				continue
			}
			t.Fatalf("query %s at %d: %v", scope, version, err)
		}
		fmt.Fprintf(&b, "|%s=%d/%d/%v/%v/%v/%v", scope, sol.Weight, sol.Cardinality,
			sol.Matched, sol.UnmatchedLeft, sol.UnmatchedRight, sol.Feasible)
	}
	return b.String()
}

// fingerprintAll probes every version in [1, maxVersion] and records the
// fingerprint of each readable one.
func fingerprintAll(t *testing.T, svc *Service, maxVersion int64) map[int64]string {
	t.Helper()
	out := map[int64]string{}
	for v := int64(1); v <= maxVersion; v++ {
		if _, err := svc.GraphAt(v); errors.Is(err, ErrUnknownVersion) {
			continue
		} else if err != nil {
			t.Fatalf("graph at %d: %v", v, err)
		}
		out[v] = versionFingerprint(t, svc, v)
	}
	return out
}

func mustCommit(t *testing.T, txn *Txn) int64 {
	t.Helper()
	version, err := txn.Commit()
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return version
}

// Every version readable before a restart must stay bit-for-bit readable
// after it: graph and all three scopes.
func TestHistoryReadableAfterRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "svc")
	svc, err := Open(dir, seedGraph())
	if err != nil {
		t.Fatal(err)
	}

	const commits = 12
	for i := 0; i < commits; i++ {
		txn := svc.BeginUpdate()
		switch i % 5 {
		case 0:
			if err := txn.SetWeight(pair("a", "x"), int64(10+i)); err != nil {
				t.Fatal(err)
			}
		case 1:
			if err := txn.AddEdge(pair("c", fmt.Sprintf("x%d", i)), int64(3+i)); err != nil {
				t.Fatal(err)
			}
		case 2:
			if err := txn.SetLeftCap("a", 1+i%2); err != nil {
				t.Fatal(err)
			}
		case 3:
			if err := txn.Forbid(pair("b", "x")); err != nil {
				t.Fatal(err)
			}
		case 4:
			if err := txn.Allow(pair("b", "x")); err != nil {
				t.Fatal(err)
			}
		}
		mustCommit(t, txn)
	}
	want := fingerprintAll(t, svc, commits+1)
	if len(want) != commits+1 {
		t.Fatalf("readable versions = %d, want %d", len(want), commits+1)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := fingerprintAll(t, reopened, commits+1)
	if len(got) != len(want) {
		t.Fatalf("readable versions after restart = %d, want %d", len(got), len(want))
	}
	for v, fp := range want {
		if got[v] != fp {
			t.Fatalf("version %d differs across restart:\nbefore: %s\nafter:  %s", v, fp, got[v])
		}
	}
}

// The retention window must be identical before and after a restart: the
// same versions are readable, pruned versions report ErrUnknownVersion.
func TestHistoryRetentionConsistentAcrossRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "svc")
	svc, err := OpenWithHistoryLimit(dir, seedGraph(), 4)
	if err != nil {
		t.Fatal(err)
	}

	const commits = 20
	for i := 0; i < commits; i++ {
		txn := svc.BeginUpdate()
		if err := txn.SetWeight(pair("a", "x"), int64(100+i)); err != nil {
			t.Fatal(err)
		}
		mustCommit(t, txn)
	}
	before := fingerprintAll(t, svc, commits+1)
	if len(before) == commits+1 {
		t.Fatal("expected some versions to be pruned with history limit 4")
	}
	// Pruned versions report a distinguishable error.
	if _, err := svc.GraphAt(1); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("pruned read = %v, want ErrUnknownVersion", err)
	}
	if _, err := svc.Query(matching.ScopeMaxWeight, 1); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("pruned query = %v, want ErrUnknownVersion", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenWithHistoryLimit(dir, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after := fingerprintAll(t, reopened, commits+1)
	if len(after) != len(before) {
		t.Fatalf("readable window changed across restart: %d -> %d", len(before), len(after))
	}
	for v, fp := range before {
		if after[v] != fp {
			t.Fatalf("version %d differs across restart", v)
		}
	}
	if _, err := reopened.GraphAt(1); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("pruned read after restart = %v, want ErrUnknownVersion", err)
	}
}

// After a restart (with compaction active), new commits keep history
// readable and stay bit-for-bit identical to cold solves.
func TestPostRestartIncrementalMatchesCold(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "svc")
	svc, err := OpenWithHistoryLimit(dir, seedGraph(), 4)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		txn := svc.BeginUpdate()
		if err := txn.SetWeight(pair("a", "x"), int64(50+i)); err != nil {
			t.Fatal(err)
		}
		mustCommit(t, txn)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenWithHistoryLimit(dir, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for i := 0; i < 6; i++ {
		txn := reopened.BeginUpdate()
		if i%2 == 0 {
			if err := txn.SetWeight(pair("a", "y"), int64(200+i)); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := txn.AddEdge(pair("b", fmt.Sprintf("z%d", i)), int64(30+i)); err != nil {
				t.Fatal(err)
			}
		}
		mustCommit(t, txn)
		g, err := reopened.GraphAt(0)
		if err != nil {
			t.Fatal(err)
		}
		assertMatchesCold(t, reopened, g)
	}
	// Retained history is still readable after the post-restart commits.
	if got := fingerprintAll(t, reopened, reopened.Version()); len(got) == 0 {
		t.Fatal("no readable history after post-restart commits")
	}
}

// A data directory written by the previous on-disk format (records without a
// changes journal) opens directly, serves history, and accepts new commits.
func TestLegacyDataDirOpensWithoutMigration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "svc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	solution := func(scope, weight int) string {
		return fmt.Sprintf(`{"Scope":%d,"Weight":%d,"Cardinality":1,"Matched":[{"Left":"a","Right":"x","Weight":%d}],"UnmatchedLeft":null,"UnmatchedRight":null,"Feasible":true}`, scope, weight, weight)
	}
	entry := func(version, weight int) string {
		return fmt.Sprintf(`{"version":%d,"changes":null,"solutions":{"max_weight":%s,"max_cardinality":%s,"perfect":%s},"graph":{"left":["a"],"right":["x"],"edges":[{"l":"a","r":"x","w":%d}]}}`,
			version, solution(0, weight), solution(1, weight), solution(2, weight), weight)
	}
	var wal []byte
	for _, e := range []string{entry(1, 10), entry(2, 20)} {
		frame := make([]byte, 8)
		binary.BigEndian.PutUint64(frame, uint64(len(e)))
		wal = append(wal, frame...)
		wal = append(wal, e...)
		wal = append(wal, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "wal.log"), wal, 0o644); err != nil {
		t.Fatal(err)
	}

	svc, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.Version(); got != 2 {
		t.Fatalf("version = %d, want 2", got)
	}
	for v, want := range map[int64]int64{1: 10, 2: 20} {
		sol, err := svc.Query(matching.ScopeMaxWeight, v)
		if err != nil {
			t.Fatalf("legacy version %d unreadable: %v", v, err)
		}
		if sol.Weight != want {
			t.Fatalf("legacy version %d weight = %d, want %d", v, sol.Weight, want)
		}
	}
	// New commits work on top of the legacy data and survive a reopen.
	txn := svc.BeginUpdate()
	if err := txn.SetWeight(pair("a", "x"), 30); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, txn)
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Version(); got != 3 {
		t.Fatalf("version after reopen = %d, want 3", got)
	}
	if _, err := reopened.Query(matching.ScopeMaxWeight, 1); err != nil {
		t.Fatalf("legacy history lost after new commit: %v", err)
	}
}
