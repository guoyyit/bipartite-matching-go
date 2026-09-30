package matching

import (
	"errors"
	"testing"
)

func graphFromEdges(left, right []string, edges ...Edge) *Graph {
	g := NewGraph()
	g.Left = append(g.Left, left...)
	g.Right = append(g.Right, right...)
	for _, e := range edges {
		g.Edges[e.Pair] = e.Weight
	}
	return g
}

func pairs(matches []Match) []Pair {
	out := make([]Pair, len(matches))
	for i, m := range matches {
		out[i] = m.Pair
	}
	return out
}

func expectPairs(t *testing.T, got []Match, want ...Pair) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("matched %v, want %v", got, want)
	}
	gotSet := map[Pair]bool{}
	for _, m := range got {
		gotSet[m.Pair] = true
	}
	for _, p := range want {
		if !gotSet[p] {
			t.Fatalf("missing pair %v in %v", p, got)
		}
	}
}

func TestMaxWeightPicksPositiveEdges(t *testing.T) {
	g := graphFromEdges(
		[]string{"a", "b"}, []string{"x", "y"},
		Edge{Pair{"a", "x"}, 5},
		Edge{Pair{"a", "y"}, 1},
		Edge{Pair{"b", "x"}, -2},
	)
	sol, err := Solve(g, ScopeMaxWeight)
	if err != nil {
		t.Fatal(err)
	}
	expectPairs(t, sol.Matched, Pair{"a", "x"})
	if sol.Weight != 5 {
		t.Fatalf("weight = %d, want 5", sol.Weight)
	}
	if len(sol.UnmatchedLeft) != 1 || sol.UnmatchedLeft[0] != "b" {
		t.Fatalf("unmatched left = %v", sol.UnmatchedLeft)
	}
	if len(sol.UnmatchedRight) != 1 || sol.UnmatchedRight[0] != "y" {
		t.Fatalf("unmatched right = %v", sol.UnmatchedRight)
	}
}

func TestDeterministicTieBreak(t *testing.T) {
	// Two perfect matchings with equal weight: canonical-order inclusion
	// must deterministically prefer (a,x),(b,y) over (a,y),(b,x).
	g := graphFromEdges(
		[]string{"a", "b"}, []string{"x", "y"},
		Edge{Pair{"a", "x"}, 3},
		Edge{Pair{"a", "y"}, 3},
		Edge{Pair{"b", "x"}, 3},
		Edge{Pair{"b", "y"}, 3},
	)
	for trial := 0; trial < 5; trial++ {
		sol, err := Solve(g, ScopePerfect)
		if err != nil {
			t.Fatal(err)
		}
		expectPairs(t, sol.Matched, Pair{"a", "x"}, Pair{"b", "y"})
	}
}

func TestMaxCardinalityIgnoresWeights(t *testing.T) {
	g := graphFromEdges(
		[]string{"a", "b"}, []string{"x", "y"},
		Edge{Pair{"a", "x"}, 100},
		Edge{Pair{"a", "y"}, -50},
		Edge{Pair{"b", "y"}, -50},
	)
	sol, err := Solve(g, ScopeMaxCardinality)
	if err != nil {
		t.Fatal(err)
	}
	expectPairs(t, sol.Matched, Pair{"a", "x"}, Pair{"b", "y"})
	if sol.Cardinality != 2 {
		t.Fatalf("cardinality = %d, want 2", sol.Cardinality)
	}
}

func TestPerfectMatching(t *testing.T) {
	g := graphFromEdges(
		[]string{"a", "b"}, []string{"x", "y"},
		Edge{Pair{"a", "x"}, 10},
		Edge{Pair{"a", "y"}, 9},
		Edge{Pair{"b", "x"}, 1},
		Edge{Pair{"b", "y"}, 8},
	)
	sol, err := Solve(g, ScopePerfect)
	if err != nil {
		t.Fatal(err)
	}
	// Max-weight perfect matching: (a,y)+(b,x)=10 vs (a,x)+(b,y)=18.
	expectPairs(t, sol.Matched, Pair{"a", "x"}, Pair{"b", "y"})
	if sol.Weight != 18 {
		t.Fatalf("weight = %d, want 18", sol.Weight)
	}
}

