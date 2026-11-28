package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"

	"github.com/chhuax/craft/internal/store/migrate"
	_ "modernc.org/sqlite"
)

// SQLite 将写事务串行化，读事务各自在独立的 WAL 快照中运行。
type SQLite struct {
	writer *sql.DB
	reader *sql.DB
	logger *slog.Logger
}

// OpenSQLite 打开或创建本地文件并在返回前完成迁移。WAL 依赖本机锁与共享内存，
// 不支持网络盘。调用方负责 Close，logger 为 nil 时使用 slog 默认日志。
func OpenSQLite(ctx context.Context, path string, logger *slog.Logger) (_ *SQLite, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析数据库路径 %q: %w", path, err)
	}
	file := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	params := url.Values{"_pragma": {
		"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)",
	}}
	params.Set("_txlock", "immediate")
	file.RawQuery = params.Encode()
	writer, err := sql.Open("sqlite", file.String())
	if err != nil {
		return nil, fmt.Errorf("打开写连接: %w", err)
	}
	// 唯一写连接同时也是 data_version 的观察者。保留空闲连接才能跨事务比较版本。
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	if logger == nil {
		logger = slog.Default()
	}
	s := &SQLite{writer: writer, logger: logger}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Close())
		}
	}()
	if err := migrate.Apply(ctx, writer); err != nil {
		return nil, fmt.Errorf("打开数据库 %q: %w", path, err)
	}

	params.Del("_txlock")
	params.Set("mode", "ro")
	file.RawQuery = params.Encode()
	s.reader, err = sql.Open("sqlite", file.String())
	if err != nil {
		return nil, fmt.Errorf("打开只读连接: %w", err)
	}
	if err := s.reader.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("连接只读数据库: %w", err)
	}
	return s, nil
}

// Close 在所有使用者退出后释放读写连接。
func (s *SQLite) Close() error {
	var err error
	if s.reader != nil {
		if e := s.reader.Close(); e != nil {
			err = fmt.Errorf("关闭只读连接: %w", e)
		}
	}
	if e := s.writer.Close(); e != nil {
		err = errors.Join(err, fmt.Errorf("关闭写连接: %w", e))
	}
	return err
}

// DataVersion 在写事务外调用，只能比较同一打开实例的两次结果，以发现其他连接的提交。
func (s *SQLite) DataVersion(ctx context.Context) (int64, error) {
	var version int64
	if err := s.writer.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("读取 data_version: %w", err)
	}
	return version, nil
}
