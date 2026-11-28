package method

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func taskText(dir *os.Root, change string) (string, error) {
	data, err := dir.ReadFile(change + ".tasks.md")
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取 %q 的任务清单：%w", change, err)
	}
	text := string(data)
	if _, err := ParseTasks(text, nil); err != nil {
		return "", fmt.Errorf("读取 %q 的任务清单：%w", change, err)
	}
	return text, nil
}

// LoadTasks 读取任务正文，清单缺失时返回空串。
func (s *fileStore) LoadTasks(change string) (string, error) {
	if !safeComponent(change) {
		return "", fmt.Errorf("任务清单的变更名无效：%q", change)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var text string
	err := s.withDir(false, func(dir *os.Root) (err error) {
		text, err = taskText(dir, change)
		return err
	})
	return text, err
}

// SaveTasks 保存 Markdown 原文，仅在落盘旧式无头清单时补入版本。
func (s *fileStore) SaveTasks(change, text string) error {
	if !safeComponent(change) {
		return fmt.Errorf("任务清单的变更名无效：%q", change)
	}
	doc := Parse("tasks.md", text)
	if _, err := parseTaskDocument(doc, nil); err != nil {
		return err
	}
	// 旧清单没有文件头。在首次写入状态目录时标记版本，原有 Markdown 正文不转成另一份数据。
	if doc.Front == nil {
		text = fmt.Sprintf("---\ncraft: %d\n---\n", formatVersion) + strings.TrimPrefix(text, "\ufeff")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withDir(true, func(dir *os.Root) error {
		if _, err := taskText(dir, change); err != nil {
			return err
		}
		if err := ensureIgnored(dir); err != nil {
			return err
		}
		return atomicWrite(dir, change+".tasks.md", []byte(text), false)
	})
}

func readLedger(dir *os.Root, change string) ([]Record, error) {
	data, err := dir.ReadFile(change + ".jsonl")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 %q 的事实日志：%w", change, err)
	}
	var log []Record
	for len(data) > 0 {
		line, rest, ok := bytes.Cut(data, []byte{'\n'})
		if !ok {
			return nil, fmt.Errorf("日志 %q 第 %d 行未写完", change, len(log)+1)
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("解析日志 %q 第 %d 行：%w", change, len(log)+1, err)
		}
		if err := r.validate(); err != nil {
			return nil, fmt.Errorf("日志 %q 第 %d 行：%w", change, len(log)+1, err)
		}
		if r.Review != nil {
			n := r.Review.ClosingLine
			if n > len(log) || log[n-1].Closing == nil {
				return nil, fmt.Errorf("日志 %q 的独立报告引用了不存在的收尾记录", change)
			}
		}
		log = append(log, r)
		data = rest
	}
	return log, nil
}

// LoadLedger 按写入顺序读取本地事实，不跳过损坏或未知版本的记录。
func (s *fileStore) LoadLedger(change string) ([]Record, error) {
	if !safeComponent(change) {
		return nil, fmt.Errorf("读取日志的变更名无效：%q", change)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var log []Record
	err := s.withDir(false, func(dir *os.Root) (err error) {
		log, err = readLedger(dir, change)
		return err
	})
	return log, err
}

// RecordClosing 保留本次收尾的实际结果，失败及未完成的验证也要留存。
func (s *fileStore) RecordClosing(change string, at time.Time, run ClosingRun) error {
	return s.appendRecord(change, Record{Version: formatVersion, At: at.UTC(), Closing: &run})
}

// RecordFact 追加一条观察到的事实，单独的文字说明不影响完成或验证结论。
func (s *fileStore) RecordFact(change string, at time.Time, fact string) error {
	return s.appendRecord(change, Record{Version: formatVersion, At: at.UTC(), Fact: strings.TrimSpace(fact)})
}

// RecordReview 核对验证者所见快照和完整报告，再绑定到对应的收尾记录。
func (s *fileStore) RecordReview(change string, at time.Time, current string, req Document, report VerifierReport) error {
	if err := report.check(req); err != nil {
		return err
	}
	log, err := s.LoadLedger(change)
	if err != nil {
		return err
	}
	full, _ := latestEvidence(log)
	if full < 0 || log[full].Closing.state(current) != "pass" {
		return fmt.Errorf("独立报告需要与当前快照一致的成功收尾记录")
	}
	review := IndependentReview{ClosingLine: full + 1, Report: report, Pending: pendingAssumptions(req)}
	return s.appendRecord(change, Record{Version: formatVersion, At: at.UTC(), Review: &review})
}

func (s *fileStore) appendRecord(change string, r Record) error {
	if !safeComponent(change) {
		return fmt.Errorf("记录事实的变更名无效：%q", change)
	}
	if err := r.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("编码事实记录：%w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withDir(true, func(dir *os.Root) (err error) {
		log, err := readLedger(dir, change)
		if err != nil {
			return err
		}
		if r.Review != nil {
			full, _ := latestEvidence(log)
			if r.Review.ClosingLine != full+1 {
				return fmt.Errorf("记录独立报告期间出现了新的收尾验证")
			}
		}
		if err := ensureIgnored(dir); err != nil {
			return err
		}
		// 每条记录只做一次追加写，多个端口不会互相覆盖。读端依行号绑定收尾与报告。
		f, err := dir.OpenFile(change+".jsonl", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
		if err != nil {
			return fmt.Errorf("打开 %q 的事实日志：%w", change, err)
		}
		defer closeResource(f, &err)
		if _, err := f.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("追加 %q 的事实记录：%w", change, err)
		}
		return nil
	})
}
