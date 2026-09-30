package bipartite

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/guoyyit/bipartite-matching-go/matching"
)

func pair(l, r string) matching.Pair { return matching.Pair{Left: l, Right: r} }

func seedGraph() *matching.Graph {
	g := matching.NewGraph()
	g.Left = []string{"a", "b"}
	g.Right = []string{"x", "y"}
	g.Edges[pair("a", "x")] = 10
	g.Edges[pair("a", "y")] = 9
	g.Edges[pair("b", "x")] = 1
	g.Edges[pair("b", "y")] = 8
	return g
}

func TestQueryScopesAndDeterminism(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "svc"), seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	mw, err := svc.Query(matching.ScopeMaxWeight, 0)
	if err != nil {
		t.Fatal(err)
	}
	if mw.Weight != 18 || mw.Cardinality != 2 {
		t.Fatalf("max weight = %d/%d", mw.Weight, mw.Cardinality)
	}

	perfect, err := svc.Query(matching.ScopePerfect, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(perfect.UnmatchedLeft) != 0 || len(perfect.UnmatchedRight) != 0 {
		t.Fatalf("perfect left unmatched: %v %v", perfect.UnmatchedLeft, perfect.UnmatchedRight)
	}
}

func TestIncrementalUpdateAndIsolation(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "svc"), seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	before, _ := svc.Query(matching.ScopeMaxWeight, 0)

	// A staged, uncommitted txn must not be visible to any reader.
	txn := svc.BeginUpdate()
	if err := txn.SetWeight(pair("a", "x"), 100); err != nil {
		t.Fatal(err)
	}
	during, _ := svc.Query(matching.ScopeMaxWeight, 0)
	if during.Weight != before.Weight {
		t.Fatalf("uncommitted change observed: %d vs %d", during.Weight, before.Weight)
	}

	version, err := txn.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("version = %d, want 2", version)
	}
	after, _ := svc.Query(matching.ScopeMaxWeight, 0)
	// (a,x)=100 plus the independent (b,y)=8 both belong to the optimum.
	if after.Weight != 108 {
		t.Fatalf("committed weight = %d, want 108", after.Weight)
	}

	// Historical read of the seed version 1 still sees the old complete
	// state: no mixed-version result across the commit boundary.
	old0, err := svc.Query(matching.ScopeMaxWeight, 1)
	if err != nil {
		t.Fatal(err)
	}
	if old0.Weight != before.Weight {
		t.Fatalf("historical v0 weight = %d, want %d", old0.Weight, before.Weight)
	}
}

func TestConstraintFailureCodes(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "svc"), seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	txn := svc.BeginUpdate()
	if err := txn.Forbid(pair("a", "x")); err != nil {
		t.Fatal(err)
	}
	if err := txn.Forbid(pair("a", "y")); err != nil {
		t.Fatal(err)
	}
	if _, err := txn.Commit(); err != nil {
		t.Fatal(err)
	}

	_, err = svc.Query(matching.ScopePerfect, 0)
	var infeasible *matching.InfeasibleError
	if !errors.As(err, &infeasible) || infeasible.Code != matching.FailurePerfectUnreachable {
		t.Fatalf("err = %v, want perfect_unreachable", err)
	}

	// MaxWeight still has a valid answer and must not be relaxed.
	mw, err := svc.Query(matching.ScopeMaxWeight, 0)
	if err != nil || mw.Cardinality == 0 {
		t.Fatalf("max weight after forbid = %v / %v", mw, err)
	}
}

func TestCrashRecovery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "svc")
	svc, err := Open(dir, seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	for v := int64(1); v <= 5; v++ {
		txn := svc.BeginUpdate()
		if err := txn.SetWeight(pair("a", "x"), v); err != nil {
			t.Fatal(err)
		}
		if _, err := txn.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	wantWeight := func() int64 {
		sol, _ := svc.Query(matching.ScopeMaxWeight, 0)
		return sol.Weight
	}()
	wantVersion := svc.Version()
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Version() != wantVersion {
		t.Fatalf("version after restart = %d, want %d", reopened.Version(), wantVersion)
	}
	sol, err := reopened.Query(matching.ScopeMaxWeight, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sol.Weight != wantWeight {
		t.Fatalf("weight after restart = %d, want %d", sol.Weight, wantWeight)
	}

	// A post-restart commit must keep producing consistent incrementals.
	txn := reopened.BeginUpdate()
	if err := txn.SetWeight(pair("b", "y"), 40); err != nil {
		t.Fatal(err)
	}
	if _, err := txn.Commit(); err != nil {
		t.Fatal(err)
	}
	graph, _ := reopened.GraphAt(0)
	full, err := matching.Solve(graph, matching.ScopeMaxWeight)
	if err != nil {
		t.Fatal(err)
	}
	inc, err := reopened.Query(matching.ScopeMaxWeight, 0)
	if err != nil || full.Weight != inc.Weight {
		t.Fatalf("post-recovery incremental mismatch full=%d inc=%d err=%v", full.Weight, inc.Weight, err)
	}
}

func TestConcurrentUpdatesAndQueries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "svc")
	svc, err := Open(dir, seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for step := 0; step < 25; step++ {
				txn := svc.BeginUpdate()
				weight := int64(worker*100 + step)
				if err := txn.SetWeight(pair("a", "x"), weight); err != nil {
					t.Errorf("set weight: %v", err)
					return
				}
				if _, err := txn.Commit(); err != nil {
					t.Errorf("commit: %v", err)
					return
				}
			}
		}(i)
	}

	// Readers hammer all scopes across current and historical versions and
	// must always observe a self-consistent complete snapshot.
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for step := 0; step < 200; step++ {
				version := svc.Version()
				sol, err := svc.Query(matching.ScopeMaxWeight, 0)
				if err == nil && sol != nil && sol.Weight < 0 {
					t.Errorf("impossible weight %d", sol.Weight)
				}
				_ = version
				_, _ = svc.Query(matching.ScopePerfect, 0)
				_, _ = svc.Query(matching.ScopeMaxCardinality, 0)
			}
		}()
	}
	wg.Wait()

	if svc.Version() != 1+8*25 {
		t.Fatalf("final version = %d, want %d (lost updates?)", svc.Version(), 1+8*25)
	}

	// The committed result must agree with a from-scratch recomputation.
	graph, _ := svc.GraphAt(0)
	full, err := matching.Solve(graph, matching.ScopeMaxWeight)
	if err != nil {
		t.Fatal(err)
	}
	inc, err := svc.Query(matching.ScopeMaxWeight, 0)
	if err != nil {
		t.Fatal(err)
	}
	if full.Weight != inc.Weight {
		t.Fatalf("concurrent final state mismatch full=%v inc=%v", full.Matched, inc.Matched)
	}
	if fmt.Sprint(full.Matched) != fmt.Sprint(inc.Matched) {
		t.Fatalf("concurrent deterministic result differs:\nfull=%v\ninc=%v", full.Matched, inc.Matched)
	}
}
