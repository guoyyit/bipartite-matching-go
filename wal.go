package matching

import (
	"bufio"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

// 记录帧格式（小端）：
//   magic(4)=0x4D41574C | length(4) | payload(length) | crc32(4)
// crc32 覆盖 length 与 payload。重放时遇到不完整或校验失败的尾部帧
// 一律截断丢弃——崩溃时未 fsync 的部分不会留下“半生效”状态。

const (
	walMagic    uint32 = 0x4D41574C
	walHeader          = 8 // magic + length
	walFooter          = 4 // crc32
	maxWalFrame        = 256 * 1024 * 1024
)

// walRecordKind 区分日志条目类型。
type walRecordKind byte

const (
	// walMutation 是一条已提交的变更。
	walMutation walRecordKind = 1
)

// wal 是只追加的预写日志。
type wal struct {
	file *os.File
	w    *bufio.Writer
	path string
}

// openWAL 打开或创建目录 dir 下的 WAL 文件。
func openWAL(dir string) (*wal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "wal.log")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	// 定位到文件末尾，确保新记录追加而非覆盖。
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, err
	}
	return &wal{file: f, w: bufio.NewWriter(f), path: p}, nil
}

// appendFrame 写入一帧（不 fsync）。
func (w *wal) appendFrame(frame []byte) error {
	hdr := make([]byte, walHeader)
	binary.LittleEndian.PutUint32(hdr[0:], walMagic)
	binary.LittleEndian.PutUint32(hdr[4:], uint32(len(frame)))
	if _, err := w.w.Write(hdr); err != nil {
		return err
	}
	if _, err := w.w.Write(frame); err != nil {
		return err
	}
	cs := crc32.ChecksumIEEE(append(hdr[4:], frame...))
	var crcb [walFooter]byte
	binary.LittleEndian.PutUint32(crcb[:], cs)
	_, err := w.w.Write(crcb[:])
	return err
}

// append 缓存一条变更记录；提交需调用 sync。
func (w *wal) append(rec []byte) error {
	return w.appendFrame(append([]byte{byte(walMutation)}, rec...))
}

// appendSync 写入并 fsync 一条记录，返回后即持久化。
func (w *wal) appendSync(rec []byte) error {
	if err := w.append(rec); err != nil {
		return err
	}
	return w.sync()
}

// sync 刷新缓冲并 fsync。
func (w *wal) sync() error {
	if err := w.w.Flush(); err != nil {
		return err
	}
	return w.file.Sync()
}

// replay 按顺序重放全部完整帧；遇到撕裂/损坏尾帧则截断并停止。
func (w *wal) replay(visit func(kind walRecordKind, rec []byte) error) error {
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(w.file)
	validEnd := int64(0)
	for {
		hdr := make([]byte, walHeader)
		_, err := io.ReadFull(r, hdr)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
		magic := binary.LittleEndian.Uint32(hdr[0:])
		length := binary.LittleEndian.Uint32(hdr[4:])
		if magic != walMagic || length > maxWalFrame {
			break
		}
		frame := make([]byte, length)
		if _, err := io.ReadFull(r, frame); err != nil {
			break
		}
		var crcb [walFooter]byte
		if _, err := io.ReadFull(r, crcb[:]); err != nil {
			break
		}
		want := binary.LittleEndian.Uint32(crcb[:])
		got := crc32.ChecksumIEEE(append(hdr[4:], frame...))
		if want != got || len(frame) < 1 {
			break
		}
		if err := visit(walRecordKind(frame[0]), frame[1:]); err != nil {
			return err
		}
		validEnd += int64(walHeader + len(frame) + walFooter)
	}
	if err := w.w.Flush(); err != nil {
		return err
	}
	if err := w.file.Truncate(validEnd); err != nil {
		return err
	}
	if _, err := w.file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	w.w.Reset(w.file)
	return w.file.Sync()
}

// close 刷盘并关闭 WAL。
func (w *wal) close() error {
	if err := w.w.Flush(); err != nil {
		return err
	}
	if err := w.file.Sync(); err != nil {
		return err
	}
	return w.file.Close()
}
