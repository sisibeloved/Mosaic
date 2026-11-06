// 策展动作面测试（0.7.1 / ADR-0038 §2/§3）：状态机矩阵 / CAS / edit_history
// 留痕 / supersede 双动作与锚门 / 时效口径。
package memory

import (
	"context"
	"errors"
	"testing"
)

func proposedCmd() CreateCmd {
	cmd := okCmd()
	cmd.Status = StatusProposed
	return cmd
}

func TestCurationAcceptMatrix(t *testing.T) {
	ctx := context.Background()
	svc := NewService(NewMemStore(), allAnchors(true, nil), fixedNow)

	t.Run("proposed → active（edit_history 恰一条 accept，版本不变）", func(t *testing.T) {
		it, err := svc.Create(ctx, proposedCmd())
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Accept(ctx, it.MemoryID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusActive || got.Version != it.Version {
			t.Fatalf("接受后状态/版本错：%s v%d", got.Status, got.Version)
		}
		if len(got.EditHistory) != 1 || got.EditHistory[0].Cause != CauseAccept {
			t.Fatalf("edit_history 应恰一条 accept，得 %+v", got.EditHistory)
		}
	})

	t.Run("active 幂等返回（不追加留痕）", func(t *testing.T) {
		it, _ := svc.Create(ctx, okCmd())
		got, err := svc.Accept(ctx, it.MemoryID)
		if err != nil || got.Status != StatusActive {
			t.Fatalf("幂等接受错：%v %s", err, got.Status)
		}
		if len(got.EditHistory) != 0 {
			t.Fatalf("幂等不应追加留痕，得 %+v", got.EditHistory)
		}
	})

	t.Run("superseded 终结 409", func(t *testing.T) {
		it, _ := svc.Create(ctx, okCmd())
		if _, err := svc.Reject(ctx, it.MemoryID, "撤回"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Accept(ctx, it.MemoryID); !errors.Is(err, ErrTerminal) {
			t.Fatalf("应 ErrTerminal，得 %v", err)
		}
	})
}

func TestCurationRejectMatrix(t *testing.T) {
	ctx := context.Background()
	svc := NewService(NewMemStore(), allAnchors(true, nil), fixedNow)

	t.Run("proposed 否决 cause=reject", func(t *testing.T) {
		it, _ := svc.Create(ctx, proposedCmd())
		got, err := svc.Reject(ctx, it.MemoryID, "来源不可信")
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusSuperseded || got.Version != it.Version+1 {
			t.Fatalf("否决后状态/版本错：%s v%d", got.Status, got.Version)
		}
		last := got.EditHistory[len(got.EditHistory)-1]
		if last.Cause != CauseReject || last.Note != "来源不可信" || last.Version != got.Version {
			t.Fatalf("留痕错：%+v", last)
		}
	})

	t.Run("active 撤回 cause=withdraw", func(t *testing.T) {
		it, _ := svc.Create(ctx, okCmd())
		got, err := svc.Reject(ctx, it.MemoryID, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.EditHistory[len(got.EditHistory)-1].Cause != CauseWithdraw {
			t.Fatalf("应 withdraw，得 %+v", got.EditHistory)
		}
	})

	t.Run("superseded 再拒 409", func(t *testing.T) {
		it, _ := svc.Create(ctx, okCmd())
		svc.Reject(ctx, it.MemoryID, "")
		if _, err := svc.Reject(ctx, it.MemoryID, ""); !errors.Is(err, ErrTerminal) {
			t.Fatalf("应 ErrTerminal，得 %v", err)
		}
	})

	t.Run("note 超长 400", func(t *testing.T) {
		it, _ := svc.Create(ctx, okCmd())
		long := string(make([]rune, 501))
		if _, err := svc.Reject(ctx, it.MemoryID, long); !errors.Is(err, ErrInvalid) {
			t.Fatalf("应 ErrInvalid，得 %v", err)
		}
	})
}

func TestCurationRevise(t *testing.T) {
	ctx := context.Background()
	svc := NewService(NewMemStore(), allAnchors(true, nil), fixedNow)
	it, _ := svc.Create(ctx, okCmd())

	title := "偏好：极简回复"
	clear := ""
	conf := 0.8
	got, err := svc.Revise(ctx, it.MemoryID, ReviseCmd{
		ExpectedVersion: it.Version,
		Title:           &title,
		ValidFrom:       &clear, // 清除有效期（原本也无——覆盖设置再清的场景见下）
		Confidence:      &conf,
		Note:            "措辞收紧",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != it.Version+1 || got.Title != title || got.Confidence != 0.8 {
		t.Fatalf("修订结果错：v%d %q %v", got.Version, got.Title, got.Confidence)
	}
	if got.Status != StatusActive {
		t.Fatalf("修订不应改状态：%s", got.Status)
	}
	last := got.EditHistory[len(got.EditHistory)-1]
	if last.Cause != CauseRevise || last.Note != "措辞收紧" {
		t.Fatalf("留痕错：%+v", last)
	}

	t.Run("CAS 失败 409", func(t *testing.T) {
		if _, err := svc.Revise(ctx, it.MemoryID, ReviseCmd{ExpectedVersion: it.Version, Title: &title}); !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("应 ErrVersionConflict，得 %v", err)
		}
	})
	t.Run("superseded 不可修订", func(t *testing.T) {
		svc.Reject(ctx, it.MemoryID, "")
		if _, err := svc.Revise(ctx, it.MemoryID, ReviseCmd{ExpectedVersion: got.Version, Title: &title}); !errors.Is(err, ErrTerminal) {
			t.Fatalf("应 ErrTerminal，得 %v", err)
		}
	})
	t.Run("字段校验（title 超长）", func(t *testing.T) {
		it2, _ := svc.Create(ctx, okCmd())
		bad := string(make([]rune, 201))
		if _, err := svc.Revise(ctx, it2.MemoryID, ReviseCmd{ExpectedVersion: it2.Version, Title: &bad}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("应 ErrInvalid，得 %v", err)
		}
	})
}

func TestCurationSupersede(t *testing.T) {
	ctx := context.Background()
	// 锚按 id 判定（okCmd 锚 ag_1 在册；missing 拒）。
	anchored := func(_ context.Context, id string) (bool, error) { return id == "ag_1", nil }
	svc := NewService(NewMemStore(), Anchors{AgentExists: anchored}, fixedNow)
	old, err0 := svc.Create(ctx, okCmd())
	if err0 != nil {
		t.Fatal(err0)
	}
	repl := okCmd()
	repl.Title = "偏好：简洁中文回复（修订）"
	repl.SourceRefs = []string{"room:evt_2"}
	oldGot, newGot, err := svc.Supersede(ctx, old.MemoryID, old.Version, "初版措辞不准", repl)
	if err != nil {
		t.Fatal(err)
	}
	if oldGot.Status != StatusSuperseded || oldGot.Version != old.Version+1 {
		t.Fatalf("旧条目应终结+版本递增：%s v%d", oldGot.Status, oldGot.Version)
	}
	if oldGot.EditHistory[len(oldGot.EditHistory)-1].Cause != CauseSupersede {
		t.Fatalf("旧条目留痕错")
	}
	if newGot.Supersedes[0] != old.MemoryID || newGot.Status != StatusActive || newGot.Version != 1 {
		t.Fatalf("新条目链错：%v %s v%d", newGot.Supersedes, newGot.Status, newGot.Version)
	}
	// 旧条目再动作 → 终结。
	if _, _, err := svc.Supersede(ctx, old.MemoryID, oldGot.Version, "", okCmd()); !errors.Is(err, ErrTerminal) {
		t.Fatalf("应 ErrTerminal，得 %v", err)
	}
	// CAS 失败。
	if _, _, err := svc.Supersede(ctx, newGot.MemoryID, 999, "", okCmd()); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("应 ErrVersionConflict，得 %v", err)
	}
	// replacement 锚门。
	replBad := okCmd()
	replBad.ScopeID = "missing"
	if _, _, err := svc.Supersede(ctx, newGot.MemoryID, newGot.Version, "", replBad); !errors.Is(err, ErrScopeNotFound) {
		t.Fatalf("应 ErrScopeNotFound，得 %v", err)
	}
	// replacement proposed 候选合法（替代链上的候选）。
	repl2 := proposedCmd()
	_, _, err = svc.Supersede(ctx, newGot.MemoryID, newGot.Version, "再修订", repl2)
	if err != nil {
		t.Fatalf("proposed replacement 应合法：%v", err)
	}
	// replacement 非法 status（superseded 直造）拒。
	repl3 := okCmd()
	repl3.Status = StatusSuperseded
	if _, _, err := svc.Supersede(ctx, oldGot.MemoryID, oldGot.Version, "", repl3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("终结态直造应拒，得 %v", err)
	}
}

func TestCurationTimeliness(t *testing.T) {
	ctx := context.Background()
	svc := NewService(NewMemStore(), allAnchors(true, nil), func() string {
		return "2026-11-05T12:00:00Z"
	})
	mk := func(validTo, status string) Item {
		cmd := okCmd()
		cmd.Status = Status(status)
		cmd.ValidTo = validTo
		it, err := svc.Create(ctx, cmd)
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	mk("2026-11-05T11:59:59Z", "active")   // 已过期（< now）
	mk("2026-11-05T12:00:00Z", "active")   // 恰好到期（valid_to == now 未过期——Before 语义）
	mk("2026-11-06T00:00:00Z", "active")   // 未过期
	mk("2026-11-04T00:00:00Z", "proposed") // 过期但 proposed——显式口径不过滤

	res, err := svc.List(ctx, ListFilter{Namespace: NSAgent})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 { // 恰到期 + 未过期
		t.Fatalf("默认 active 口径应 2 条（过期排除），得 %d", len(res.Items))
	}
	// include_expired 显式可见。
	res, err = svc.List(ctx, ListFilter{Namespace: NSAgent, IncludeExpired: true})
	if err != nil || len(res.Items) != 3 {
		t.Fatalf("include_expired 应 3 条，得 %d %v", len(res.Items), err)
	}
	// proposed 显式口径不过滤有效期。
	res, err = svc.List(ctx, ListFilter{Namespace: NSAgent, Status: StatusProposed})
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("proposed 口径应 1 条（过期不过滤），得 %d %v", len(res.Items), err)
	}
}

func TestCreateStatusValidation(t *testing.T) {
	svc := NewService(NewMemStore(), allAnchors(true, nil), fixedNow)
	cmd := okCmd()
	cmd.Status = StatusSuperseded
	if _, err := svc.Create(context.Background(), cmd); !errors.Is(err, ErrInvalid) {
		t.Fatalf("终结态直造应拒，得 %v", err)
	}
}
