// Package matching 实现一个本地运行的二分图带权匹配服务：
// 带权二分图、带约束的最大权匹配、版本化快照、WAL 持久化、
// 崩溃恢复以及并发更新与查询。
package matching

import "sort"

// Edge 表示一条左顶点 U 与右顶点 V 之间的带权边。
type Edge struct {
	U      string
	V      string
	Weight int64
}

// Mode 表示求解范围。
type Mode int

const (
	// ModePerfect 要求所有顶点都被匹配（完美匹配）。
	ModePerfect Mode = iota
	// ModeMaxCardinality 先最大化匹配基数，再最大化总权重。
	ModeMaxCardinality
	// ModeMaxWeight 只最大化总权重，允许任意顶点未匹配。
	ModeMaxWeight
)

// Constraints 表示匹配约束。
type Constraints struct {
	// Forbidden 禁止出现的配对，键为 [左顶点, 右顶点]。
	Forbidden map[[2]string]struct{}
	// MaxLeft 是左顶点配对数上限，缺省为 1。
	MaxLeft map[string]int
	// MaxRight 是右顶点配对数上限，缺省为 1。
	MaxRight map[string]int
}

// Match 是匹配结果中的一条已匹配边。
type Match struct {
	U      string
	V      string
	Weight int64
}

// Result 是某个版本上的确定性匹配结果。
type Result struct {
	Version        int64
	Mode           Mode
	Matched        []Match
	UnmatchedLeft  []string
	UnmatchedRight []string
	Cardinality    int
	TotalWeight    int64
}

// FailureReason 是可区分的求解失败原因。
type FailureReason string

const (
	// FailurePerfectInfeasible 表示完美匹配在当前图与约束下不可行。
	FailurePerfectInfeasible FailureReason = "perfect_match_infeasible"
	// FailureInvalidConstraint 表示约束本身非法（如上限 <= 0）。
	FailureInvalidConstraint FailureReason = "invalid_constraint"
	// FailureInvalidGraph 表示图或边数据非法。
	FailureInvalidGraph FailureReason = "invalid_graph"
)

// SolveError 携带可区分的失败原因，约束不可行时绝不返回部分结果。
type SolveError struct {
	Reason  FailureReason
	Message string
}

func (e *SolveError) Error() string {
	if e == nil {
		return "solve error"
	}
	return string(e.Reason) + ": " + e.Message
}

// Graph 是某一个版本的只读图视图。Graph 一旦构造即不可变，
// 可被任意数量的并发查询同时读取，无需加锁。
//
// 顶点集合由当前仍存在的边隐式确定：当某顶点的最后一条关联边被删除后，
// 该顶点不再出现在图中（因此完美匹配的判定也随之变化）。
type Graph struct {
	version int64
	edges   map[[2]string]int64
	left    map[string]struct{}
	right   map[string]struct{}
}

// Version 返回该图视图对应的版本号。
func (g *Graph) Version() int64 { return g.version }

// Weight 返回边 (u,v) 的权重；边不存在时 ok 为 false。
func (g *Graph) Weight(u, v string) (w int64, ok bool) {
	w, ok = g.edges[[2]string{u, v}]
	return
}

// Edges 按 (U, V) 字典序返回全部边的稳定副本，供确定性求解使用。
func (g *Graph) Edges() []Edge {
	out := make([]Edge, 0, len(g.edges))
	for k, w := range g.edges {
		out = append(out, Edge{U: k[0], V: k[1], Weight: w})
	}
	sortEdges(out)
	return out
}

// LeftVertices 按字典序返回当前左侧顶点集合。
func (g *Graph) LeftVertices() []string { return sortedKeys(g.left) }

// RightVertices 按字典序返回当前右侧顶点集合。
func (g *Graph) RightVertices() []string { return sortedKeys(g.right) }

// newGraph 用已有的边映射构造指定版本的不可变图，并推导顶点集合。
// 调用方移交 edges 的所有权，构造后不得再修改。
func newGraph(version int64, edges map[[2]string]int64) *Graph {
	g := &Graph{
		version: version,
		edges:   edges,
		left:    make(map[string]struct{}),
		right:   make(map[string]struct{}),
	}
	for k := range edges {
		g.left[k[0]] = struct{}{}
		g.right[k[1]] = struct{}{}
	}
	return g
}

// applyMutation 在父版本基础上增量应用变更，生成新版本图。
// 增量复制只拷贝边映射本身；求解器的热启动由 solver 层负责。
func applyMutation(parent *Graph, version int64, mut Mutation) (*Graph, error) {
	edges := make(map[[2]string]int64, len(parent.edges)+len(mut.Upsert))
	for k, w := range parent.edges {
		edges[k] = w
	}
	for _, e := range mut.Upsert {
		if e.U == "" || e.V == "" {
			return nil, &SolveError{
				Reason:  FailureInvalidGraph,
				Message: "edge endpoint must not be empty: " + e.U + "->" + e.V,
			}
		}
		edges[[2]string{e.U, e.V}] = e.Weight
	}
	for _, k := range mut.Delete {
		delete(edges, k)
	}
	return newGraph(version, edges), nil
}

// emptyGraph 构造版本 0 的空图。
func emptyGraph() *Graph {
	return newGraph(0, make(map[[2]string]int64))
}

// sortEdges 按 (U, V) 原地字典序排序。
func sortEdges(es []Edge) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].U != es[j].U {
			return es[i].U < es[j].U
		}
		return es[i].V < es[j].V
	})
}

// sortedKeys 返回 map 键的字典序切片。
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
