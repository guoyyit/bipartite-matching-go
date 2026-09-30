package matching

import (
	"math/rand"
	"testing"
)

// TestIncrementalCheaperThanFull verifies the single-edge-change cost does
// not degenerate to a from-scratch recomputation on a large graph.
func TestIncrementalCheaperThanFull(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	const n = 60
	leftNames := make([]string, n)
	rightNames := make([]string, n)
	all := []Pair{}
	for i := 0; i < n; i++ {
		leftNames[i] = "L" + itoaN(int64(i))
		rightNames[i] = "R" + itoaN(int64(i))
		for j := 0; j < n; j++ {
			all = append(all, Pair{Left: leftNames[i], Right: rightNames[j]})
		}
	}
	rng := rand.New(rand.NewSource(7))
	g := graphFromEdges(leftNames, rightNames)
	for _, p := range all {
		g.Edges[p] = rng.Int63n(100)
	}
	warm := NewWarmSolver(ScopeMaxWeight)
	if _, err := warm.Apply(g); err != nil {
		t.Fatal(err)
	}

	// Change one edge; the warm re-solve should not rebuild from scratch.
	bg := g.Clone()
	bg.Edges[all[0]] = 99

	fullResult := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := Solve(bg, ScopeMaxWeight); err != nil {
				b.Fatal(err)
			}
		}
	})
	warmResult := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := warm.Apply(bg); err != nil {
				b.Fatal(err)
			}
		}
	})

	fullNs := float64(fullResult.T.Nanoseconds()) / float64(fullResult.N)
	warmNs := float64(warmResult.T.Nanoseconds()) / float64(warmResult.N)
	t.Logf("full=%dns incremental=%dns speedup=%.1fx", int64(fullNs), int64(warmNs), fullNs/warmNs)
	if warmNs >= fullNs {
		t.Fatalf("incremental (%dns) not faster than full (%dns)", int64(warmNs), int64(fullNs))
	}
}

func itoaN(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
