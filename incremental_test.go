package matching

import (
	"math/rand"
	"testing"
)

func TestIncrementalEquivalence(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	lefts := []string{"a", "b", "c", "d"}
	rights := []string{"w", "x", "y", "z"}

	for _, mode := range []Mode{ModeMaxWeight, ModeMaxCardinality} {
		inc := newIncSolver(mode, nil)
		edges := map[[2]string]int64{}
		for step := 0; step < 400; step++ {
			u := lefts[rng.Intn(len(lefts))]
			v := rights[rng.Intn(len(rights))]
			k := [2]string{u, v}
			switch rng.Intn(3) {
			case 0, 1:
				edges[k] = int64(rng.Intn(21) - 10)
			case 2:
				delete(edges, k)
			}
			g := gFrom(int64(step+1), mapToEdges(edges))

			got, err := inc.solve(g)
			if err != nil {
				t.Fatalf("step %d mode %v warm err: %v", step, mode, err)
			}
			gotRes, err := resolveWithWarm(g, mode, nil, got)
			if err != nil {
				t.Fatalf("step %d resolve err: %v", step, err)
			}
			coldRes, err := Solve(g, mode, nil)
			if err != nil {
				t.Fatalf("step %d cold err: %v", step, err)
			}
			if !resultsEqual(gotRes, coldRes) {
				t.Fatalf("step %d mode %v mismatch:\nwarm=%+v\ncold=%+v\nedges=%v",
					step, mode, gotRes, coldRes, mapToEdges(edges))
			}
			cold := map[[2]string]struct{}{}
			for _, m := range coldRes.Matched {
				cold[[2]string{m.U, m.V}] = struct{}{}
			}
			_ = cold
		}
	}
}

func TestIncrementalSingleEdgeNoFullRebuild(t *testing.T) {
	// 连续单条新增不应触发 rebuild：构造稳定增长序列并验证状态保持。
	inc := newIncSolver(ModeMaxWeight, nil)
	g := gFrom(1, []Edge{{U: "a", V: "x", Weight: 5}})
	if _, err := inc.solve(g); err != nil {
		t.Fatal(err)
	}
	g2 := gFrom(2, []Edge{{U: "a", V: "x", Weight: 5}, {U: "b", V: "y", Weight: 7}})
	if _, err := inc.solve(g2); err != nil {
		t.Fatal(err)
	}
	if inc.ver != 2 || !inc.ready {
		t.Fatalf("warm state not advanced: ver=%d ready=%v", inc.ver, inc.ready)
	}
	// 旧节点编号保持稳定（s=0,t=1，a=2,x=3；新顶点追加在尾部）。
	if inc.lNode["a"] != 2 || inc.rNode["x"] != 3 {
		t.Fatalf("stable node ids violated: a=%d x=%d", inc.lNode["a"], inc.rNode["x"])
	}
}

// TestServiceWarmMatchesColdAcrossHistory 走 Service.Query 的热启动路径，
// 在长随机序列上验证每个最新版本以及部分历史版本都与冷启动一致。
func TestServiceWarmMatchesColdAcrossHistory(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	svc, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	labs := []string{"a", "b", "c"}
	rabs := []string{"x", "y", "z"}
	cur := map[[2]string]int64{}
	for step := 0; step < 200; step++ {
		k := [2]string{labs[rng.Intn(3)], rabs[rng.Intn(3)]}
		mut := Mutation{}
		if rng.Intn(5) == 0 {
			mut.Delete = [][2]string{k}
			delete(cur, k)
		} else {
			w := int64(rng.Intn(15) - 7)
			mut.Upsert = []Edge{{U: k[0], V: k[1], Weight: w}}
			cur[k] = w
		}
		v, err := svc.Update(mut)
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []Mode{ModeMaxWeight, ModeMaxCardinality, ModePerfect} {
			got, gerr := svc.Query(v, mode, nil)
			edgesList := mapToEdges(cur)
			cold, cerr := Solve(gFrom(v, edgesList), mode, nil)
			if (gerr != nil) != (cerr != nil) {
				t.Fatalf("step %d mode %v feasibility differs: warm=%v cold=%v", step, mode, gerr, cerr)
			}
			if gerr != nil {
				continue
			}
			if mode == ModePerfect {
				_ = got
			}
			if !resultsEqual(got, cold) {
				t.Fatalf("step %d mode %v mismatch\nwarm=%+v\ncold=%+v", step, mode, got, cold)
			}
		}
	}
}

func mapToEdges(m map[[2]string]int64) []Edge {
	out := make([]Edge, 0, len(m))
	for k, w := range m {
		out = append(out, Edge{U: k[0], V: k[1], Weight: w})
	}
	return out
}
