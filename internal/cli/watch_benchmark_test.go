package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/app"
	"github.com/chhuax/craft/internal/engine"
)

// 两种库只改变 Thread 数量。都保留一千件在办工作、相同数量的待办和可分页的决策。
func seedWatch(t testing.TB, home string, threads int) {
	t.Helper()
	db := testDB(t, home)
	err := app.NewSQLiteStore(db).Atomic(t.Context(), func(u engine.Unit) error {
		for i := range threads {
			ref := engine.NewThreadRef("feed", "item", fmt.Sprint(i))
			if err := u.PutThread(engine.Thread{Key: engine.ThreadKey(ref), Ref: ref}); err != nil {
				return err
			}
		}
		for i := range 1000 {
			id := fmt.Sprintf("work-%04d", i)
			w := engine.Work{ID: id, Title: "核对外部状态", CreatedAt: testNow, UpdatedAt: testNow}
			if i%20 == 0 {
				w.Attention = &engine.Question{ID: "q-" + id, Question: "请确认"}
			}
			if err := u.PutWork(w); err != nil {
				return err
			}
		}
		for i := range 100 {
			d := engine.Decision{ID: fmt.Sprintf("decision-%04d", i), Kind: engine.Ignore, ThreadKey: "feed:item:1", At: testNow, Reason: []engine.Reason{{Code: "observed"}}}
			if err := u.PutDecision(d); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func BenchmarkWatch(b *testing.B) {
	for _, threads := range []int{100, 10000} {
		b.Run(fmt.Sprintf("threads-%d", threads), func(b *testing.B) {
			home := b.TempDir()
			b.Setenv("CRAFT_HOME", home)
			seedWatch(b, home, threads)
			for _, command := range []string{"status", "list --open", "attention", "decisions --limit 50"} {
				b.Run(strings.ReplaceAll(command, " ", "_"), func(b *testing.B) {
					args := append([]string{"watch"}, strings.Fields(command)...)
					b.ReportAllocs()
					for b.Loop() {
						if n := run(args, "dev", io.Discard, io.Discard, os.Getenv, os.UserHomeDir); n != 0 {
							b.Fatal(n)
						}
					}
				})
			}
		})
	}
}
