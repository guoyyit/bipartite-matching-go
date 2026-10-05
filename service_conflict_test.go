package bipartite

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/guoyyit/bipartite-matching-go/matching"
)

func allScopes() []matching.Scope {
	return []matching.Scope{
		matching.ScopeMaxWeight,
		matching.ScopeMaxCardinality,
		matching.ScopePerfect,
	}
}

func solutionsEqual(a, b *matching.Solution) bool {
	return a.Weight == b.Weight &&
		a.Cardinality == b.Cardinality &&
		a.Feasible == b.Feasible &&
		fmt.Sprint(a.Matched) == fmt.Sprint(b.Matched) &&
		fmt.Sprint(a.UnmatchedLeft) == fmt.Sprint(b.UnmatchedLeft) &&
		fmt.Sprint(a.UnmatchedRight) == fmt.Sprint(b.UnmatchedRight)
}

// assertMatchesCold verifies every scope of the latest committed version is
// bit-for-bit identical to a from-scratch solve of the committed graph.
func assertMatchesCold(t *testing.T, svc *Service, g *matching.Graph) {
	t.Helper()
	for _, scope := range allScopes() {
		full, fullErr := matching.Solve(g, scope)
		inc, incErr := svc.Query(scope, 0)
		if (fullErr == nil) != (incErr == nil) {
			t.Fatalf("scope %s: cold err=%v incremental err=%v", scope, fullErr, incErr)
		}
		if fullErr != nil {
			var cold, warm *matching.InfeasibleError
			if !errors.As(fullErr, &cold) || !errors.As(incErr, &warm) || cold.Code != warm.Code {
				t.Fatalf("scope %s: cold err=%v incremental err=%v", scope, fullErr, incErr)
			}
			continue
		}
		if !solutionsEqual(full, inc) {
			t.Fatalf("scope %s mismatch:\ncold=%v\nwarm=%v", scope, full.Matched, inc.Matched)
		}
	}
}

// Two transactions based on the same version modify different edges; both
// commits must survive in the final state (no lost update).
func TestOverlappingTxnsDifferentEdges(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "svc"), seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	t1 := svc.BeginUpdate()
	t2 := svc.BeginUpdate()
	if err := t1.SetWeight(pair("a", "x"), 100); err != nil {
		t.Fatal(err)
	}
	if err := t2.SetWeight(pair("b", "y"), 50); err != nil {
		t.Fatal(err)
	}
	if _, err := t1.Commit(); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if _, err := t2.Commit(); err != nil {
		t.Fatalf("second commit must merge cleanly: %v", err)
	}

	g, err := svc.GraphAt(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Edges[pair("a", "x")]; got != 100 {
		t.Fatalf("first committer's change lost: (a,x) = %d, want 100", got)
	}
	if got := g.Edges[pair("b", "y")]; got != 50 {
		t.Fatalf("second committer's change lost: (b,y) = %d, want 50", got)
	}

	// The merged state equals serial application of t1 then t2 on the base.
	reference := seedGraph()
	reference.Edges[pair("a", "x")] = 100
	reference.Edges[pair("b", "y")] = 50
	assertMatchesCold(t, svc, reference)
}

// Two transactions modify the same edge: the later committer's value wins,
// bit-for-bit identical to serial application.
func TestOverlappingTxnsSameEdge(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "svc"), seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	t1 := svc.BeginUpdate()
	t2 := svc.BeginUpdate()
	if err := t1.SetWeight(pair("a", "x"), 100); err != nil {
		t.Fatal(err)
	}
	if err := t2.SetWeight(pair("a", "x"), 42); err != nil {
		t.Fatal(err)
	}
	if _, err := t1.Commit(); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if _, err := t2.Commit(); err != nil {
		t.Fatalf("same-edge reweight must merge: %v", err)
	}

	g, err := svc.GraphAt(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Edges[pair("a", "x")]; got != 42 {
		t.Fatalf("serial last-writer-wins violated: (a,x) = %d, want 42", got)
	}
	reference := seedGraph()
	reference.Edges[pair("a", "x")] = 42
	assertMatchesCold(t, svc, reference)
}

