package bipartite

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/guoyyit/bipartite-matching-go/matching"
)

func openSeedSvc(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "svc"), seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func edgeWeight(t *testing.T, svc *Service, p matching.Pair) int64 {
	t.Helper()
	g, err := svc.GraphAt(0)
	if err != nil {
		t.Fatal(err)
	}
	return g.Edges[p]
}

// assertSolutionsEqualCold verifies the latest committed state bit-for-bit
// against the expected graph for every scope: the live incremental solutions
// must equal from-scratch solves.
func assertSolutionsEqualCold(t *testing.T, svc *Service, expected *matching.Graph) {
	t.Helper()
	g, err := svc.GraphAt(0)
	if err != nil {
		t.Fatal(err)
	}
	assertGraphEqual(t, g, expected)
	for _, scope := range scopes() {
		cold, coldErr := matching.Solve(expected, scope)
		warm, warmErr := svc.Query(scope, 0)
		if (coldErr == nil) != (warmErr == nil) {
			t.Fatalf("scope %s feasibility differs cold=%v warm=%v", scope, coldErr, warmErr)
		}
		if coldErr != nil {
			var ci, wi *matching.InfeasibleError
			errors.As(coldErr, &ci)
			errors.As(warmErr, &wi)
			if ci == nil || wi == nil || ci.Code != wi.Code {
				t.Fatalf("scope %s error differs cold=%v warm=%v", scope, coldErr, warmErr)
			}
			continue
		}
		if cold.Weight != warm.Weight ||
			cold.Cardinality != warm.Cardinality ||
			fmt.Sprint(cold.Matched) != fmt.Sprint(warm.Matched) ||
			fmt.Sprint(cold.UnmatchedLeft) != fmt.Sprint(warm.UnmatchedLeft) ||
			fmt.Sprint(cold.UnmatchedRight) != fmt.Sprint(warm.UnmatchedRight) {
			t.Fatalf("scope %s mismatch cold=%+v warm=%+v", scope, cold, warm)
		}
	}
}

func assertGraphEqual(t *testing.T, got, want *matching.Graph) {
	t.Helper()
	if fmt.Sprint(got.Left) != fmt.Sprint(want.Left) ||
		fmt.Sprint(got.Right) != fmt.Sprint(want.Right) ||
		fmt.Sprint(got.Edges) != fmt.Sprint(want.Edges) ||
		fmt.Sprint(got.Forbidden) != fmt.Sprint(want.Forbidden) ||
		fmt.Sprint(got.LeftCap) != fmt.Sprint(want.LeftCap) ||
		fmt.Sprint(got.RightCap) != fmt.Sprint(want.RightCap) {
		t.Fatalf("graph mismatch\ngot  left=%v right=%v edges=%v forbid=%v capsL=%v capsR=%v\nwant left=%v right=%v edges=%v forbid=%v capsL=%v capsR=%v",
			got.Left, got.Right, got.Edges, got.Forbidden, got.LeftCap, got.RightCap,
			want.Left, want.Right, want.Edges, want.Forbidden, want.LeftCap, want.RightCap)
	}
}

// Two transactions taken from the same committed version touch disjoint
// data. Both successful commits must survive: the final state equals the
// two changes applied serially in commit order.
func TestOverlappingTxnsDistinctChangesMerge(t *testing.T) {
	svc := openSeedSvc(t)
	defer svc.Close()

	t1 := svc.BeginUpdate()
	t2 := svc.BeginUpdate()
	if err := t1.AddEdge(pair("a", "z"), 30); err != nil {
		t.Fatal(err)
	}
	if err := t2.SetWeight(pair("b", "y"), 25); err != nil {
		t.Fatal(err)
	}

	v1, err := t1.Commit()
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}
	v2, err := t2.Commit()
	if err != nil {
		t.Fatalf("stale-but-mergeable commit: %v", err)
	}
	if v1 != 2 || v2 != 3 {
		t.Fatalf("versions = %d,%d want 2,3", v1, v2)
	}

	if got := edgeWeight(t, svc, pair("a", "z")); got != 30 {
		t.Fatalf("first txn change lost: a/z = %d want 30", got)
	}
	if got := edgeWeight(t, svc, pair("b", "y")); got != 25 {
		t.Fatalf("second txn change lost: b/y = %d want 25", got)
	}

	serial := seedGraph()
	serial.Right = append(serial.Right, "z")
	serial.Edges[pair("a", "z")] = 30
	serial.Edges[pair("b", "y")] = 25
	assertSolutionsEqualCold(t, svc, serial)
}

