package bipartite

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/guoyyit/bipartite-matching-go/matching"
	"github.com/guoyyit/bipartite-matching-go/store"
)

type versionCapture struct {
	graph     *matching.Graph
	solutions map[matching.Scope]*matching.Solution
	codes     map[matching.Scope]matching.FailureCode
}

func captureAllVersions(t *testing.T, svc *Service, latest int64) map[int64]versionCapture {
	t.Helper()
	out := map[int64]versionCapture{}
	for v := int64(1); v <= latest; v++ {
		g, err := svc.GraphAt(v)
		if err != nil {
			t.Fatalf("graph version %d: %v", v, err)
		}
		cap := versionCapture{
			graph:     g,
			solutions: map[matching.Scope]*matching.Solution{},
			codes:     map[matching.Scope]matching.FailureCode{},
		}
		for _, scope := range scopes() {
			sol, err := svc.Query(scope, v)
			if err != nil {
				var infeasible *matching.InfeasibleError
				if !errors.As(err, &infeasible) {
					t.Fatalf("version %d scope %s: %v", v, scope, err)
				}
				cap.codes[scope] = infeasible.Code
				continue
			}
			cap.solutions[scope] = sol
		}
		out[v] = cap
	}
	return out
}

func assertCapturesEqual(t *testing.T, before, after map[int64]versionCapture) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("retained version sets differ: %d before, %d after", len(before), len(after))
	}
	for v, want := range before {
		got, ok := after[v]
		if !ok {
			t.Fatalf("version %d unreadable after restart", v)
		}
		assertGraphEqual(t, got.graph, want.graph)
		for _, scope := range scopes() {
			ws, wSol := want.solutions[scope]
			gs, gSol := got.solutions[scope]
			if wSol != gSol {
				t.Fatalf("v%d scope %s solution presence differs", v, scope)
			}
			if !wSol {
				if want.codes[scope] != got.codes[scope] {
					t.Fatalf("v%d scope %s code %q vs %q", v, scope, got.codes[scope], want.codes[scope])
				}
				continue
			}
			if ws.Weight != gs.Weight || ws.Cardinality != gs.Cardinality ||
				fmt.Sprint(ws.Matched) != fmt.Sprint(gs.Matched) ||
				fmt.Sprint(ws.UnmatchedLeft) != fmt.Sprint(gs.UnmatchedLeft) ||
				fmt.Sprint(ws.UnmatchedRight) != fmt.Sprint(gs.UnmatchedRight) {
				t.Fatalf("v%d scope %s differs before/after restart:\nbefore=%+v\nafter =%+v", v, scope, ws, gs)
			}
		}
	}
}

// Every historical version readable before restart stays readable after,
// with bit-for-bit identical graph and all three solve scopes.
func TestHistorySurvivesRestartBitForBit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "svc")
	svc, err := Open(dir, seedGraph())
	if err != nil {
		t.Fatal(err)
	}

	// A mixed stream: add/delete/reweight edges and toggle constraints so some
	// versions make scopes infeasible.
	mutate := func(t *testing.T, txn *Txn, step int) {
		t.Helper()
		switch step % 5 {
		case 0:
			p := pair("a", fmt.Sprintf("h%d", step))
			if err := txn.AddEdge(p, int64(step+1)); err != nil {
				t.Fatalf("add %v: %v", p, err)
			}
		case 1:
			if err := txn.SetWeight(pair("a", "x"), int64(step)); err != nil {
				t.Fatalf("reweight: %v", err)
			}
		case 2:
			if err := txn.Forbid(pair("b", "y")); err != nil {
				t.Fatalf("forbid: %v", err)
			}
		case 3:
			if err := txn.Allow(pair("b", "y")); err != nil {
				t.Fatalf("allow: %v", err)
			}
		default:
			if err := txn.SetRightCap("y", 1-step%2); err != nil {
				t.Fatalf("cap: %v", err)
			}
		}
	}

	const commits = 40
	for step := 0; step < commits; step++ {
		txn := svc.BeginUpdate()
		mutate(t, txn, step)
		if _, err := txn.Commit(); err != nil {
			t.Fatalf("commit %d: %v", step, err)
		}
	}

	latest := svc.Version()
	before := captureAllVersions(t, svc, latest)
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Version() != latest {
		t.Fatalf("latest after restart = %d want %d", reopened.Version(), latest)
	}
	after := captureAllVersions(t, reopened, latest)
	assertCapturesEqual(t, before, after)
}

