// 记忆编辑闭环（RFC-0007 §7.4 裁定 5 / v1.45）：记忆可查看、可纠错——人工编辑
// 留 edit_history、生效于下次组装。胶囊是一等 Memory 的最小编辑面（conclusions/
// assumptions 整组替换）；编辑后视图是注入面与查看面的同源投影（不双轨）。
// v1.46 同时修复 v1.36 声明失实：capsule 注入（contextx 第八层）从未接进引擎
// 组装——本文件与 engine.assembleChat 的接线一并补齐。
package room

import (
	"encoding/json"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// CapsuleBudgetRunes 恒常平面字符容量（v1.45 裁定 3：Hermes 纪律——硬上限 +
// 水位可见 + 超限倒逼合并，不静默截断）。最新在前填充，装不下即停：被挤出
// 注入的条数在调试面可见（dropped_count），人类据此合并/编辑胶囊。
const CapsuleBudgetRunes = 3000

// CapsuleBudgetStat 容量水位（调试面透出——"恒常平面还剩多少"可观测）。
type CapsuleBudgetStat struct {
	BudgetRunes   int `json:"budget_runes"`
	InjectedRunes int `json:"injected_runes"`
	InjectedCount int `json:"injected_count"`
	DroppedCount  int `json:"dropped_count"`
}

// MemoryEditRecord 编辑历史条目（正文不重复存——编辑后内容在事件流里，
// 此处只留溯源位）。
type MemoryEditRecord struct {
	EventID     string `json:"event_id"`
	EditVersion int    `json:"edit_version"`
	Note        string `json:"note,omitempty"`
	EditedBy    string `json:"edited_by"`
	OccurredAt  string `json:"occurred_at"`
}

// MemoryCapsuleView 编辑后胶囊视图（查看端点 + 语境注入同源）。
type MemoryCapsuleView struct {
	protocol.ClosureCapsule
	EditHistory []MemoryEditRecord `json:"edit_history"`
}

// MemoryCapsulesOf 已接受胶囊 + memory.edited 链应用（最新在前，与
// AcceptedCapsulesOf 同序）。纠错生效于下次组装：注入面消费本函数，不再
// 消费未编辑的原胶囊。
func MemoryCapsulesOf(events []StoredEvent) []MemoryCapsuleView {
	capsules := AcceptedCapsulesOf(events)
	edits := memoryEditsOf(events)
	out := make([]MemoryCapsuleView, 0, len(capsules))
	for _, c := range capsules {
		v := MemoryCapsuleView{ClosureCapsule: c, EditHistory: []MemoryEditRecord{}}
		for _, e := range edits[c.ClosureID] {
			if e.Conclusions != nil {
				v.Conclusions = e.Conclusions
			}
			if e.Assumptions != nil {
				v.Assumptions = e.Assumptions
			}
			v.EditHistory = append(v.EditHistory, MemoryEditRecord{
				EventID: e.EventID, EditVersion: e.EditVersion,
				Note: e.Note, EditedBy: e.EditedBy, OccurredAt: e.OccurredAt,
			})
		}
		out = append(out, v)
	}
	return out
}

// memoryEdit 编辑链内部条目（payload + 溯源位；不进对外 schema）。
type memoryEdit struct {
	protocol.MemoryEditedPayload
	EventID    string
	OccurredAt string
}

// memoryEditsOf 事件流 → 按 memory_id 的编辑链（时间序；edit_version 服务端
// 自事件流递增，此处只按事件序应用）。
func memoryEditsOf(events []StoredEvent) map[string][]memoryEdit {
	chain := map[string][]memoryEdit{}
	for _, ev := range events {
		if ev.Envelope.Type != protocol.EventMemoryEdited {
			continue
		}
		var p protocol.MemoryEditedPayload
		if json.Unmarshal(ev.Envelope.Payload, &p) == nil && p.MemoryID != "" {
			chain[p.MemoryID] = append(chain[p.MemoryID], memoryEdit{
				MemoryEditedPayload: p,
				EventID:             ev.Envelope.EventID,
				OccurredAt:          ev.Envelope.OccurredAt,
			})
		}
	}
	return chain
}

// capsuleMemoriesOf 语境注入位（v1.46 接线修复 + 容量纪律）：编辑后胶囊 →
// 最新在前填充至字符预算，装不下即停（不截断半条、不静默丢旧——dropped
// 计数透出调试面倒逼合并）。
func capsuleMemoriesOf(envs []protocol.Envelope) ([]protocol.ClosureCapsule, CapsuleBudgetStat) {
	views := MemoryCapsulesOf(storedOfEnvelopes(envs))
	stat := CapsuleBudgetStat{BudgetRunes: CapsuleBudgetRunes, InjectedCount: 0, DroppedCount: 0}
	out := make([]protocol.ClosureCapsule, 0, len(views))
	for _, v := range views {
		n := capsuleRunes(v.ClosureCapsule)
		if stat.InjectedCount > 0 && stat.InjectedRunes+n > CapsuleBudgetRunes {
			break
		}
		out = append(out, v.ClosureCapsule)
		stat.InjectedRunes += n
		stat.InjectedCount++
	}
	stat.DroppedCount = len(views) - stat.InjectedCount
	return out, stat
}

// capsuleRunes 胶囊注入体积（conclusions/assumptions/dissent 的字符总量）。
func capsuleRunes(c protocol.ClosureCapsule) int {
	n := 0
	for _, s := range c.Conclusions {
		n += len([]rune(s))
	}
	for _, s := range c.Assumptions {
		n += len([]rune(s))
	}
	for _, d := range c.NamedDissent {
		n += len([]rune(d.Basis))
	}
	return n
}

// CapsuleBudgetOf 恒常平面容量水位（查看面/调试面共用；注入面同一函数——
// 不双轨：UI 看到的 dropped 就是注入实际挤出的条数）。
func CapsuleBudgetOf(envs []protocol.Envelope) CapsuleBudgetStat {
	_, stat := capsuleMemoriesOf(envs)
	return stat
}

// NextEditVersionOf 下一个编辑版本号（自事件流计数；0 = 尚未编辑过）。
func NextEditVersionOf(events []StoredEvent, memoryID string) int {
	n := 0
	for _, ev := range events {
		if ev.Envelope.Type != protocol.EventMemoryEdited {
			continue
		}
		var p protocol.MemoryEditedPayload
		if json.Unmarshal(ev.Envelope.Payload, &p) == nil && p.MemoryID == memoryID {
			n++
		}
	}
	return n + 1
}