// Two transactions reweight the same edge. The later committer's value wins,
// in both commit orders, and the result equals that serial schedule.
func TestOverlappingTxnsSameEdgeLastCommitterWins(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "forward"
		if reverse {
			name = "reverse"
		}
		t.Run(name, func(t *testing.T) {
			svc := openSeedSvc(t)
			defer svc.Close()

			t1 := svc.BeginUpdate()
			t2 := svc.BeginUpdate()
			if err := t1.SetWeight(pair("a", "x"), 100); err != nil {
				t.Fatal(err)
			}
			if err := t2.SetWeight(pair("a", "x"), 200); err != nil {
				t.Fatal(err)
			}

			var want int64
			if !reverse {
				if _, err := t1.Commit(); err != nil {
					t.Fatal(err)
				}
				if _, err := t2.Commit(); err != nil {
					t.Fatal(err)
				}
				want = 200
			} else {
				if _, err := t2.Commit(); err != nil {
					t.Fatal(err)
				}
				if _, err := t1.Commit(); err != nil {
					t.Fatal(err)
				}
				want = 100
			}
			if got := edgeWeight(t, svc, pair("a", "x")); got != want {
				t.Fatalf("a/x = %d want later committer %d", got, want)
			}

			serial := seedGraph()
			serial.Edges[pair("a", "x")] = want
			assertSolutionsEqualCold(t, svc, serial)
		})
	}
}

// Same constraint key written from both transactions: later committer wins,
// while disjoint constraint changes merge.
func TestOverlappingTxnsSameConstraintLastCommitterWins(t *testing.T) {
	svc := openSeedSvc(t)
	defer svc.Close()

	t1 := svc.BeginUpdate()
	t2 := svc.BeginUpdate()
	if err := t1.SetLeftCap("a", 2); err != nil {
		t.Fatal(err)
	}
	if err := t2.SetLeftCap("a", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := t1.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := t2.Commit(); err != nil {
		t.Fatal(err)
	}
	g, _ := svc.GraphAt(0)
	if got := g.LeftCap["a"]; got != 1 {
		t.Fatalf("cap a = %d want later committer 1", got)
	}

	t3 := svc.BeginUpdate()
	t4 := svc.BeginUpdate()
	if err := t3.Forbid(pair("a", "y")); err != nil {
		t.Fatal(err)
	}
	if err := t4.Forbid(pair("b", "x")); err != nil {
		t.Fatal(err)
	}
	if _, err := t3.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := t4.Commit(); err != nil {
		t.Fatal(err)
	}
	g, _ = svc.GraphAt(0)
	if _, banned := g.Forbidden[pair("a", "y")]; !banned {
		t.Fatal("later commit forbidding a/y lost")
	}
	if _, banned := g.Forbidden[pair("b", "x")]; !banned {
		t.Fatal("merge of disjoint forbid b/x lost")
	}

	serial := seedGraph()
	serial.LeftCap["a"] = 1
	serial.Forbidden[pair("a", "y")] = struct{}{}
	serial.Forbidden[pair("b", "x")] = struct{}{}
	assertSolutionsEqualCold(t, svc, serial)
}

// A stale txn that modifies an edge a concurrent commit deleted cannot be
// replayed serially: it is rejected with the distinct, retryable
// ErrConflict, publishes nothing, and a fresh retry from the new latest
// version succeeds.
func TestConflictRejectedAndRetrySucceeds(t *testing.T) {
	svc := openSeedSvc(t)
	defer svc.Close()

	t1 := svc.BeginUpdate()
	t2 := svc.BeginUpdate()
	if err := t1.DeleteEdge(pair("a", "x")); err != nil {
		t.Fatal(err)
	}
	if err := t2.SetWeight(pair("a", "x"), 50); err != nil {
		t.Fatal(err)
	}

	v1, err := t1.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := t2.Commit(); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale modify-after-delete err = %v, want ErrConflict", err)
	}

	// Rejection published and persisted nothing.
	if svc.Version() != v1 {
		t.Fatalf("version = %d want %d after rejected commit", svc.Version(), v1)
	}
	g, _ := svc.GraphAt(0)
	if _, exists := g.Edges[pair("a", "x")]; exists {
		t.Fatal("edge reappeared after rejected commit")
	}

	// Caller retries on a brand-new transaction based on the latest state.
	retry := svc.BeginUpdate()
	if err := retry.AddEdge(pair("a", "x"), 50); err != nil {
		t.Fatal(err)
	}
	v3, err := retry.Commit()
	if err != nil {
		t.Fatalf("retry commit: %v", err)
	}
	if v3 != v1+1 {
		t.Fatalf("retry version = %d want %d", v3, v1+1)
	}
	if got := edgeWeight(t, svc, pair("a", "x")); got != 50 {
		t.Fatalf("retried edge weight = %d want 50", got)
	}

	serial := seedGraph()
	serial.Edges[pair("a", "x")] = 50
	assertSolutionsEqualCold(t, svc, serial)
}

