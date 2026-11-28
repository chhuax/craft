package exthost

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const logLimit = 1 << 20

type tailLog struct {
	file *os.File
	size int64
}

func openLog(dir, id string) (*tailLog, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".log"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	l := &tailLog{f, size}
	if err == nil && size > logLimit {
		err = l.keep(logLimit)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return l, nil
}

func (l *tailLog) keep(n int64) error {
	tail := make([]byte, n)
	if n > 0 {
		if _, err := l.file.ReadAt(tail, l.size-n); err != nil {
			return err
		}
	}
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := l.file.Write(tail); err != nil {
		return err
	}
	if err := l.file.Truncate(n); err != nil {
		return err
	}
	l.size = n
	return nil
}

// Write 由单个 stderr 读取循环调用，使截尾与追加按扩展的输出顺序完成。
func (l *tailLog) Write(p []byte) (int, error) {
	n := len(p)
	if int64(n)+l.size > logLimit {
		// 腾出半个文件以摊薄搬移成本；日志始终是按时间顺序可读的连续尾部。
		if n > logLimit {
			p = p[n-logLimit:]
		}
		keep := min(l.size, logLimit/2, int64(logLimit-len(p)))
		if err := l.keep(keep); err != nil {
			return 0, fmt.Errorf("裁剪 %s: %w", l.file.Name(), err)
		}
	}
	written, err := l.file.Write(p)
	l.size += int64(written)
	if err != nil {
		return written, err
	}
	return n, nil
}

func (l *tailLog) capture(r *os.File) error {
	_, err := io.Copy(l, r)
	if err != nil {
		// 磁盘写满时仍要排空 stderr，否则扩展可能连 shutdown 都收不到。
		_, drainErr := io.Copy(io.Discard, r)
		err = errors.Join(err, drainErr)
	}
	return errors.Join(err, r.Close(), l.file.Close())
}
