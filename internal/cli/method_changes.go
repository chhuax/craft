package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/chhuax/craft/internal/method"
)

var methodShortName = regexp.MustCompile("^[a-z0-9]+(-[a-z0-9]+){0,4}$")
var methodChangeID = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-([a-z0-9]+(?:-[a-z0-9]+){0,4})$`)
var methodDocNames = []string{"change.md", "requirements.md", "design.md"}

type methodChange struct {
	ID        string   `json:"change"`
	Dir       string   `json:"dir,omitempty"`
	Kind      string   `json:"type,omitempty"`
	Tier      string   `json:"tier,omitempty"`
	Files     []string `json:"files"`
	TasksFile string   `json:"tasksFile,omitempty"`
	Items     []string `json:"workItems,omitempty"`
	docs      map[string]method.Document
}

func frontText(d method.Document, key string) string {
	for _, f := range d.Front {
		if f.Key == key {
			s, _ := f.Value.(string)
			return s
		}
	}
	return ""
}

func changeAt(root *os.Root, dir string) (methodChange, error) {
	c := methodChange{ID: path.Base(dir), Dir: dir}
	c.docs = map[string]method.Document{}
	for _, name := range methodDocNames {
		rel := path.Join(dir, name)
		raw, err := root.ReadFile(filepath.FromSlash(rel))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return c, fmt.Errorf("读取 %s：%w", rel, err)
		}
		d := method.Parse(rel, string(raw))
		c.docs[name] = d
		c.Files = append(c.Files, rel)
		if name == "design.md" {
			continue
		}
		c.Kind, c.Tier = frontText(d, "type"), frontText(d, "tier")
		for _, f := range d.Front {
			if f.Key == "workItems" {
				items, _ := f.Value.([]any)
				for _, item := range items {
					c.Items = append(c.Items, fmt.Sprint(item))
				}
			}
		}
	}
	if c.Tier == "standard" || c.Tier == "heavy" {
		c.TasksFile = path.Join(methodStateDir, c.ID+".tasks.md")
	}
	return c, nil
}

func (m *methodCLI) findChanges() ([]methodChange, error) {
	var found []methodChange
	known := map[string]bool{}
	for _, base := range []string{methodChanges, methodDrafts} {
		err := fs.WalkDir(m.root.FS(), base, func(p string, entry fs.DirEntry, err error) error {
			if p == base && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil || !entry.IsDir() || p == base {
				return err
			}
			if methodChangeID.MatchString(entry.Name()) {
				c, err := changeAt(m.root, p)
				if err != nil {
					return err
				}
				if len(c.Files) > 0 {
					found = append(found, c)
					known[c.ID] = true
				}
				return fs.SkipDir
			}
			if base == methodDrafts || path.Dir(p) != base {
				return fs.SkipDir
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("查找 %s 的变更：%w", base, err)
		}
	}
	// 人可以直接改；没有契约时，已记录的事实仍然能定位这次变更。
	entries, err := fs.ReadDir(m.root.FS(), methodStateDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("列出事实记录：%w", err)
	}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".jsonl")
		if ok && entry.Type().IsRegular() && !known[id] && (methodShortName.MatchString(id) || methodChangeID.MatchString(id)) {
			found = append(found, methodChange{ID: id})
		}
	}
	if m.spec.path == "method status" && m.flags["--item"] != "" {
		found = slices.DeleteFunc(found, func(c methodChange) bool { return !slices.Contains(c.Items, m.flags["--item"]) })
	}
	return found, nil
}

func matchingChanges(changes []methodChange, ref string) []methodChange {
	if ref == "" {
		return changes
	}
	var matches []methodChange
	for _, c := range changes {
		id := methodChangeID.FindStringSubmatch(c.ID)
		if c.ID == ref || len(id) > 0 && id[1] == ref {
			matches = append(matches, c)
		}
	}
	return matches
}

func (m *methodCLI) selectChange(changes []methodChange) error {
	ref := m.flags["--change"]
	matches := matchingChanges(changes, ref)
	switch len(matches) {
	case 0:
		if m.spec.path == "method fact" && (methodShortName.MatchString(ref) || methodChangeID.MatchString(ref)) {
			m.change = methodChange{ID: ref}
			return nil
		}
		return fmt.Errorf("%w：变更 %q", errMissing, ref)
	case 1:
		m.change = matches[0]
	default:
		return invalid("匹配到 %d 个变更，请用 --change 指定唯一编号", len(matches))
	}

	return nil
}

func (m *methodCLI) newChange() (_ result, err error) {
	name, kind, tier := m.flags["--name"], m.flags["--type"], m.flags["--tier"]
	if !methodShortName.MatchString(name) {
		return result{}, invalid("短名应为最多五个用连字符分隔的小写字母或数字词")
	}

	group := m.flags["--group"]
	if group != "" && (!filepath.IsLocal(group) || strings.ContainsAny(group, "/\\<>:\"|?*") || strings.ContainsFunc(group, func(r rune) bool { return r < 32 }) || strings.HasPrefix(group, ".") || strings.HasSuffix(group, ".") || strings.HasSuffix(group, " ")) {
		return result{}, invalid("分组须为单个目录名")
	}

	lock, err := m.lockDocuments()
	if err != nil {
		return result{}, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	changes, err := m.findChanges()
	if err != nil {
		return result{}, err
	}
	prior := matchingChanges(changes, m.flags["--change"])
	if items := m.listFlags["--item"]; len(items) > 0 || m.flags["--change"] == "" {
		prior = slices.DeleteFunc(slices.Clone(prior), func(c methodChange) bool {
			return !slices.ContainsFunc(c.Items, func(item string) bool { return slices.Contains(items, item) })
		})
	}
	if m.flags["--change"] != "" && len(prior) == 0 {
		return result{}, fmt.Errorf("%w：变更 %q", errMissing, m.flags["--change"])
	}
	if _, again := m.flags["--new-round"]; !again && len(prior) > 0 {
		if len(prior) != 1 {
			return result{}, invalid("有多个候选变更，请指定 --change")
		}
		return m.resumeDraft(prior[0], group)
	}
	id := m.now.Local().Format("2006-01-02") + "-" + name
	if len(matchingChanges(changes, id)) != 0 {
		return result{}, invalid("变更 %s 已存在", id)
	}
	dir := path.Join(methodChanges, group, id)
	_, draft := m.flags["--draft"]
	if draft {
		dir = path.Join(methodDrafts, id)
	}
	if err := method.ScaffoldAt(m.root, dir, kind, tier, m.flags["--base-branch"], m.listFlags["--item"], m.listFlags["--capability"]); err != nil {
		if errors.Is(err, fs.ErrInvalid) || errors.Is(err, fs.ErrExist) {
			return result{}, invalid("%v", err)
		}
		return result{}, err
	}
	c, err := changeAt(m.root, dir)
	if err != nil {
		return result{}, err
	}
	return data(struct {
		methodChange
		IgnoreHint string `json:"ignoreHint"`
	}{c, "状态目录自带忽略规则；普通变更文档按项目的忽略规则处理。"})
}

func (m *methodCLI) resumeDraft(c methodChange, group string) (result, error) {
	if c.Dir == "" {
		return result{}, invalid("变更 %s 只有事实记录，尚无文档可接续", c.ID)
	}
	if _, draft := m.flags["--draft"]; !draft && path.Dir(c.Dir) == methodDrafts {
		target := path.Join(methodChanges, group, c.ID)
		if _, err := m.root.Lstat(filepath.FromSlash(target)); err == nil {
			return result{}, invalid("接续目录 %s 已存在", target)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return result{}, err
		}
		if err := m.root.MkdirAll(filepath.FromSlash(path.Dir(target)), 0755); err != nil {
			return result{}, err
		}
		if err := m.root.Rename(filepath.FromSlash(c.Dir), filepath.FromSlash(target)); err != nil {
			return result{}, fmt.Errorf("移出草稿：%w", err)
		}
		var err error
		c, err = changeAt(m.root, target)
		if err != nil {
			return result{}, err
		}
	}
	return data(struct {
		methodChange
		Reused bool `json:"reused"`
	}{c, true})
}

// 多份文档并存时，以工作区改动和基础分支差异识别进行中的变更，避免选中历史契约。
func (m *methodCLI) activeChanges(changes []methodChange) ([]methodChange, error) {
	paths, err := method.WorktreeStatus(m.ctx, m.root.Name())
	if err != nil {
		return nil, err
	}
	var active []methodChange
	for _, c := range changes {
		if c.Dir == "" || path.Dir(c.Dir) == methodDrafts || slices.ContainsFunc(paths, func(p method.PathStatus) bool { return strings.HasPrefix(p.Path, c.Dir+"/") }) {
			active = append(active, c)
			continue
		}
		req, ok := c.docs["change.md"]
		if !ok {
			req = c.docs["requirements.md"]
		}
		branch := frontText(req, "baseBranch")
		if branch == "" {
			continue
		}
		base, err := method.BaseCommit(m.ctx, m.root.Name(), branch)
		if err != nil {
			return nil, fmt.Errorf("判断 %s 是否在进行中：%w", c.ID, err)
		}
		diff, err := m.gitOutput("diff", "--name-only", "-z", base, "--", c.Dir)
		if err != nil {
			return nil, err
		}
		if diff != "" {
			active = append(active, c)
		}
	}
	return active, nil
}
