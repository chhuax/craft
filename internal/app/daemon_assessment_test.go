package app

import (
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/store"
)

func TestDaemonAssessmentTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := testRuntime(t, inboxDB(t, filepath.Join(t.TempDir(), "db")))
		ctx := t.Context()
		checkErr(t, r.ApplyPolicy(ctx, engine.Settings{EvalTimeoutMs: 1000}, []engine.EventDef{{Source: "feed", Event: "change", Default: engine.ModeAuto, Strength: engine.Explicit}}))
		s := testSource(t, r.repo.db, r.Request)
		_, err := s.Receive(ctx, inboxTrigger("first"))
		checkErr(t, err)
		_, err = r.drain(ctx)
		checkErr(t, err)
		h := assessmentHandler{r, benchScope{}}
		for attempt := 1; attempt <= 3; attempt++ {
			n, err := r.pass.EvaluateDue(ctx)
			checkErr(t, err)
			if n != 1 {
				t.Fatal(n)
			}
			var id string
			checkErr(t, r.repo.Read(ctx, func(rd engine.Reader) error {
				v, err := rd.EvaluatingThreads()
				if len(v) != 1 {
					t.Fatal(v)
				}
				id = v[0].Evaluating.ID
				return err
			}))
			<-r.clock.NewTimer(time.Second).C
			checkErr(t, h.Run(ctx, store.Effect{Key: id}).Err)
			// 重放已终结的代次不能再增加失败次数。
			checkErr(t, h.Run(ctx, store.Effect{Key: id}).Err)
			checkErr(t, r.repo.Read(ctx, func(rd engine.Reader) error {
				th, err := rd.GetThread("feed:item:first")
				if err != nil {
					return err
				}
				if th.Evaluating != nil {
					t.Fatal(th)
				}
				if attempt < 3 {
					if len(th.Pending) != 1 || th.Pending[0] != "first" {
						t.Fatal(th.Pending)
					}
					return nil
				}
				ds, err := rd.DecisionsByThread(th.Key)
				if len(th.Pending) != 0 || len(ds) != 1 || ds[0].Kind != engine.Unavailable || ds[0].Reason[0].Code != engine.ReasonTimeout {
					t.Fatal(th.Pending, ds)
				}
				return err
			}))
		}
	})
}

func TestAssessmentHandedToWorker(t *testing.T) {
	r := testRuntime(t, inboxDB(t, filepath.Join(t.TempDir(), "db")))
	ctx := t.Context()
	checkErr(t, r.ApplyPolicy(ctx, engine.Settings{}, []engine.EventDef{{Source: "feed", Event: "change", Default: engine.ModeAuto, Strength: engine.Explicit}}))
	tr := inboxTrigger("one")
	tr.Human = true
	_, err := testSource(t, r.repo.db, r.Request).Receive(ctx, tr)
	checkErr(t, err)
	_, err = r.drain(ctx)
	checkErr(t, err)
	_, err = r.pass.EvaluateDue(ctx)
	checkErr(t, err)
	var id string
	checkErr(t, r.repo.Atomic(ctx, func(u engine.Unit) error {
		d, err := u.GetDisposition(tr.ID)
		if err != nil {
			return err
		}
		id = d.EvalID
		// 模拟候选创建后接收人已经认领；Decide 会拒绝把同一事件再交给研判。
		d.Fanout = []string{"worker"}
		if err := u.PutWork(engine.Work{ID: "worker"}); err != nil {
			return err
		}
		return u.PutDisposition(tr.ID, *d)
	}))
	h := assessmentHandler{r, benchScope{}}
	checkErr(t, h.Run(ctx, store.Effect{Key: id}).Err)
	_, err = r.pass.EvaluateDue(ctx)
	checkErr(t, err)
	checkErr(t, r.repo.Read(ctx, func(rd engine.Reader) error {
		th, err := rd.GetThread("feed:item:one")
		if err != nil {
			return err
		}
		if th.Evaluating != nil || len(th.Pending) != 0 {
			t.Fatalf("事件交给执行方后仍占用研判: %+v", th)
		}
		return nil
	}))
}
