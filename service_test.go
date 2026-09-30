package matching

import (
	"fmt"
	"sync"
	"testing"
)

func sampleEdges(i int) []Edge {
	return []Edge{
		{U: "a", V: "x", Weight: int64(1 + i)},
		{U: "a", V: "y", Weight: int64(9 - i)},
		{U: "b", V: "x", Weight: int64(8)},
		{U: "b", V: "y", Weight: int64(1 + i)},
	}
}

func TestServiceQueryAndVersion(t *testing.T) {
	svc, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	v, err := svc.Update(Mutation{Upsert: sampleEdges(0)})
	if err != nil || v != 1 {
		t.Fatalf("update v=%d err=%v", v, err)
	}
	res, err := svc.Query(0, ModeMaxWeight, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 1 || res.TotalWeight != 17 {
		t.Fatalf("res=%+v", res)
	}

	// 历史版本快照可重复读取。
	v2, _ := svc.Update(Mutation{Upsert: []Edge{{U: "a", V: "x", Weight: 100}}})
	if v2 != 2 {
		t.Fatalf("v2=%d", v2)
	}
	r1, _ := svc.Query(1, ModeMaxWeight, nil)
	r2, _ := svc.Query(2, ModeMaxWeight, nil)
	if r1.TotalWeight != 17 {
		t.Fatalf("v1 weight changed: %d", r1.TotalWeight)
	}
	if r2.TotalWeight != 101 {
		t.Fatalf("v2 weight = %d, want 101", r2.TotalWeight)
	}
}

func TestServiceDeterministic(t *testing.T) {
	for trial := 0; trial < 5; trial++ {
		svc, _ := NewService(t.TempDir())
		for _, batch := range [][]Edge{sampleEdges(0), sampleEdges(1), sampleEdges(2)} {
			if _, err := svc.Update(Mutation{Upsert: batch}); err != nil {
				t.Fatal(err)
			}
		}
		var first *Result
		for i := 0; i < 20; i++ {
			r, err := svc.Query(3, ModeMaxWeight, nil)
			if err != nil {
				t.Fatal(err)
			}
			if first == nil {
				first = r
			} else if !resultsEqual(first, r) {
				t.Fatalf("nondeterministic result\n%v\n%v", first, r)
			}
		}
		svc.Close()
	}
}

func resultsEqual(a, b *Result) bool {
	if a.Version != b.Version || a.Mode != b.Mode || a.Cardinality != b.Cardinality || a.TotalWeight != b.TotalWeight {
		return false
	}
	if len(a.Matched) != len(b.Matched) {
		return false
	}
	for i := range a.Matched {
		if a.Matched[i] != b.Matched[i] {
			return false
		}
	}
	return true
}

// TestIncrementalEqualsFullRecompute 逐条增量更新得到的每个版本结果，
// 必须与用该版本全部边从头构建图求解的结果一致。
func TestIncrementalEqualsFullRecompute(t *testing.T) {
	svc, _ := NewService(t.TempDir())
	defer svc.Close()

	all := []Edge{
		{U: "a", V: "x", Weight: 3},
		{U: "b", V: "y", Weight: 7},
		{U: "a", V: "y", Weight: -2},
		{U: "c", V: "z", Weight: 5},
		{U: "b", V: "x", Weight: 4},
	}
	current := map[[2]string]int64{}
	for i, e := range all {
		if _, err := svc.Update(Mutation{Upsert: []Edge{e}}); err != nil {
			t.Fatal(err)
		}
		current[[2]string{e.U, e.V}] = e.Weight
		inc, err := svc.Query(int64(i+1), ModeMaxWeight, nil)
		if err != nil {
			t.Fatal(err)
		}
		full := solveFromEdges(int64(i+1), current, ModeMaxWeight, nil)
		if !resultsEqual(full, inc) {
			t.Fatalf("after %d edges mismatch\nfull=%+v\ninc =%+v", i+1, full, inc)
		}
	}

	// 删除后同样一致。
	for i, k := range [][2]string{{"a", "y"}, {"b", "x"}} {
		if _, err := svc.Update(Mutation{Delete: [][2]string{k}}); err != nil {
			t.Fatal(err)
		}
		delete(current, k)
		inc, _ := svc.Query(int64(len(all)+1+i), ModeMaxWeight, nil)
		full := solveFromEdges(inc.Version, current, ModeMaxWeight, nil)
		if !resultsEqual(full, inc) {
			t.Fatalf("delete mismatch\nfull=%+v\ninc =%+v", full, inc)
		}
	}
}

func solveFromEdges(version int64, m map[[2]string]int64, mode Mode, cons *Constraints) *Result {
	es := make([]Edge, 0, len(m))
	for k, w := range m {
		es = append(es, Edge{U: k[0], V: k[1], Weight: w})
	}
	g := gFrom(version, es)
	r, err := Solve(g, mode, cons)
	if err != nil {
		panic(err)
	}
	return r
}

// TestConcurrentUpdatesAndQueries 并发更新必须等价于某一串行顺序（版本号
// 连续无丢失），并发查询只读到完整版本，且结果与按最终顺序的串行结果一致。
func TestConcurrentUpdatesAndQueries(t *testing.T) {
	svc, _ := NewService(t.TempDir())
	defer svc.Close()

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	versions := make(chan int64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := svc.Update(Mutation{Upsert: []Edge{{U: "u", V: fmt.Sprintf("v%02d", i), Weight: int64(i)}}})
			if err != nil {
				errs <- err
				return
			}
			versions <- v
		}(i)
	}

	// 并发查询：任何时刻读到的必须是某个完整版本（这里只验证不 panic、
	// 结果自洽且版本号合法）。
	qdone := make(chan struct{})
	go func() {
		var qwg sync.WaitGroup
		for j := 0; j < 8; j++ {
			qwg.Add(1)
			go func() {
				defer qwg.Done()
				for k := 0; k < 200; k++ {
					r, err := svc.Query(0, ModeMaxCardinality, nil)
					if err == nil && r.Cardinality < 0 {
						panic("negative cardinality")
					}
				}
			}()
		}
		qwg.Wait()
		close(qdone)
	}()

	wg.Wait()
	close(errs)
	close(versions)
	for e := range errs {
		t.Fatal(e)
	}
	seen := map[int64]bool{}
	for v := range versions {
		if v < 1 || v > n || seen[v] {
			t.Fatalf("bad/dup version %d", v)
		}
		seen[v] = true
	}
	if len(seen) != n || svc.CurrentVersion() != n {
		t.Fatalf("lost updates: %d committed, latest %d", len(seen), svc.CurrentVersion())
	}
	<-qdone

	// 最终结果与按 0..n-1 串行应用一致。
	final, _ := svc.Query(0, ModeMaxCardinality, nil)
	m := map[[2]string]int64{}
	for i := 0; i < n; i++ {
		m[[2]string{"u", fmt.Sprintf("v%02d", i)}] = int64(i)
	}
	want := solveFromEdges(n, m, ModeMaxCardinality, nil)
	if !resultsEqual(want, final) {
		t.Fatalf("final mismatch\nwant=%+v\ngot =%+v", want, final)
	}
}

func TestServiceConstraintFingerprint(t *testing.T) {
	c1 := &Constraints{Forbidden: map[[2]string]struct{}{{"a", "x"}: {}, {"b", "y"}: {}}}
	c2 := &Constraints{Forbidden: map[[2]string]struct{}{{"b", "y"}: {}, {"a", "x"}: {}}}
	if fingerprintConstraints(c1) != fingerprintConstraints(c2) {
		t.Fatal("map order changed fingerprint")
	}
	if fingerprintConstraints(nil) == fingerprintConstraints(c1) {
		t.Fatal("nil vs constrained must differ")
	}
}
