// Claim 认知投影最小账本（RFC-0006 Claim 对象的确定性子集，M3-4 v1.54 补齐）。
// 派生源全部确定性（零 LLM）：
//   - closure.accepted 胶囊 conclusions → kind=conclusion，status=strengthened
//     （收束确认）；同 thread 更晚的接受胶囊把早者结论标 superseded（新胶囊
//     取代旧结论——重开后再收束的演化留痕）；assumptions → kind=assumption
//     status=open（待证假设）
//   - evidence_request → kind=open_question；resolved→strengthened（证据到位）、
//     dismissed→weakened
//
// "按 flag 离线启用"的落地形态：仅 dev 调试面暴露（GET /v1/debug/rooms/{id}/claims），
// 不入在线组装、不入公开快照；RFC-0006 E-03 的模型混合提取（置信度/stance 台账）
// 随工具面成熟分期——本账本是它的确定性骨架与供给接口。
package room

import (
	"crypto/sha256"
	"encoding/hex"
)

// ClaimView 认知投影条目（dev claims 端点视图）。
type ClaimView struct {
	ClaimID   string `json:"claim_id"`
	Kind      string `json:"kind"` // conclusion | assumption | open_question
	Statement string `json:"statement"`
	Status    string `json:"status"` // open | strengthened | weakened | superseded
	ThreadID  string `json:"thread_id,omitempty"`
	OriginID  string `json:"origin_id"` // closure_id 或 ereq_ id（provenance）
}

// claimIDOf 确定性派生（cml_ 前缀）。
func claimIDOf(parts ...string) string {
	sum := sha256.Sum256([]byte(joinNul(parts)))
	return "cml_" + hex.EncodeToString(sum[:])[:12]
}

func joinNul(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\x1f"
		}
		out += p
	}
	return out
}

// ClaimsOf 自事件流重建认知账本（纯函数，回放一致）。胶囊取编辑后视图
// （MemoryCapsulesOf——与注入同源；容量纪律不适用：账本求全不预算）。
func ClaimsOf(events []StoredEvent) []ClaimView {
	out := []ClaimView{}

	// 胶囊面：接受序遍历（MemoryCapsulesOf 返回新→旧——反转取时间正序），
	// 同 thread 后到者取代先到者的结论
	caps := MemoryCapsulesOf(events)
	for i := len(caps) - 1; i >= 0; i-- {
		c := caps[i]
		for i := range out {
			if out[i].Kind == "conclusion" && out[i].Status == "strengthened" && out[i].ThreadID == c.ThreadID {
				out[i].Status = "superseded"
			}
		}
		for _, concl := range c.Conclusions {
			out = append(out, ClaimView{
				ClaimID: claimIDOf("concl", c.ClosureID, concl),
				Kind:    "conclusion", Statement: concl, Status: "strengthened",
				ThreadID: c.ThreadID, OriginID: c.ClosureID,
			})
		}
		for _, assum := range c.Assumptions {
			out = append(out, ClaimView{
				ClaimID: claimIDOf("assum", c.ClosureID, assum),
				Kind:    "assumption", Statement: assum, Status: "open",
				ThreadID: c.ThreadID, OriginID: c.ClosureID,
			})
		}
	}

	// 证据需求单面：问题即开放论断，解决/驳回映射状态
	for _, r := range EvidenceRequestsOf(events) {
		status := "open"
		switch r.Status {
		case "resolved":
			status = "strengthened"
		case "dismissed":
			status = "weakened"
		}
		out = append(out, ClaimView{
			ClaimID: claimIDOf("ereq", r.RequestID),
			Kind:    "open_question", Statement: r.Question, Status: status,
			OriginID: r.RequestID,
		})
	}
	return out
}