// Two transactions modify the same constraint: serial semantics apply.
func TestOverlappingTxnsSameConstraint(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "svc"), seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	t1 := svc.BeginUpdate()
	t2 := svc.BeginUpdate()
	if err := t1.SetLeftCap("a", 2); err != nil {
		t.Fatal(err)
	}
	if err := t2.SetLeftCap("a", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := t1.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := t2.Commit(); err != nil {
		t.Fatalf("same-constraint update must merge: %v", err)
	}

	g, err := svc.GraphAt(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := g.LeftCap["a"]; got != 3 {
		t.Fatalf("cap = %d, want 3 (later committer wins)", got)
	}
}

// A commit that cannot be merged safely is rejected with a distinguishable,
// retryable conflict error; the retried transaction then succeeds.
func TestConflictingCommitRejectedAndRetried(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "svc"), seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	deleter := svc.BeginUpdate()
	reweighter := svc.BeginUpdate()
	if err := deleter.DeleteEdge(pair("a", "x")); err != nil {
		t.Fatal(err)
	}
	if err := reweighter.SetWeight(pair("a", "x"), 77); err != nil {
		t.Fatal(err)
	}
	if _, err := deleter.Commit(); err != nil {
		t.Fatal(err)
	}

	// Reweighting an edge that no longer exists cannot be serialized.
	_, err = reweighter.Commit()
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("err = %v, want ErrTxnConflict", err)
	}
	if errors.Is(err, ErrTxnRolledBack) || errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("conflict error must be distinguishable: %v", err)
	}
	// The rejected commit assigned no version and published nothing.
	if got := svc.Version(); got != 2 {
		t.Fatalf("version = %d, want 2 (rejected commit left no hole)", got)
	}

	// Retry path: a fresh transaction based on the latest state re-applies
	// the intent (the edge is gone, so it is re-added) and commits.
	retry := svc.BeginUpdate()
	if err := retry.AddEdge(pair("a", "x"), 77); err != nil {
		t.Fatal(err)
	}
	version, err := retry.Commit()
	if err != nil {
		t.Fatalf("retry commit: %v", err)
	}
	if version != 3 {
		t.Fatalf("retry version = %d, want 3", version)
	}
	g, err := svc.GraphAt(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Edges[pair("a", "x")]; got != 77 {
		t.Fatalf("retried change missing: (a,x) = %d, want 77", got)
	}
}

// Two transactions adding the same edge conflict; the loser is rejected.
func TestConflictingDuplicateAddRejected(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "svc"), seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	t1 := svc.BeginUpdate()
	t2 := svc.BeginUpdate()
	if err := t1.AddEdge(pair("c", "z"), 5); err != nil {
		t.Fatal(err)
	}
	if err := t2.AddEdge(pair("c", "z"), 9); err != nil {
		t.Fatal(err)
	}
	if _, err := t1.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := t2.Commit(); !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("err = %v, want ErrTxnConflict", err)
	}
	g, err := svc.GraphAt(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Edges[pair("c", "z")]; got != 5 {
		t.Fatalf("(c,z) = %d, want 5 (first committer)", got)
	}
}

// A rejected transaction must never commit successfully afterwards.
func TestRejectedTxnCannotBeReused(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "svc"), seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	t1 := svc.BeginUpdate()
	t2 := svc.BeginUpdate()
	if err := t1.DeleteEdge(pair("a", "x")); err != nil {
		t.Fatal(err)
	}
	if err := t2.SetWeight(pair("a", "x"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := t1.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := t2.Commit(); !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("err = %v, want ErrTxnConflict", err)
	}
	if _, err := t2.Commit(); err == nil {
		t.Fatal("rejected transaction committed on reuse")
	}
	if err := t2.SetWeight(pair("a", "y"), 1); !errors.Is(err, ErrTxnRolledBack) {
		t.Fatalf("staging on rejected txn = %v, want ErrTxnRolledBack", err)
	}
	t2.Rollback() // must not panic
	if got := svc.Version(); got != 2 {
		t.Fatalf("version = %d, want 2", got)
	}
}

