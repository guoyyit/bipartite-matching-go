package matching

import "testing"

func TestPerfectUnequalSides(t *testing.T) {
	// 2 left, 1 right; the right vertex must accept two matches (cap 2).
	g := graphFromEdges(
		[]string{"a", "b"}, []string{"x"},
		Edge{Pair{"a", "x"}, 5},
		Edge{Pair{"b", "x"}, 3},
	)
	g.RightCap["x"] = 2
	sol, err := Solve(g, ScopePerfect)
	if err != nil {
		t.Fatal(err)
	}
	expectPairs(t, sol.Matched, Pair{"a", "x"}, Pair{"b", "x"})
	if len(sol.UnmatchedLeft) != 0 || len(sol.UnmatchedRight) != 0 {
		t.Fatalf("perfect matching left unmatched: %v %v", sol.UnmatchedLeft, sol.UnmatchedRight)
	}
}

func TestPerfectUnequalSidesInfeasible(t *testing.T) {
	g := graphFromEdges(
		[]string{"a", "b"}, []string{"x"},
		Edge{Pair{"a", "x"}, 5},
		Edge{Pair{"b", "x"}, 3},
	)
	// x keeps default cap 1: two left vertices cannot both be covered.
	_, err := Solve(g, ScopePerfect)
	expectInfeasible(t, err, FailurePerfectUnreachable)
}

func TestPerfectUsesNegativeEdgesWhenForced(t *testing.T) {
	g := graphFromEdges(
		[]string{"a", "b"}, []string{"x", "y"},
		Edge{Pair{"a", "x"}, -1},
		Edge{Pair{"a", "y"}, -5},
		Edge{Pair{"b", "x"}, -5},
		Edge{Pair{"b", "y"}, -1},
	)
	sol, err := Solve(g, ScopePerfect)
	if err != nil {
		t.Fatal(err)
	}
	expectPairs(t, sol.Matched, Pair{"a", "x"}, Pair{"b", "y"})
	if sol.Weight != -2 {
		t.Fatalf("weight = %d, want -2", sol.Weight)
	}

	// MaxWeight on the same graph takes no negative edge.
	free, err := Solve(g, ScopeMaxWeight)
	if err != nil {
		t.Fatal(err)
	}
	if len(free.Matched) != 0 || free.Weight != 0 {
		t.Fatalf("max weight should be empty, got %v weight %d", free.Matched, free.Weight)
	}
}

func expectInfeasible(t *testing.T, err error, code FailureCode) {
	t.Helper()
	infeasible, ok := err.(*InfeasibleError)
	if !ok || infeasible.Code != code {
		t.Fatalf("err = %v, want code %s", err, code)
	}
}

func TestForbiddenPairOnly(t *testing.T) {
	g := graphFromEdges(
		[]string{"a"}, []string{"x"},
		Edge{Pair{"a", "x"}, 1},
	)
	g.Forbidden[Pair{"a", "x"}] = struct{}{}
	sol, err := Solve(g, ScopeMaxWeight)
	if err != nil {
		t.Fatal(err)
	}
	if len(sol.Matched) != 0 {
		t.Fatalf("expected empty matching, got %v", sol.Matched)
	}
	_, err = Solve(g, ScopePerfect)
	expectInfeasible(t, err, FailurePerfectUnreachable)
}
