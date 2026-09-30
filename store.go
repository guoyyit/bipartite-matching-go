package matching

import (
	"encoding/binary"
	"fmt"
	"sync"
)

// Mutation 表示一次事务性变更：批量插入/改权与批量删除。
// 同一 Mutation 要么整体生效，要么完全不生效。
type Mutation struct {
	// Upsert 中的边按顺序作用：存在则改权，不存在则新增。
	Upsert []Edge
	// Delete 删除给定 [左顶点, 右顶点] 边，不存在视为无操作。
	Delete [][2]string
}

// Snapshot 是某个完整版本的不可变只读快照，可被并发查询安全共享。
type Snapshot struct {
	Version int64
	Graph   *Graph
}

// Store 是版本化存储。更新在互斥区内串行提交并写 WAL；查询只读取
// 不可变快照指针，因此进行中的更新不会阻塞查询，也不会泄漏中间状态。
type Store struct {
	mu   sync.RWMutex
	wal  *wal
	log  []Mutation
	hist []*Snapshot // hist[v] 为版本 v 的快照，hist[0] 为空图
	cur  *Snapshot
	dir  string
}

// OpenStore 打开（或首次创建）目录 dir 下的本地存储并重放 WAL 恢复。
func OpenStore(dir string) (*Store, error) {
	w, err := openWAL(dir)
	if err != nil {
		return nil, err
	}
	s := &Store{
		wal:  w,
		dir:  dir,
		cur:  &Snapshot{Version: 0, Graph: emptyGraph()},
		hist: []*Snapshot{{Version: 0, Graph: emptyGraph()}},
	}
	// 重放已提交记录，重建全部版本。撕裂尾帧已由 WAL 截断丢弃。
	if err := w.replay(func(kind walRecordKind, rec []byte) error {
		if kind != walMutation {
			return fmt.Errorf("unknown wal record kind: %d", kind)
		}
		mut, err := decodeMutation(rec)
		if err != nil {
			return err
		}
		if err := s.applyReplay(mut); err != nil {
			return err
		}
		return nil
	}); err != nil {
		w.close()
		return nil, err
	}
	return s, nil
}

// applyReplay 在不写 WAL 的前提下提交一条变更（仅用于恢复重放）。
func (s *Store) applyReplay(mut Mutation) error {
	g, err := applyMutation(s.cur.Graph, s.cur.Version+1, mut)
	if err != nil {
		return err
	}
	snap := &Snapshot{Version: s.cur.Version + 1, Graph: g}
	s.log = append(s.log, mut)
	s.hist = append(s.hist, snap)
	s.cur = snap
	return nil
}

// Close 刷盘并关闭存储。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wal.close()
}

// Apply 原子提交一次变更：先在内存构造新版本图，再把记录写入 WAL 并
// fsync，最后才发布快照指针。因此未完成（未 fsync）的更新绝不会被查询
// 观察到；崩溃时未提交记录被 WAL 重放截断丢弃，不留部分生效状态。
// 多个 Apply 由写锁串行化，天然等价于某一串行顺序。
func (s *Store) Apply(mut Mutation) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := applyMutation(s.cur.Graph, s.cur.Version+1, mut)
	if err != nil {
		return nil, err
	}
	rec := encodeMutation(mut)
	if err := s.wal.appendSync(rec); err != nil {
		return nil, err
	}
	snap := &Snapshot{Version: s.cur.Version + 1, Graph: g}
	s.log = append(s.log, mut)
	s.hist = append(s.hist, snap)
	s.cur = snap
	return snap, nil
}

// Snapshot 读取指定版本；version <= 0 表示当前最新版本。
// 查询只取不可变快照的读锁/指针，不被进行中的更新阻塞。
func (s *Store) Snapshot(version int64) (*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if version <= 0 {
		return s.cur, nil
	}
	if version >= int64(len(s.hist)) {
		return nil, fmt.Errorf("version %d does not exist (latest=%d)", version, s.cur.Version)
	}
	return s.hist[version], nil
}

// CurrentVersion 返回最新已提交版本号。
func (s *Store) CurrentVersion() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur.Version
}

// encodeMutation / decodeMutation 是 Mutation 的确定性二进制编码。
func encodeMutation(m Mutation) []byte {
	b := make([]byte, 0, 32)
	var tmp [8]byte
	putStr := func(str string) {
		binary.LittleEndian.PutUint32(tmp[:], uint32(len(str)))
		b = append(b, tmp[:4]...)
		b = append(b, str...)
	}
	putI64 := func(v int64) {
		binary.LittleEndian.PutUint64(tmp[:], uint64(v))
		b = append(b, tmp[:8]...)
	}
	binary.LittleEndian.PutUint32(tmp[:], uint32(len(m.Upsert)))
	b = append(b, tmp[:4]...)
	for _, e := range m.Upsert {
		putStr(e.U)
		putStr(e.V)
		putI64(e.Weight)
	}
	binary.LittleEndian.PutUint32(tmp[:], uint32(len(m.Delete)))
	b = append(b, tmp[:4]...)
	for _, k := range m.Delete {
		putStr(k[0])
		putStr(k[1])
	}
	return b
}

func decodeMutation(b []byte) (Mutation, error) {
	var m Mutation
	pos := 0
	rdU32 := func() (uint32, error) {
		if pos+4 > len(b) {
			return 0, fmt.Errorf("wal record truncated")
		}
		v := binary.LittleEndian.Uint32(b[pos:])
		pos += 4
		return v, nil
	}
	rdI64 := func() (int64, error) {
		if pos+8 > len(b) {
			return 0, fmt.Errorf("wal record truncated")
		}
		v := int64(binary.LittleEndian.Uint64(b[pos:]))
		pos += 8
		return v, nil
	}
	rdStr := func() (string, error) {
		n, err := rdU32()
		if err != nil {
			return "", err
		}
		if pos+int(n) > len(b) {
			return "", fmt.Errorf("wal record truncated")
		}
		v := string(b[pos : pos+int(n)])
		pos += int(n)
		return v, nil
	}
	nu, err := rdU32()
	if err != nil {
		return m, err
	}
	m.Upsert = make([]Edge, 0, nu)
	for i := uint32(0); i < nu; i++ {
		u, err := rdStr()
		if err != nil {
			return m, err
		}
		v, err := rdStr()
		if err != nil {
			return m, err
		}
		w, err := rdI64()
		if err != nil {
			return m, err
		}
		m.Upsert = append(m.Upsert, Edge{U: u, V: v, Weight: w})
	}
	nd, err := rdU32()
	if err != nil {
		return m, err
	}
	m.Delete = make([][2]string, 0, nd)
	for i := uint32(0); i < nd; i++ {
		u, err := rdStr()
		if err != nil {
			return m, err
		}
		v, err := rdStr()
		if err != nil {
			return m, err
		}
		m.Delete = append(m.Delete, [2]string{u, v})
	}
	if pos != len(b) {
		return m, fmt.Errorf("wal record has %d trailing bytes", len(b)-pos)
	}
	return m, nil
}
