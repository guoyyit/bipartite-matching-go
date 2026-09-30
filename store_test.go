package matching

import (
	"os"
	"testing"
)

func openFileRW(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDWR, 0o644)
}

func TestStoreVersioning(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s1, err := s.Apply(Mutation{Upsert: []Edge{{U: "a", V: "x", Weight: 1}, {U: "a", V: "y", Weight: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if s1.Version != 1 {
		t.Fatalf("v=%d", s1.Version)
	}
	s2, err := s.Apply(Mutation{Upsert: []Edge{{U: "a", V: "x", Weight: 10}}, Delete: [][2]string{{"a", "y"}}})
	if err != nil {
		t.Fatal(err)
	}
	if s2.Version != 2 {
		t.Fatalf("v=%d", s2.Version)
	}

	// 历史版本内容冻结。
	v1, err := s.Snapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := v1.Graph.Weight("a", "x"); !ok || w != 1 {
		t.Fatalf("v1 a-x = %d,%v want 1", w, ok)
	}
	if _, ok := v1.Graph.Weight("a", "y"); !ok {
		t.Fatal("v1 should still have a-y")
	}

	// 当前版本反映改权与删除。
	cur, _ := s.Snapshot(0)
	if w, ok := cur.Graph.Weight("a", "x"); !ok || w != 10 {
		t.Fatalf("cur a-x = %d,%v want 10", w, ok)
	}
	if _, ok := cur.Graph.Weight("a", "y"); ok {
		t.Fatal("cur should not have a-y")
	}
	if len(cur.Graph.RightVertices()) != 1 || cur.Graph.RightVertices()[0] != "x" {
		t.Fatalf("cur rights = %v, want [x]", cur.Graph.RightVertices())
	}

	if _, err := s.Snapshot(99); err == nil {
		t.Fatal("expected error for future version")
	}
}

func TestStoreRecovery(t *testing.T) {
	dir := t.TempDir()
	func() {
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.Apply(Mutation{Upsert: []Edge{{U: "a", V: "x", Weight: 5}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Apply(Mutation{Upsert: []Edge{{U: "b", V: "y", Weight: 7}}}); err != nil {
			t.Fatal(err)
		}
	}()

	// 重新打开，已提交的两个版本必须完整恢复。
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.CurrentVersion() != 2 {
		t.Fatalf("recovered version %d, want 2", s2.CurrentVersion())
	}
	cur, _ := s2.Snapshot(0)
	if w, ok := cur.Graph.Weight("a", "x"); !ok || w != 5 {
		t.Fatalf("a-x = %d,%v", w, ok)
	}
	if w, ok := cur.Graph.Weight("b", "y"); !ok || w != 7 {
		t.Fatalf("b-y = %d,%v", w, ok)
	}
	// 恢复后可继续提交，版本号连续。
	s3, err := s2.Apply(Mutation{Delete: [][2]string{{"a", "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if s3.Version != 3 {
		t.Fatalf("post-recovery version %d, want 3", s3.Version)
	}
}

func TestStoreRecoveryTornTail(t *testing.T) {
	dir := t.TempDir()
	func() {
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.Apply(Mutation{Upsert: []Edge{{U: "a", V: "x", Weight: 5}}}); err != nil {
			t.Fatal(err)
		}
	}()
	// 追加一条帧并破坏其 CRC，模拟崩溃留下的半写帧。
	w, err := openWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.appendFrame([]byte{0x7f, 'g', 'a', 'r', 'b', 'a', 'g', 'e'}); err != nil {
		t.Fatal(err)
	}
	if err := w.w.Flush(); err != nil {
		t.Fatal(err)
	}
	name := w.path
	w.file.Sync()
	w.close()

	// 翻转最后一字节（CRC 区）使其校验失败。
	f, err := openFileRW(name)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := f.Stat()
	buf := make([]byte, 4)
	// 垃圾帧位于文件末尾，长度 header(8)+payload(8)+crc(4)=20。
	const garbageTotal, garbageBody = 20, 16
	crcOff := info.Size() - garbageTotal + garbageBody
	if _, err := f.ReadAt(buf, crcOff); err != nil {
		t.Fatal(err)
	}
	for i := range buf {
		buf[i] ^= 0xFF
	}
	if _, err := f.WriteAt(buf, crcOff); err != nil {
		t.Fatal(err)
	}
	f.Sync()
	f.Close()
	dump, _ := os.ReadFile(name)
	t.Logf("wal bytes after corrupt: % x", dump)

	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.CurrentVersion() != 1 {
		t.Fatalf("version %d, want 1 (corrupt tail dropped)", s.CurrentVersion())
	}
}

func TestMutationCodecRoundTrip(t *testing.T) {
	m := Mutation{
		Upsert: []Edge{{U: "u1", V: "v1", Weight: -123}, {U: "", V: "z", Weight: 0}},
		Delete: [][2]string{{"a", "b"}, {"k", "j"}},
	}
	got, err := decodeMutation(encodeMutation(m))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Upsert) != 2 || got.Upsert[0] != m.Upsert[0] || got.Upsert[1] != m.Upsert[1] {
		t.Fatalf("upsert mismatch %+v", got.Upsert)
	}
	if len(got.Delete) != 2 || got.Delete[0] != m.Delete[0] || got.Delete[1] != m.Delete[1] {
		t.Fatalf("delete mismatch %+v", got.Delete)
	}
}
