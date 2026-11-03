package mission

// 0.6.10 审校 #1/#5（ADR-0033 v0.2）：送验/裁定事件的契约版本与轮次随事实留痕
// （存储事务内核对后的实际值，InjectReviewFacts / 服务层载荷）——回房投影按
// 事件还原当时契约与当轮，不读 missions 当前行。

import (
	"context"
	"encoding/json"
	"testing"
)

func TestReviewEventCarriesFacts(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := reviewable(t, svc)

	if _, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_a"); err != nil {
		t.Fatalf("首轮送验: %v", err)
	}
	if _, err := svc.RecordReview(ctx, m.MissionID, 1, 1, ReviewDecision{Decision: ReviewReturned, Reason: "再补证"}); err != nil {
		t.Fatalf("首轮退回: %v", err)
	}
	if _, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_b"); err != nil {
		t.Fatalf("次轮送验: %v", err)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	reviewing := 0
	returned := 0
	for _, e := range evs {
		if e.EventType != EventStatusChanged {
			continue
		}
		var p map[string]any
		if json.Unmarshal(e.Payload, &p) != nil {
			t.Fatalf("载荷解析: %s", e.Payload)
		}
		switch {
		case p["to"] == StatusReviewing:
			reviewing++
			if p["contract_version"] != float64(1) {
				t.Fatalf("送验事件缺当时契约版本（第 %d 轮）: %v", reviewing, p)
			}
			if p["review_round"] != float64(reviewing) {
				t.Fatalf("送验事件轮次应为 %d: %v", reviewing, p)
			}
		case p["to"] == StatusReady && p["decision"] == ReviewReturned:
			returned++
			if p["contract_version"] != float64(1) || p["review_round"] != float64(1) {
				t.Fatalf("裁定事件缺当时版本/轮次: %v", p)
			}
		}
	}
	if reviewing != 2 || returned != 1 {
		t.Fatalf("事件计数异常: reviewing=%d returned=%d", reviewing, returned)
	}
}