func TestPerfectInfeasible(t *testing.T) {
	g := graphFromEdges(
		[]string{"a", "b"}, []string{"x", "y"},
		Edge{Pair{"a", "x"}, 1},
		Edge{Pair{"b", "x"}, 1},
	)
	_, err := Solve(g, ScopePerfect)
	var infeasible *InfeasibleError
	if !errors.As(err, &infeasible) || infeasible.Code != FailurePerfectUnreachable {
		t.Fatalf("err = %v, want perfect_unreachable", err)
	}
}

func TestForbiddenPair(t *testing.T) {
	g := graphFromEdges(
		[]string{"a", "b"}, []string{"x", "y"},
		Edge{Pair{"a", "x"}, 10},
		Edge{Pair{"a", "y"}, 9},
		Edge{Pair{"b", "x"}, 1},
		Edge{Pair{"b", "y"}, 8},
	)
	g.Forbidden[Pair{"a", "x"}] = struct{}{}
	sol, err := Solve(g, ScopePerfect)
	if err != nil {
		t.Fatal(err)
	}
	expectPairs(t, sol.Matched, Pair{"a", "y"}, Pair{"b", "x"})
}

func TestVertexCap(t *testing.T) {
	// One left vertex with cap 2 may cover two right vertices.
	g := graphFromEdges(
		[]string{"a"}, []string{"x", "y"},
		Edge{Pair{"a", "x"}, 5},
		Edge{Pair{"a", "y"}, 4},
	)
	g.LeftCap["a"] = 2
	sol, err := Solve(g, ScopePerfect)
	if err != nil {
		t.Fatal(err)
	}
	expectPairs(t, sol.Matched, Pair{"a", "x"}, Pair{"a", "y"})
}

func TestZeroCapPerfectInfeasible(t *testing.T) {
	g := graphFromEdges(
		[]string{"a"}, []string{"x"},
		Edge{Pair{"a", "x"}, 1},
	)
	g.LeftCap["a"] = 0
	_, err := Solve(g, ScopePerfect)
	var infeasible *InfeasibleError
	if !errors.As(err, &infeasible) || infeasible.Code != FailureCapInfeasible {
		t.Fatalf("err = %v, want cap_infeasible", err)
	}
}

func TestDeterminismAcrossRuns(t *testing.T) {
	g := graphFromEdges(
		[]string{"a", "b", "c"}, []string{"x", "y", "z"},
		Edge{Pair{"a", "x"}, 7},
		Edge{Pair{"a", "y"}, 7},
		Edge{Pair{"a", "z"}, 7},
		Edge{Pair{"b", "x"}, 7},
		Edge{Pair{"b", "y"}, 7},
		Edge{Pair{"b", "z"}, 7},
		Edge{Pair{"c", "x"}, 7},
		Edge{Pair{"c", "y"}, 7},
		Edge{Pair{"c", "z"}, 7},
	)
	first, err := Solve(g, ScopeMaxWeight)
	if err != nil {
		t.Fatal(err)
	}
	for trial := 0; trial < 10; trial++ {
		sol, err := Solve(g, ScopeMaxWeight)
		if err != nil {
			t.Fatal(err)
		}
		if len(sol.Matched) != len(first.Matched) {
			t.Fatalf("run %d cardinality differs", trial)
		}
		for i := range sol.Matched {
			if sol.Matched[i].Pair != first.Matched[i].Pair {
				t.Fatalf("run %d pair %d = %v, want %v", trial, i, sol.Matched[i].Pair, first.Matched[i].Pair)
			}
		}
	}
}