// Rollback interleaved with commits leaves no trace and no version hole.
func TestRollbackInterleavedWithCommit(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "svc"), seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	rolledBack := svc.BeginUpdate()
	committed := svc.BeginUpdate()
	if err := rolledBack.SetWeight(pair("a", "x"), 1000); err != nil {
		t.Fatal(err)
	}
	if err := committed.SetWeight(pair("a", "y"), 7); err != nil {
		t.Fatal(err)
	}
	rolledBack.Rollback()
	if _, err := rolledBack.Commit(); !errors.Is(err, ErrTxnRolledBack) {
		t.Fatalf("commit after rollback = %v, want ErrTxnRolledBack", err)
	}
	version, err := committed.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("version = %d, want 2 (rollback left no hole)", version)
	}
	g, err := svc.GraphAt(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Edges[pair("a", "x")]; got != 10 {
		t.Fatalf("rolled-back change visible: (a,x) = %d, want 10", got)
	}
	if got := g.Edges[pair("a", "y")]; got != 7 {
		t.Fatalf("committed change missing: (a,y) = %d, want 7", got)
	}
}

// Concurrent overlapping transactions under pressure: every successful
// commit's changes survive, versions have no holes, and the final state is
// bit-for-bit identical to a cold solve. Stable across repeated runs; run
// with -race.
func TestConcurrentCommitPressure(t *testing.T) {
	for run := 0; run < 3; run++ {
		t.Run(fmt.Sprintf("run%d", run), func(t *testing.T) {
			svc, err := Open(filepath.Join(t.TempDir(), "svc"), seedGraph())
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()

			const workers = 8
			const iters = 20
			var committed atomic.Int64
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(worker int) {
					defer wg.Done()
					for i := 0; i < iters; i++ {
						// Unique edge per worker: must never be lost.
						own := pair(fmt.Sprintf("w%d", worker), fmt.Sprintf("n%d", i))
						for {
							txn := svc.BeginUpdate()
							if err := txn.AddEdge(own, int64(worker*1000+i)); err != nil {
								t.Errorf("stage add: %v", err)
								return
							}
							if _, err := txn.Commit(); err != nil {
								if errors.Is(err, ErrTxnConflict) {
									continue
								}
								t.Errorf("commit add: %v", err)
								return
							}
							committed.Add(1)
							break
						}
						// Shared edge: reweights merge, deletes race. Both
						// outcomes are legal; conflicts are retried once.
						for attempt := 0; attempt < 2; attempt++ {
							txn := svc.BeginUpdate()
							var stageErr error
							if (worker+i+attempt)%2 == 0 {
								stageErr = txn.SetWeight(pair("b", "x"), int64(worker*1000+i))
							} else {
								stageErr = txn.DeleteEdge(pair("b", "x"))
							}
							if stageErr != nil {
								txn.Rollback()
								break // edge already gone: nothing to serialize
							}
							if _, err := txn.Commit(); err != nil {
								if errors.Is(err, ErrTxnConflict) {
									continue
								}
								t.Errorf("commit shared: %v", err)
								return
							}
							committed.Add(1)
							break
						}
					}
				}(w)
			}
			wg.Wait()

			if got, want := svc.Version(), 1+committed.Load(); got != want {
				t.Fatalf("version = %d, want %d (hole or lost commit)", got, want)
			}
			g, err := svc.GraphAt(0)
			if err != nil {
				t.Fatal(err)
			}
			for w := 0; w < workers; w++ {
				for i := 0; i < iters; i++ {
					own := pair(fmt.Sprintf("w%d", w), fmt.Sprintf("n%d", i))
					if _, ok := g.Edges[own]; !ok {
						t.Fatalf("lost committed edge %v", own)
					}
				}
			}
			assertMatchesCold(t, svc, g)
		})
	}
}
