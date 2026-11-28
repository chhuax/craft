package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/chhuax/craft/internal/store/migrate"
	_ "modernc.org/sqlite"
)

// SQLite 将写事务串行化，读事务各自在独立的 WAL 快照中运行。
type SQLite struct {
	writeDB *sql.DB
	writer  *sql.Conn
	writing chan struct{}
	reader  *sql.DB
	logger  *slog.Logger
	now     func() time.Time
}

// Option 调整 OpenSQLite 的可注入行为。
type Option func(*SQLite)

// WithClock 注入存储层用来给队列行打时间的时钟。effect 的 due 与 since 由存储写入，
// 如果它们读的是数据库所在机器的系统时钟，而调度读取用的是注入时钟，测试里就无法确定地拨动时间。
func WithClock(now func() time.Time) Option {
	return func(s *SQLite) { s.now = now }
}

// OpenSQLite 打开或创建本地文件并在返回前完成迁移。WAL 依赖本机锁与共享内存，
// 不支持网络盘。调用方负责 Close，logger 为 nil 时使用 slog 默认日志。
func OpenSQLite(ctx context.Context, path string, logger *slog.Logger, opts ...Option) (_ *SQLite, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析数据库路径 %q: %w", path, err)
	}
	uriPath := filepath.ToSlash(abs)
	// Windows 盘符前也需要斜杠，否则 file://C:/... 会把盘符解释成主机名。
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	file := url.URL{Scheme: "file", Path: uriPath}
	params := url.Values{"_pragma": {
		"busy_timeout(5000)", "synchronous(FULL)", "foreign_keys(ON)",
	}}
	params.Set("_txlock", "immediate")
	file.RawQuery = params.Encode()
	writeDB, err := sql.Open("sqlite", file.String())
	if err != nil {
		return nil, fmt.Errorf("打开写连接: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &SQLite{writeDB: writeDB, writing: make(chan struct{}, 1), logger: logger, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Close())
		}
	}()
	// data_version 只能在同一物理连接上比较。即使池大小为一，驱动也可能更换连接。
	s.writer, err = writeDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("连接数据库 %q: %w", path, err)
	}
	if err := migrate.Apply(ctx, s.writer); err != nil {
		return nil, fmt.Errorf("打开数据库 %q: %w", path, err)
	}

	params.Del("_txlock")
	params.Set("mode", "ro")
	params.Add("_pragma", "journal_mode(WAL)")
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
	if s.writer != nil {
		if e := s.writer.Close(); e != nil && !errors.Is(e, sql.ErrConnDone) {
			err = errors.Join(err, fmt.Errorf("归还写连接: %w", e))
		}
	}
	if e := s.writeDB.Close(); e != nil {
		err = errors.Join(err, fmt.Errorf("关闭写连接池: %w", e))
	}
	return err
}

// DataVersion 在写事务外调用，只能比较同一打开实例的两次结果，以发现其他连接的提交。
func (s *SQLite) DataVersion(ctx context.Context) (int64, error) {
	var version int64
	err := s.withWriter(ctx, func() error {
		return s.writer.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version)
	})
	if err != nil {
		return 0, fmt.Errorf("读取 data_version: %w", err)
	}
	return version, nil
}

func (s *SQLite) withWriter(ctx context.Context, fn func() error) error {
	// 独占覆盖整个事务，保证同一连接上的其他使用者无法进入未提交的事务。
	select {
	case s.writing <- struct{}{}:
		defer func() { <-s.writing }()
		return fn()
	case <-ctx.Done():
		return fmt.Errorf("等候 SQLite 写连接: %w", ctx.Err())
	}
}