// A rejected transaction is dead: neither recommit nor further mutation can
// succeed and none of its staged changes leak into committed state.
func TestRejectedTxnCannotBeReused(t *testing.T) {
	svc := openSeedSvc(t)
	defer svc.Close()

	t1 := svc.BeginUpdate()
	t2 := svc.BeginUpdate()
	if err := t1.DeleteEdge(pair("a", "y")); err != nil {
		t.Fatal(err)
	}
	if err := t2.SetWeight(pair("a", "y"), 7); err != nil {
		t.Fatal(err)
	}
	if _, err := t1.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := t2.Commit(); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v want ErrConflict", err)
	}
	if _, err := t2.Commit(); !errors.Is(err, ErrTxnRolledBack) {
		t.Fatalf("recommit err = %v want ErrTxnRolledBack", err)
	}
	if err := t2.SetWeight(pair("b", "y"), 1); !errors.Is(err, ErrTxnRolledBack) {
		t.Fatalf("post-reject mutation err = %v want ErrTxnRolledBack", err)
	}

	g, _ := svc.GraphAt(0)
	if _, exists := g.Edges[pair("a", "y")]; exists {
		t.Fatal("rejected txn leaked its base assumption")
	}
	if g.Edges[pair("b", "y")] != 8 {
		t.Fatalf("post-reject mutation leaked: b/y = %d want 8", g.Edges[pair("b", "y")])
	}
}

// Rolled-back transactions consume no version and leak no change even when
// interleaved with an overlapping commit.
func TestRollbackAndCommitInterleaving(t *testing.T) {
	svc := openSeedSvc(t)
	defer svc.Close()

	t1 := svc.BeginUpdate()
	t2 := svc.BeginUpdate()
	if err := t1.SetWeight(pair("a", "x"), 111); err != nil {
		t.Fatal(err)
	}
	if err := t2.SetWeight(pair("a", "x"), 222); err != nil {
		t.Fatal(err)
	}
	t1.Rollback()
	if err := t1.SetWeight(pair("a", "x"), 1); !errors.Is(err, ErrTxnRolledBack) {
		t.Fatalf("rolled-back mutation err = %v", err)
	}
	if _, err := t1.Commit(); !errors.Is(err, ErrTxnRolledBack) {
		t.Fatalf("rolled-back commit err = %v", err)
	}
	v, err := t2.Commit()
	if err != nil {
		t.Fatalf("t2 commit: %v", err)
	}
	if v != 2 || svc.Version() != 2 {
		t.Fatalf("version = %d (head %d), rollback must not consume a version", v, svc.Version())
	}
	if got := edgeWeight(t, svc, pair("a", "x")); got != 222 {
		t.Fatalf("a/x = %d want 222", got)
	}

	serial := seedGraph()
	serial.Edges[pair("a", "x")] = 222
	assertSolutionsEqualCold(t, svc, serial)
}

// Concurrent stress with overlapping base versions: every successful commit
// survives, head advances exactly once per success, and every final edge has
// the value of its last successful writer. Run under -race.
func TestConcurrentOverlappingStress(t *testing.T) {
	svc := openSeedSvc(t)
	defer svc.Close()

	const workers = 8
	const rounds = 20
	shared := []matching.Pair{pair("a", "x"), pair("b", "y")}

	var wg sync.WaitGroup
	var mu sync.Mutex
	committed := map[matching.Pair]int64{}
	var committedCount, conflictCount int

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := 0; round < rounds; round++ {
				txn := svc.BeginUpdate()
				value := int64(worker*1000 + round)
				ownL := fmt.Sprintf("w%d", worker)
				ownR := fmt.Sprintf("r%d", round)
				if err := txn.AddEdge(pair(ownL, ownR), value); err != nil {
					t.Errorf("add own edge: %v", err)
					return
				}
				target := shared[round%len(shared)]
				if err := txn.SetWeight(target, value); err != nil {
					t.Errorf("set shared weight: %v", err)
					return
				}
				if _, err := txn.Commit(); err != nil {
					if errors.Is(err, ErrConflict) {
						mu.Lock()
						conflictCount++
						mu.Unlock()
						continue
					}
					t.Errorf("commit: %v", err)
					return
				}
				mu.Lock()
				committedCount++
				committed[matching.Pair{Left: ownL, Right: ownR}] = value
				committed[target] = value
				mu.Unlock()
			}
		}(w)
	}

	// Readers continuously observe complete, non-negative snapshots.
	stop := make(chan struct{})
	var readerWg sync.WaitGroup
	for r := 0; r < 4; r++ {
		readerWg.Add(1)
		go func() {
			defer readerWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					for _, scope := range scopes() {
						if sol, err := svc.Query(scope, 0); err == nil && sol.Weight < 0 {
							t.Errorf("impossible weight %d", sol.Weight)
						}
					}
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	readerWg.Wait()

	if committedCount == 0 || committedCount+conflictCount != workers*rounds {
		t.Fatalf("committed=%d conflicts=%d total=%d", committedCount, conflictCount, workers*rounds)
	}
	if svc.Version() != int64(1+committedCount) {
		t.Fatalf("head = %d want %d", svc.Version(), 1+committedCount)
	}

	g, err := svc.GraphAt(0)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range committed {
		if got := g.Edges[key]; got != want {
			t.Fatalf("final %s/%s = %d want last committed %d", key.Left, key.Right, got, want)
		}
	}

	// Final incremental solutions must match from-scratch solves bit-for-bit.
	assertSolutionsEqualCold(t, svc, g)
}
