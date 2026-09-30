package matching

import "testing"

// BenchmarkApplySingleEdge 测量单条边变更的提交成本：状态在版本间只做
// O(变更规模) 的增量复制，不随图规模退化为全量重建。
func BenchmarkApplySingleEdge(b *testing.B) {
	dir := b.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	base := make([]Edge, 0, 400)
	for i := 0; i < 20; i++ {
		for j := 0; j < 20; j++ {
			base = append(base, Edge{U: "L" + itoa(i), V: "R" + itoa(j), Weight: int64(i + j)})
		}
	}
	if _, err := s.Apply(Mutation{Upsert: base}); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := Edge{U: "L0", V: "R0", Weight: int64(i)}
		if _, err := s.Apply(Mutation{Upsert: []Edge{e}}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkColdSolve 给出从头求解完整图的成本作为对照。
func BenchmarkColdSolve(b *testing.B) {
	g := gFrom(1, base)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Solve(g, ModeMaxWeight, nil); err != nil {
			b.Fatal(err)
		}
	}
}

var base = func() []Edge {
	es := make([]Edge, 0, 400)
	for i := 0; i < 20; i++ {
		for j := 0; j < 20; j++ {
			es = append(es, Edge{U: "L" + itoa(i), V: "R" + itoa(j), Weight: int64((i*31 + j*17) % 97)})
		}
	}
	return es
}()

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
