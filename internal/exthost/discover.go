package exthost

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Extension 将已校验的声明与绝对路径绑定，启动和指纹计算不受当前工作目录影响。
type Extension struct {
	Manifest          Manifest
	Dir, ManifestPath string
}

// Result 的 Extensions 按依赖在前排列。Rejected 以路径区分同名扩展及无法扫描的目录。
type Result struct {
	Extensions []Extension
	Rejected   map[string]error
}

// Discover 扫描每个根目录的直接子目录。冲突双方均拒绝，目录枚举顺序不决定归属。
func Discover(dirs ...string) Result {
	r := Result{Rejected: map[string]error{}}
	all := r.scan(dirs)
	ids := map[string][]string{}
	sources := map[string][]string{}
	for _, e := range all {
		ids[e.Manifest.ID] = append(ids[e.Manifest.ID], e.ManifestPath)
		// 权限按扩展归属。同一文件内重复列出来源不构成跨扩展争用。
		seen := map[string]bool{}
		for _, src := range e.Manifest.WatchSources {
			if !seen[src.ID] {
				sources[src.ID] = append(sources[src.ID], e.ManifestPath)
				seen[src.ID] = true
			}
		}
	}
	r.rejectConflicts("id", ids)
	candidates := map[string]Extension{}
	for _, e := range all {
		if r.Rejected[e.ManifestPath] == nil {
			candidates[e.Manifest.ID] = e
		}
	}
	ordered := r.order(candidates)
	events := map[string][]string{}
	for _, e := range ordered {
		for _, s := range e.Manifest.WatchSources {
			for _, event := range s.Events {
				key := s.ID + ":" + event.Event
				events[key] = append(events[key], e.ManifestPath)
			}
		}
	}
	r.rejectConflicts("watch_sources.events", events)
	r.rejectConflicts("watch_sources.id", sources)
	// 已拓扑排序，只需向后传播一次来源或事件冲突导致的拒绝。
	for _, e := range ordered {
		for _, id := range e.Manifest.Requires {
			if r.Rejected[candidates[id].ManifestPath] != nil {
				r.Rejected[e.ManifestPath] = errors.Join(r.Rejected[e.ManifestPath], fmt.Errorf("%s: requires %q 已拒绝", e.ManifestPath, id))
				break
			}
		}
		if r.Rejected[e.ManifestPath] == nil {
			r.Extensions = append(r.Extensions, e)
		}
	}
	return r
}

func (r Result) scan(dirs []string) []Extension {
	var all []Extension
	seen := map[string]bool{}
	for _, dir := range dirs {
		root, err := filepath.Abs(dir)
		if err != nil {
			r.Rejected[dir] = err
			continue
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		entries, err := os.ReadDir(root)
		if err != nil {
			r.Rejected[root] = err
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			child := filepath.Join(root, entry.Name())
			p := filepath.Join(child, "manifest.json")
			m, err := loadManifest(p)
			if err != nil {
				r.Rejected[p] = err
				continue
			}
			if m.ID != entry.Name() {
				r.Rejected[p] = fmt.Errorf("%s: id %q 与目录名不符", p, m.ID)
				continue
			}
			all = append(all, Extension{m, child, p})
		}
	}
	return all
}

func (r Result) rejectConflicts(field string, keys map[string][]string) {
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		paths := keys[key]
		if len(paths) < 2 {
			continue
		}
		err := fmt.Errorf("%s 重复 %q (%s)", field, key, strings.Join(paths, ", "))
		for _, p := range paths {
			r.Rejected[p] = errors.Join(r.Rejected[p], err)
		}
	}
}

func (r Result) order(exts map[string]Extension) []Extension {
	var ordered []Extension
	done := map[string]bool{}
	visiting := map[string]int{}
	var stack []string
	var visit func(string) error
	visit = func(id string) error {
		e, ok := exts[id]
		if !ok {
			return fmt.Errorf("requires 缺少依赖 %q", id)
		}
		if done[id] {
			return r.Rejected[e.ManifestPath]
		}
		if i, in := visiting[id]; in {
			return fmt.Errorf("requires 依赖环 %s -> %s", strings.Join(stack[i:], " -> "), id)
		}
		visiting[id] = len(stack)
		stack = append(stack, id)
		for _, dep := range e.Manifest.Requires {
			if err := visit(dep); err != nil {
				// 继续遍历其余依赖，否则缺失项可能掩盖同一声明上的环。
				r.Rejected[e.ManifestPath] = errors.Join(r.Rejected[e.ManifestPath], fmt.Errorf("requires %q: %w", dep, err))
			}
		}
		delete(visiting, id)
		stack = stack[:len(stack)-1]
		done[id] = true
		if err := r.Rejected[e.ManifestPath]; err != nil {
			return err
		}
		ordered = append(ordered, e)
		return nil
	}
	// 错误已按文件路径归入 Rejected，其他依赖树可以继续遍历。
	for _, id := range slices.Sorted(maps.Keys(exts)) {
		_ = visit(id)
	}
	return ordered
}

// Fingerprint 只读取 manifest 文件元数据。升级覆盖期间可能暂缺文件，空串让调用方跳过重载；
// 不要把这种情况「优化」成报错或部分指纹，否则每拍都可能触发重启。
func Fingerprint(binaryVersion string, exts []Extension) string {
	paths := make([]string, 0, len(exts))
	for _, e := range exts {
		paths = append(paths, e.ManifestPath)
	}
	slices.Sort(paths)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%q\n", binaryVersion)
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return ""
		}
		_, _ = fmt.Fprintf(h, "%q %d %d\n", p, info.ModTime().UnixNano(), info.Size())
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
