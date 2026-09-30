package matching

import (
	"math/rand"
	"testing"
)

func sameMatches(a, b *Solution) bool {
	if a.Cardinality != b.Cardinality || a.Weight != b.Weight {
		return false
	}
	for i := range a.Matched {
		if a.Matched[i].Pair != b.Matched[i].Pair {
			return false
		}
	}
	return true
}

func TestIncrementalConsistency(t *testing.T) {
	vertices := []string{"a", "b", "c", "d"}
	rights := []string{"w", "x", "y", "z"}
	allPairs := []Pair{}
	for _, l := range vertices {
		for _, r := range rights {
			allPairs = append(allPairs, Pair{l, r})
		}
	}

	for _, scope := range []Scope{ScopeMaxWeight, ScopeMaxCardinality, ScopePerfect} {
		t.Run(scope.String(), func(t *testing.T) {
			rng := rand.New(rand.NewSource(42 + int64(scope)))
			g := graphFromEdges(vertices, rights)
			warm := NewWarmSolver(scope)

			// Seed an initial dense graph.
			for _, p := range allPairs {
				g.Edges[p] = rng.Int63n(21) - 10
			}
			want, err := Solve(g, scope)
			if err != nil {
				t.Fatal(err)
			}
			got, err := warm.Apply(g)
			if err != nil {
				t.Fatal(err)
			}
			if !sameMatches(want, got) {
				t.Fatalf("seed mismatch: full=%v warm=%v", want.Matched, got.Matched)
			}

			for step := 0; step < 400; step++ {
				g = g.Clone()
				applyRandomChange(g, allPairs, rng)

				full, fullErr := Solve(g, scope)
				warmSol, warmErr := warm.Apply(g)
				if (fullErr != nil) != (warmErr != nil) {
					t.Fatalf("step %d error mismatch: full=%v warm=%v", step, fullErr, warmErr)
				}
				if fullErr == nil && !sameMatches(full, warmSol) {
					t.Fatalf("step %d mismatch:\nfull=%v (w=%d,k=%d)\nwarm=%v (w=%d,k=%d)",
						step, full.Matched, full.Weight, full.Cardinality,
						warmSol.Matched, warmSol.Weight, warmSol.Cardinality)
				}
			}
		})
	}
}

func applyRandomChange(g *Graph, allPairs []Pair, rng *rand.Rand) {
	p := allPairs[rng.Intn(len(allPairs))]
	switch rng.Intn(7) {
	case 0, 1:
		g.Edges[p] = rng.Int63n(21) - 10
	case 2:
		delete(g.Edges, p)
	case 3:
		g.Edges[p] = rng.Int63n(21) - 10
	case 4:
		g.Forbidden[p] = struct{}{}
	case 5:
		delete(g.Forbidden, p)
	case 6:
		g.LeftCap[allPairs[rng.Intn(len(allPairs))].Left] = 1 + rng.Intn(3)
	}
}
