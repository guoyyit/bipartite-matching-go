package matching

import (
	"math/rand"
	"testing"
)

func TestIncrementalStress(t *testing.T) {
	for _, size := range []int{6, 8} {
		leftNames := make([]string, size)
		rightNames := make([]string, size)
		all := []Pair{}
		for i := 0; i < size; i++ {
			leftNames[i] = string(rune('a' + i))
			rightNames[i] = string(rune('p' + i))
			for j := 0; j < size; j++ {
				all = append(all, Pair{Left: leftNames[i], Right: rightNames[j]})
			}
		}

		for _, scope := range []Scope{ScopeMaxWeight, ScopeMaxCardinality, ScopePerfect} {
			rng := rand.New(rand.NewSource(int64(size*100 + int(scope))))
			g := graphFromEdges(leftNames, rightNames)
			warm := NewWarmSolver(scope)
			for _, p := range all {
				g.Edges[p] = rng.Int63n(41) - 20
			}
			if _, err := warm.Apply(g); err != nil {
				t.Fatal(err)
			}
			for step := 0; step < 800; step++ {
				g = g.Clone()
				p := all[rng.Intn(len(all))]
				switch rng.Intn(10) {
				case 0, 1, 2:
					g.Edges[p] = rng.Int63n(41) - 20
				case 3:
					delete(g.Edges, p)
				case 4, 5:
					g.Forbidden[p] = struct{}{}
				case 6:
					delete(g.Forbidden, p)
				case 7:
					g.LeftCap[p.Left] = 1 + rng.Intn(3)
				case 8:
					g.RightCap[p.Right] = 1 + rng.Intn(3)
				default:
					delete(g.LeftCap, p.Left)
				}
				full, fullErr := Solve(g, scope)
				inc, incErr := warm.Apply(g)
				if (fullErr != nil) != (incErr != nil) {
					t.Fatalf("size=%d scope=%s step=%d err mismatch full=%v inc=%v",
						size, scope, step, fullErr, incErr)
				}
				if fullErr == nil && !sameMatches(full, inc) {
					t.Fatalf("size=%d scope=%s step=%d\nfull=%v\ninc =%v",
						size, scope, step, full.Matched, inc.Matched)
				}
			}
		}
	}
}
