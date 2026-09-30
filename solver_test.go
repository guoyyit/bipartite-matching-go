package matching

import (
	"errors"
	"testing"
)

func gFrom(version int64, es []Edge) *Graph {
	m := make(map[[2]string]int64, len(es))
	for _, e := range es {
		m[[2]string{e.U, e.V}] = e.Weight
	}
	return newGraph(version, m)
}

func failReason(t *testing.T, err error) FailureReason {
	t.Helper()
	var se *SolveError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SolveError, got %v", err)
	}
	return se.Reason
}

func TestMaxWeightBasic(t *testing.T) {
	g := gFrom(1, []Edge{
		{U: "a", V: "x", Weight: 5},
		{U: "a", V: "y", Weight: 9},
		{U: "b", V: "x", Weight: 8},
		{U: "b", V: "y", Weight: 1},
	})
	res, err := Solve(g, ModeMaxWeight, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cardinality != 2 || res.TotalWeight != 17 {
		t.Fatalf("got k=%d w=%d, want k=2 w=17", res.Cardinality, res.TotalWeight)
	}
	if res.Matched[0].U != "a" || res.Matched[0].V != "y" {
		t.Fatalf("unexpected match %+v", res.Matched[0])
	}
	if len(res.UnmatchedLeft) != 0 || len(res.UnmatchedRight) != 0 {
		t.Fatalf("expected no unmatched, got %v %v", res.UnmatchedLeft, res.UnmatchedRight)
	}
}

func TestMaxWeightSkipsNegative(t *testing.T) {
	g := gFrom(1, []Edge{
		{U: "a", V: "x", Weight: -3},
		{U: "b", V: "y", Weight: 2},
	})
	res, err := Solve(g, ModeMaxWeight, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cardinality != 1 || res.TotalWeight != 2 {
		t.Fatalf("got k=%d w=%d", res.Cardinality, res.TotalWeight)
	}
	if len(res.UnmatchedLeft) != 1 || res.UnmatchedLeft[0] != "a" {
		t.Fatalf("expected unmatched a, got %v", res.UnmatchedLeft)
	}
}

func TestMaxCardinality(t *testing.T) {
	g := gFrom(1, []Edge{
		{U: "a", V: "x", Weight: 100},
		{U: "b", V: "x", Weight: 1},
		{U: "b", V: "y", Weight: 1},
	})
	res, err := Solve(g, ModeMaxCardinality, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 唯一的两对匹配是 a-x + b-y，总权 101；基数优先必须取它。
	if res.Cardinality != 2 || res.TotalWeight != 101 {
		t.Fatalf("cardinality-first: got k=%d w=%d, want k=2 w=101", res.Cardinality, res.TotalWeight)
	}
}

func TestMaxCardinalityBeatsWeight(t *testing.T) {
	// 单对匹配最高权为 100；基数优先必须取两对匹配，即使其总权 101 仅略高，
	// 并且通过负权边构造确保基数主导费用确实压过权重。
	g := gFrom(1, []Edge{
		{U: "a", V: "x", Weight: 1000},
		{U: "b", V: "x", Weight: 1},
		{U: "b", V: "y", Weight: -500},
	})
	res, err := Solve(g, ModeMaxCardinality, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cardinality != 2 || res.TotalWeight != 500 {
		t.Fatalf("got k=%d w=%d, want k=2 w=500", res.Cardinality, res.TotalWeight)
	}
	// 最大权模式则应只取 a-x=1000。
	wres, err := Solve(g, ModeMaxWeight, nil)
	if err != nil {
		t.Fatal(err)
	}
	if wres.Cardinality != 1 || wres.TotalWeight != 1000 {
		t.Fatalf("maxweight got k=%d w=%d, want k=1 w=1000", wres.Cardinality, wres.TotalWeight)
	}
}

func TestPerfectModes(t *testing.T) {
	g := gFrom(1, []Edge{
		{U: "a", V: "x", Weight: 5},
		{U: "b", V: "x", Weight: 8},
	})
	// 右顶点 y 无边，完美匹配不可行。
	_, err := Solve(g, ModePerfect, nil)
	if failReason(t, err) != FailurePerfectInfeasible {
		t.Fatalf("want perfect infeasible, got %v", err)
	}

	g2 := gFrom(1, []Edge{
		{U: "a", V: "x", Weight: 5},
		{U: "a", V: "y", Weight: 9},
		{U: "b", V: "x", Weight: 8},
	})
	res, err := Solve(g2, ModePerfect, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cardinality != 2 || res.TotalWeight != 17 {
		t.Fatalf("perfect: got k=%d w=%d", res.Cardinality, res.TotalWeight)
	}
}

func TestForbidden(t *testing.T) {
	g := gFrom(1, []Edge{
		{U: "a", V: "x", Weight: 10},
		{U: "a", V: "y", Weight: 9},
		{U: "b", V: "x", Weight: 8},
		{U: "b", V: "y", Weight: 1},
	})
	cons := &Constraints{Forbidden: map[[2]string]struct{}{{"a", "x"}: {}}}
	res, err := Solve(g, ModeMaxWeight, cons)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Matched {
		if m.U == "a" && m.V == "x" {
			t.Fatal("forbidden pair appeared in result")
		}
	}
	// a-y=9, b-x=8 => 17
	if res.TotalWeight != 17 {
		t.Fatalf("got w=%d, want 17", res.TotalWeight)
	}
}

func TestForbiddenMakesPerfectInfeasible(t *testing.T) {
	g := gFrom(1, []Edge{
		{U: "a", V: "x", Weight: 10},
		{U: "b", V: "x", Weight: 9},
	})
	cons := &Constraints{Forbidden: map[[2]string]struct{}{{"a", "x"}: {}}}
	_, err := Solve(g, ModePerfect, cons)
	if failReason(t, err) != FailurePerfectInfeasible {
		t.Fatalf("want infeasible, got %v", err)
	}
}

func TestVertexCaps(t *testing.T) {
	g := gFrom(1, []Edge{
		{U: "a", V: "x", Weight: 10},
		{U: "a", V: "y", Weight: 9},
		{U: "a", V: "z", Weight: 8},
	})
	cons := &Constraints{MaxRight: map[string]int{"x": 1, "y": 1, "z": 1}, MaxLeft: map[string]int{"a": 2}}
	res, err := Solve(g, ModeMaxCardinality, cons)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cardinality != 2 || res.TotalWeight != 19 {
		t.Fatalf("cap: got k=%d w=%d, want k=2 w=19", res.Cardinality, res.TotalWeight)
	}
}

func TestInvalidConstraint(t *testing.T) {
	g := gFrom(1, []Edge{{U: "a", V: "x", Weight: 1}})
	cons := &Constraints{MaxLeft: map[string]int{"a": 0}}
	_, err := Solve(g, ModeMaxWeight, cons)
	if failReason(t, err) != FailureInvalidConstraint {
		t.Fatalf("want invalid constraint, got %v", err)
	}
}

func TestDeterminismAndTieBreak(t *testing.T) {
	g := gFrom(1, []Edge{
		{U: "a", V: "x", Weight: 1},
		{U: "a", V: "y", Weight: 1},
		{U: "b", V: "x", Weight: 1},
		{U: "b", V: "y", Weight: 1},
	})
	want := []Match{{U: "a", V: "x"}, {U: "b", V: "y"}}
	for i := 0; i < 10; i++ {
		res, err := Solve(g, ModeMaxWeight, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Matched) != len(want) {
			t.Fatalf("iter %d: got %v", i, res.Matched)
		}
		for j := range want {
			if res.Matched[j].U != want[j].U || res.Matched[j].V != want[j].V {
				t.Fatalf("iter %d: tie-break got %v, want %v", i, res.Matched, want)
			}
		}
	}
}

func TestEmptyGraph(t *testing.T) {
	g := emptyGraph()
	res, err := Solve(g, ModeMaxWeight, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cardinality != 0 || res.TotalWeight != 0 {
		t.Fatalf("empty graph: %+v", res)
	}
	pres, err := Solve(g, ModePerfect, nil)
	if err != nil {
		t.Fatalf("empty graph perfect match is vacuously feasible, got %v", err)
	}
	if pres.Cardinality != 0 {
		t.Fatalf("empty perfect: %+v", pres)
	}
}
