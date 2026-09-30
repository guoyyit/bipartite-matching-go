package matching

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWALRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w, err := openWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	recs := [][]byte{[]byte("alpha"), []byte("beta"), []byte("gamma")}
	for _, r := range recs {
		if err := w.appendSync(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}

	w2, err := openWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got [][]byte
	err = w2.replay(func(kind walRecordKind, rec []byte) error {
		if kind != walMutation {
			t.Fatalf("unexpected kind %d", kind)
		}
		got = append(got, append([]byte(nil), rec...))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(recs) {
		t.Fatalf("got %d recs, want %d", len(got), len(recs))
	}
	for i := range recs {
		if string(got[i]) != string(recs[i]) {
			t.Fatalf("rec %d = %q, want %q", i, got[i], recs[i])
		}
	}

	// 重放截断后仍可继续追加，且新记录能被再次重放。
	if err := w2.appendSync([]byte("delta")); err != nil {
		t.Fatal(err)
	}
	if err := w2.close(); err != nil {
		t.Fatal(err)
	}
	w3, err := openWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := w3.replay(func(kind walRecordKind, rec []byte) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("after append got %d, want 4", count)
	}
	w3.close()
}

func TestWALTornTail(t *testing.T) {
	dir := t.TempDir()
	w, err := openWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.appendSync([]byte("good1")); err != nil {
		t.Fatal(err)
	}
	if err := w.appendSync([]byte("good2")); err != nil {
		t.Fatal(err)
	}
	if err := w.w.Flush(); err != nil {
		t.Fatal(err)
	}
	// 不 fsync 直接追加一帧的前若干字节，模拟崩溃撕裂尾。
	torn := []byte{byte(walMutation), 'x', 'x'}
	if err := w.appendFrame(torn); err != nil {
		t.Fatal(err)
	}
	if err := w.w.Flush(); err != nil {
		t.Fatal(err)
	}
	w.close()

	// 手动把文件末尾的完整尾帧截断成半帧（删除最后 3 字节）。
	p := filepath.Join(dir, "wal.log")
	info, _ := os.Stat(p)
	if err := os.Truncate(p, info.Size()-3); err != nil {
		t.Fatal(err)
	}

	w2, err := openWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := w2.replay(func(kind walRecordKind, rec []byte) error {
		got = append(got, string(rec))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "good1" || got[1] != "good2" {
		t.Fatalf("recovered %v, want [good1 good2]", got)
	}
	// 撕裂尾已被物理截断，文件应只含两帧。
	if err := w2.appendSync([]byte("good3")); err != nil {
		t.Fatal(err)
	}
	w2.close()

	w3, _ := openWAL(dir)
	n := 0
	if err := w3.replay(func(kind walRecordKind, rec []byte) error {
		n++
		if n == 3 && string(rec) != "good3" {
			t.Fatalf("rec3 = %q", rec)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("got %d, want 3", n)
	}
	w3.close()
}
