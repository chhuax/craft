package boundary

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// 扩展宿主共享公开协议，适配器契约套件共享引擎端口；例外均精确到包。
var standardImportExceptions = map[[2]string]bool{
	{"extensions/github", "github.com/chhuax/craft/pkg/protocol"}:             true,
	{"internal/exthost", "github.com/chhuax/craft/pkg/protocol"}:              true,
	{"internal/engine/enginetest", "github.com/chhuax/craft/internal/engine"}: true,
	{"internal/engine/repotest", "github.com/chhuax/craft/internal/engine"}:   true,
}

func allowedImport(dir, pkg string) bool {
	return standardImportExceptions[[2]string{dir, pkg}]
}

func TestStandardLibraryOnly(t *testing.T) {
	root := os.DirFS(filepath.Join("..", ".."))
	imports := make(map[string][]string)
	for _, dir := range []string{"extensions/github", "internal/alarm", "internal/engine", "internal/exthost", "internal/method"} {
		err := fs.WalkDir(root, dir, func(name string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(name, ".go") || (dir != "extensions/github" && strings.HasSuffix(name, "_test.go")) {
				return err
			}
			if !entry.Type().IsRegular() {
				t.Errorf("%s 必须是仓库内的普通源文件", name)
				return nil
			}
			return visitImports(root, name, func(pkg string) {
				if !allowedImport(path.Dir(name), pkg) {
					imports[pkg] = append(imports[pkg], name)
				}
			})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(imports) == 0 {
		return
	}

	// 按实际导入路径查询 Go 的 Standard 标记，覆盖带平台约束的文件与标准包。
	args := []string{"list", "-e", "-find", "-f", "{{if .Standard}}{{.ImportPath}}{{end}}", "--"}
	for pkg := range imports {
		args = append(args, pkg)
	}
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("无法核对标准库：%v\n%s", err, output)
	}
	standard := strings.Fields(string(output))
	for pkg, files := range imports {
		if !slices.Contains(standard, pkg) {
			t.Errorf("%s 导入了非标准库 %q", strings.Join(files, "、"), pkg)
		}
	}
}

func TestSQLOnlyInStore(t *testing.T) {
	root := os.DirFS(filepath.Join("..", ".."))
	err := fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || name == "internal/store" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		if !entry.Type().IsRegular() {
			t.Errorf("%s 必须是仓库内的普通源文件", name)
			return nil
		}
		return visitImports(root, name, func(pkg string) {
			if pkg == "database/sql" || pkg == "modernc.org/sqlite" {
				t.Errorf("%s 导入了 %q，SQL 只能在 internal/store 内使用", name, pkg)
			}
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// 直接解析导入，带构建约束的源文件和别名导入也要守住同一边界。
func visitImports(root fs.FS, name string, visit func(string)) error {
	data, err := fs.ReadFile(root, name)
	if err != nil {
		return err
	}
	file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.ImportsOnly)
	if err != nil {
		return err
	}
	for _, spec := range file.Imports {
		pkg, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return err
		}
		visit(pkg)
	}
	return nil
}

const wordList = "tools/forbidden-words.txt"

func TestForbiddenWords(t *testing.T) {
	root := os.DirFS(filepath.Join("..", ".."))
	list, err := fs.ReadFile(root, wordList)
	if err != nil {
		t.Fatal(err)
	}
	words := strings.Fields(strings.ToLower(string(list)))
	if len(words) == 0 {
		t.Fatal("禁用词表为空")
	}
	err = fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || name == "docs/adr" {
				return fs.SkipDir
			}
			return nil
		}
		switch name {
		case "LICENSE", "NOTICE", "docs/roadmap.md", "docs/open-questions.md", "internal/boundary/boundary_test.go", wordList:
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		code := !strings.HasPrefix(name, "docs/") && isCode(name, data)
		lineNo := 0
		for line := range strings.SplitSeq(strings.ToLower(string(data)), "\n") {
			lineNo++
			for _, word := range words {
				if (word != "ticket" || code) && strings.Contains(line, word) {
					t.Errorf("%s:%d 含禁用词 %q", name, lineNo, word)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func isCode(name string, data []byte) bool {
	if bytes.HasPrefix(data, []byte("#!")) {
		return true
	}
	switch strings.ToLower(path.Base(name)) {
	case "makefile", "dockerfile":
		return true
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".go", ".s", ".c", ".h", ".cc", ".cpp", ".hpp", ".rs", ".java", ".kt", ".cs", ".swift",
		".js", ".cjs", ".mjs", ".jsx", ".ts", ".cts", ".mts", ".tsx", ".py", ".rb", ".php", ".lua",
		".sh", ".bash", ".zsh", ".fish", ".ps1", ".psm1", ".cmd", ".bat", ".sql", ".mk":
		return true
	}
	return false
}

func TestStandardImportException(t *testing.T) {
	for _, tc := range []struct {
		dir, pkg string
		want     bool
	}{
		{"internal/engine/enginetest", "github.com/chhuax/craft/internal/engine", true},
		{"internal/engine/enginetest", "github.com/chhuax/craft/internal/store", false},
		{"internal/engine/enginetest/extra", "github.com/chhuax/craft/internal/engine", false},
		{"internal/engine/repotest", "github.com/chhuax/craft/internal/engine", true},
		{"internal/engine/repotest", "github.com/chhuax/craft/internal/store", false},
		{"internal/engine/repotest", "example.org/helper", false},
		{"internal/engine/repotest/extra", "github.com/chhuax/craft/internal/engine", false},
		{"internal/method", "github.com/chhuax/craft/internal/engine", false},
		{"internal/alarm", "github.com/chhuax/craft/internal/engine", false},
		{"internal/exthost", "github.com/chhuax/craft/pkg/protocol", true},
		{"internal/exthost", "github.com/chhuax/craft/pkg/protocol/extra", false},
		{"internal/exthost", "github.com/chhuax/craft/internal/engine", false},
		{"internal/exthost", "github.com/chhuax/craft/internal/store", false},
		{"internal/exthost", "github.com/chhuax/craft/internal/alarm", false},
		{"internal/exthost", "github.com/chhuax/craft/internal/app", false},
		{"internal/exthost", "example.org/helper", false},
		{"internal/engine", "github.com/chhuax/craft/pkg/protocol", false},
	} {
		if got := allowedImport(tc.dir, tc.pkg); got != tc.want {
			t.Errorf("%s 导入 %q 放行 = %t", tc.dir, tc.pkg, got)
		}
	}
}
