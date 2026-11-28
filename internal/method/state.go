package method

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// StateStore 在同一变更名下存放确认、任务清单及证据日志，读取不存在的记录返回空值。
type StateStore interface {
	Load(change string) (*Confirmation, error)
	Save(change string, c Confirmation) error
	LoadTasks(change string) (string, error)
	SaveTasks(change, text string) error
	LoadLedger(change string) ([]Record, error)
	RecordClosing(change string, at time.Time, run ClosingRun) error
	RecordReview(change string, at time.Time, current string, req Document, report VerifierReport) error
	RecordFact(change string, at time.Time, fact string) error
}

type fileStore struct {
	project, dir string
	// Windows 上打开的文件可能无法替换；同一端口的读写须错开打开文件的时段。
	mu sync.RWMutex
}

// NewFileStore 在指定项目下存放状态。dirname 为空时用 .craft，创建动作推迟到首次写入。
func NewFileStore(project, dirname string) (StateStore, error) {
	if dirname == "" {
		dirname = ".craft"
	}
	if project == "" || !safeComponent(dirname) {
		return nil, fmt.Errorf("项目路径 %q 或状态目录名 %q 无效", project, dirname)
	}
	return &fileStore{project: project, dir: filepath.Join(dirname, "method")}, nil
}

// 所有平台都按可移植的名字校验，防止同一记录在 Windows 上被解释为设备、盘符或数据流。
func safeComponent(name string) bool {
	if name == "" || !utf8.ValidString(name) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") || strings.EqualFold(name, ".git") {
		return false
	}
	if strings.ContainsAny(name, "<>:\"/\\|?*") || strings.IndexFunc(name, func(r rune) bool { return r < 32 }) >= 0 {
		return false
	}
	base, _, _ := strings.Cut(strings.ToUpper(name), ".")
	base = strings.TrimRight(base, " ")
	switch base {
	case "NUL", "CON", "PRN", "AUX", "CONIN$", "CONOUT$":
		return false
	}
	if (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && deviceDigit(base[3:]) {
		return false
	}
	return true
}

func deviceDigit(s string) bool {
	return s != "" && utf8.RuneCountInString(s) == 1 && strings.Contains("123456789¹²³", s)
}

func safePath(name string) bool {
	for part := range strings.SplitSeq(name, "/") {
		if !safeComponent(part) {
			return false
		}
	}
	return true
}

// 项目与状态目录各持一个根句柄，符号链接不能把状态文件读写引向目录外。
func (s *fileStore) withDir(create bool, use func(*os.Root) error) (err error) {
	project, err := os.OpenRoot(s.project)
	if err != nil {
		return fmt.Errorf("打开项目 %q：%w", s.project, err)
	}
	defer closeResource(project, &err)
	if create {
		if err := project.MkdirAll(s.dir, 0700); err != nil {
			return fmt.Errorf("创建状态目录 %q：%w", s.dir, err)
		}
	}
	dir, err := project.OpenRoot(s.dir)
	if !create && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("打开状态目录 %q：%w", s.dir, err)
	}
	defer closeResource(dir, &err)
	return use(dir)
}

func (s *fileStore) Load(change string) (*Confirmation, error) {
	if !safeComponent(change) {
		return nil, fmt.Errorf("读取确认：变更名 %q 无效", change)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var c *Confirmation
	err := s.withDir(false, func(dir *os.Root) (err error) {
		c, err = loadRecord(dir, change+".json")
		return err
	})
	return c, err
}

func loadRecord(dir *os.Root, name string) (*Confirmation, error) {
	data, err := dir.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取确认记录 %q：%w", name, err)
	}
	var c Confirmation
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("解析确认记录 %q：%w", name, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("读取确认记录 %q：%w", name, err)
	}
	return &c, nil
}

func (s *fileStore) Save(change string, c Confirmation) error {
	if !safeComponent(change) {
		return fmt.Errorf("保存确认：变更名 %q 无效", change)
	}
	if err := c.validate(); err != nil {
		return fmt.Errorf("保存 %q：%w", change, err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 %q 的确认记录：%w", change, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withDir(true, func(dir *os.Root) error {
		return saveRecord(dir, change+".json", append(data, '\n'))
	})
}

func saveRecord(dir *os.Root, name string, data []byte) error {
	// 未知版本或已损坏的状态须保留，重新确认不能把它悄悄覆盖掉。
	if _, err := loadRecord(dir, name); err != nil {
		return err
	}
	if err := ensureIgnored(dir); err != nil {
		return err
	}
	return atomicWrite(dir, name, data, false)
}

func ensureIgnored(dir *os.Root) error {
	if info, err := dir.Stat(".gitignore"); errors.Is(err, os.ErrNotExist) {
		if err := atomicWrite(dir, ".gitignore", []byte("*\n"), true); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("检查状态目录忽略规则：%w", err)
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("状态目录的 .gitignore 不是普通文件")
	}
	return nil
}

func atomicWrite(dir *os.Root, name string, data []byte, exclusive bool) (err error) {
	tmp := ".tmp-" + rand.Text()
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("创建 %q 的临时文件：%w", name, err)
	}
	defer func() {
		if e := dir.Remove(tmp); e != nil && !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("清理临时文件 %q：%w", tmp, e))
		}
	}()
	if err := writeTemp(f, data); err != nil {
		return fmt.Errorf("写入 %q 的临时文件：%w", name, err)
	}
	if exclusive {
		// 忽略规则允许用户维护，利用硬链接只在文件仍不存在时发布完整内容。
		err = dir.Link(tmp, name)
		if errors.Is(err, os.ErrExist) {
			return nil
		}
	} else {
		err = dir.Rename(tmp, name)
	}
	if err != nil {
		return fmt.Errorf("发布 %q：%w", name, err)
	}
	return nil
}

func writeTemp(f *os.File, data []byte) (err error) {
	defer closeResource(f, &err)
	_, err = f.Write(data)
	return err
}