// After restart, new commits keep incrementally solving, and the new
// versions themselves survive a further restart.
func TestIncrementalAfterRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "svc")
	svc, err := Open(dir, seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 10; step++ {
		txn := svc.BeginUpdate()
		if err := txn.SetWeight(pair("a", "x"), int64(20+step)); err != nil {
			t.Fatal(err)
		}
		if _, err := txn.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	for step := 0; step < 10; step++ {
		txn := reopened.BeginUpdate()
		if err := txn.AddEdge(pair(fmt.Sprintf("n%d", step), "x"), int64(30+step)); err != nil {
			t.Fatal(err)
		}
		if _, err := txn.Commit(); err != nil {
			t.Fatalf("post-restart commit %d: %v", step, err)
		}
		// Every post-restart incremental result equals a cold solve.
		g, err := reopened.GraphAt(0)
		if err != nil {
			t.Fatal(err)
		}
		for _, scope := range scopes() {
			cold, coldErr := matching.Solve(g, scope)
			warm, warmErr := reopened.Query(scope, 0)
			if (coldErr == nil) != (warmErr == nil) {
				t.Fatalf("step %d scope %s feasibility cold=%v warm=%v", step, scope, coldErr, warmErr)
			}
			if coldErr != nil {
				continue
			}
			if cold.Weight != warm.Weight ||
				fmt.Sprint(cold.Matched) != fmt.Sprint(warm.Matched) ||
				cold.Cardinality != warm.Cardinality {
				t.Fatalf("step %d scope %s cold=%+v warm=%+v", step, scope, cold, warm)
			}
		}
	}

	latest := reopened.Version()
	before := captureAllVersions(t, reopened, latest)
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	after := captureAllVersions(t, again, latest)
	assertCapturesEqual(t, before, after)
}

// The retention window is configurable, durable across restarts, and pruned
// versions are reported with the same distinct not-found error everywhere.
func TestHistoryRetentionConfigurable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "svc")
	svc, err := Open(dir, seedGraph(), store.WithHistory(5))
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 20; step++ {
		txn := svc.BeginUpdate()
		if err := txn.SetWeight(pair("a", "x"), int64(step)); err != nil {
			t.Fatal(err)
		}
		if _, err := txn.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	latest := svc.Version()
	wantOldest := latest - 5 + 1

	// Pre-restart window.
	if _, err := svc.GraphAt(wantOldest - 1); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("pruned read err = %v want ErrUnknownVersion", err)
	}
	if _, err := svc.GraphAt(wantOldest); err != nil {
		t.Fatalf("oldest retained version: %v", err)
	}
	if _, err := svc.Query(matching.ScopeMaxWeight, latest+1); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("future version err = %v want ErrUnknownVersion", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen without re-specifying: the persisted limit still applies.
	reopened, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.storage.History(); got != 5 {
		t.Fatalf("history after reopen = %d want 5", got)
	}
	if _, err := reopened.GraphAt(wantOldest - 1); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("pruned after restart err = %v want ErrUnknownVersion", err)
	}
	for v := wantOldest; v <= latest; v++ {
		if _, err := reopened.GraphAt(v); err != nil {
			t.Fatalf("retained %d unreadable after restart: %v", v, err)
		}
	}

	// Continuing to commit keeps exactly the newest 5 versions readable.
	for step := 0; step < 5; step++ {
		txn := reopened.BeginUpdate()
		if err := txn.SetWeight(pair("b", "y"), int64(step+100)); err != nil {
			t.Fatal(err)
		}
		if _, err := txn.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	newLatest := reopened.Version()
	if newLatest != latest+5 {
		t.Fatalf("latest = %d want %d", newLatest, latest+5)
	}
	if _, err := reopened.GraphAt(newLatest - 5); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("newly pruned version err = %v", err)
	}
	if _, err := reopened.GraphAt(newLatest - 4); err != nil {
		t.Fatalf("retained boundary: %v", err)
	}
}

// Unlimited retention keeps every version, including across a restart.
func TestHistoryUnlimited(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "svc")
	svc, err := Open(dir, seedGraph(), store.WithHistory(0))
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 10; step++ {
		txn := svc.BeginUpdate()
		if err := txn.SetWeight(pair("a", "x"), int64(step)); err != nil {
			t.Fatal(err)
		}
		if _, err := txn.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for v := int64(1); v <= reopened.Version(); v++ {
		if _, err := reopened.GraphAt(v); err != nil {
			t.Fatalf("unlimited history missing v%d: %v", v, err)
		}
	}
}

// Shrinking the retention window on reopen takes effect immediately and
// consistently before/after restart.
func TestHistoryShrinkOnReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "svc")
	svc, err := Open(dir, seedGraph(), store.WithHistory(0))
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 12; step++ {
		txn := svc.BeginUpdate()
		if err := txn.SetWeight(pair("a", "x"), int64(step)); err != nil {
			t.Fatal(err)
		}
		if _, err := txn.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	latest := svc.Version()
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	shrunk, err := Open(dir, nil, store.WithHistory(3))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shrunk.GraphAt(latest - 3); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("expected shrink prune, got %v", err)
	}
	for v := latest - 2; v <= latest; v++ {
		if _, err := shrunk.GraphAt(v); err != nil {
			t.Fatalf("retained v%d: %v", v, err)
		}
	}
	if err := shrunk.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.GraphAt(latest - 3); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("window changed across restart: %v", err)
	}
}

// A directory written by the pre-fix build (no meta.json) reopens directly
// with no migration and all its originally-readable history stays readable.
func TestOpenLegacyServiceDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "svc")
	svc, err := Open(dir, seedGraph())
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 6; step++ {
		txn := svc.BeginUpdate()
		if err := txn.SetWeight(pair("a", "x"), int64(step)); err != nil {
			t.Fatal(err)
		}
		if _, err := txn.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	latest := svc.Version()
	captured := captureAllVersions(t, svc, latest)
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	// Remove the meta file the new build lazily writes, mimicking a real
	// pre-fix directory.
	removeIfExists(t, filepath.Join(dir, "meta.json"))

	reopened, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("legacy directory must open directly: %v", err)
	}
	defer reopened.Close()
	assertCapturesEqual(t, captured, captureAllVersions(t, reopened, latest))
}

func removeIfExists(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
