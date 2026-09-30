package matching

import (
	"math/rand"
	"sort"
	"testing"
)

// bruteSolve 用枚举法计算正确答案。
// 返回 (基数, 总权, 按边键排序的包含向量 bool 列表)。
// perfect 要求每个顶点恰好出现一次；capL/capR 限制顶点出现次数（缺省 1）。
func bruteSolve(edges []Edge, mode Mode, forbid map[[2]string]struct{}, capL, capR map[string]int) (int, int64, []bool) {
	n := len(edges)
	bestK := -1
	var bestW int64
	var bestMask []bool

	eval := func(mask []bool) (int, int64, bool) {
		useL := map[string]int{}
		useR := map[string]int{}
		seenV := map[string]struct{}{}
		k := 0
		var w int64
		for i, on := range mask {
			if !on {
				continue
			}
			e := edges[i]
			if _, bad := forbid[[2]string{e.U, e.V}]; bad {
				return 0, 0, false
			}
			useL[e.U]++
			useR[e.V]++
			seenV[e.U] = struct{}{}
			seenV[e.V] = struct{}{}
			k++
			w += e.Weight
		}
		for v, c := range useL {
			if c > capOf(capL, v) {
				return 0, 0, false
			}
		}
		for v, c := range useR {
			if c > capOf(capR, v) {
				return 0, 0, false
			}
		}
		if mode == ModePerfect {
			allL := map[string]struct{}{}
			allR := map[string]struct{}{}
			for _, e := range edges {
				allL[e.U] = struct{}{}
				allR[e.V] = struct{}{}
			}
			for v := range allL {
				if useL[v] != 1 {
					return 0, 0, false
				}
			}
			for v := range allR {
				if useR[v] != 1 {
					return 0, 0, false
				}
			}
		}
		return k, w, true
	}

	mask := make([]bool, n)
	var rec func(int)
	rec = func(i int) {
		if i == n {
			k, w, ok := eval(mask)
			if !ok {
				return
			}
			take := false
			if bestMask == nil {
				take = true
			} else {
				dominates := false
				switch mode {
				case ModePerfect, ModeMaxCardinality:
					dominates = k > bestK || (k == bestK && w > bestW)
				case ModeMaxWeight:
					dominates = w > bestW
				}
				equalObj := k == bestK && w == bestW
				take = dominates || (equalObj && lexMaskLess(mask, bestMask))
			}
			if take {
				bestK = k
				bestW = w
				bestMask = append([]bool(nil), mask...)
			}
			return
		}
		mask[i] = false
		rec(i + 1)
		mask[i] = true
		rec(i + 1)
	}
	rec(0)
	return bestK, bestW, bestMask
}

// lexMaskLess 比较“按字典序列出的匹配边序列”：跳过未选边，比较首个选中键。
func lexMaskLess(a, b []bool) bool {
	for i := range a {
		if a[i] == b[i] {
			continue
		}
		// 首个分歧位上 a 选中（true）即 a 的首条匹配边更小。
		return a[i]
	}
	return false
}

func TestExhaustiveVsBrute(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	lefts := []string{"a", "b", "c"}
	rights := []string{"x", "y", "z"}

	for iter := 0; iter < 300; iter++ {
		var edges []Edge
		for _, u := range lefts {
			for _, v := range rights {
				if rng.Intn(2) == 0 {
					edges = append(edges, Edge{U: u, V: v, Weight: int64(rng.Intn(11) - 5)})
				}
			}
		}
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].U != edges[j].U {
				return edges[i].U < edges[j].U
			}
			return edges[i].V < edges[j].V
		})

		var forbid map[[2]string]struct{}
		var capL, capR map[string]int
		if rng.Intn(3) == 0 && len(edges) > 0 {
			forbid = map[[2]string]struct{}{}
			for k := 0; k < 1+rng.Intn(2); k++ {
				e := edges[rng.Intn(len(edges))]
				forbid[[2]string{e.U, e.V}] = struct{}{}
			}
		}
		if rng.Intn(4) == 0 {
			capL = map[string]int{lefts[rng.Intn(len(lefts))]: 2}
		}
		if rng.Intn(4) == 0 {
			capR = map[string]int{rights[rng.Intn(len(rights))]: 2}
		}

		g := gFrom(int64(iter), edges)
		cons := &Constraints{Forbidden: forbid, MaxLeft: capL, MaxRight: capR}

		for _, mode := range []Mode{ModeMaxWeight, ModeMaxCardinality, ModePerfect} {
			res, err := Solve(g, mode, cons)
			bk, bw, bmask := bruteSolve(edges, mode, forbid, capL, capR)

			if bmask == nil {
				if mode == ModePerfect {
					if err == nil || failReason(t, err) != FailurePerfectInfeasible {
						t.Fatalf("iter %d mode %v: want infeasible, got %+v err=%v", iter, mode, res, err)
					}
					continue
				}
				t.Fatalf("iter %d: brute found no solution in non-perfect mode", iter)
			}
			if err != nil {
				t.Fatalf("iter %d mode %v: unexpected err %v", iter, mode, err)
			}
			if res.Cardinality != bk || res.TotalWeight != bw {
				t.Fatalf("iter %d mode %v: got (k=%d,w=%d), brute (k=%d,w=%d); edges=%v",
					iter, mode, res.Cardinality, res.TotalWeight, bk, bw, edges)
			}
			gotMask := make([]bool, len(edges))
			em := map[[2]string]int{}
			for i, e := range edges {
				em[[2]string{e.U, e.V}] = i
			}
			for _, m := range res.Matched {
				gotMask[em[[2]string{m.U, m.V}]] = true
			}
			if !masksEqual(gotMask, bmask) {
				_, _, freshMask := bruteSolve(edges, mode, forbid, capL, capR)
				t.Fatalf("iter %d mode %v: tie-break mismatch k=%d/%d w=%d/%d forbid=%v capL=%v capR=%v\n got %v\nwant %v\nfresh %v\nedges=%v",
					iter, mode, res.Cardinality, bk, res.TotalWeight, bw, forbid, capL, capR, gotMask, bmask, freshMask, edges)
				_ = freshMask
			}
		}
	}
}

func masksEqual(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
