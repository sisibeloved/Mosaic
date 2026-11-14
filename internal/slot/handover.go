// handover.go —— 0.7.9 / ADR-0046：Slot 换 Agent 交接——职责知识与在办事项
// 转移，不复制私有记忆（RFC-0018 §8.1"同 Slot 换 Agent"：必须携带或重新绑定
// 职责实例知识、Mission / WorkItem 状态、产物与未决动作、承接回执；不保证上
// 任 Agent 全部私有记忆——不得复制整个人格冒充原负责人）。
//
// 编排步骤（v0.2 审校修订——每步可核对的状态推进，中断重跑按现势收敛）：
//
//	① 前置核对（零变更）：实例/身份/readiness 预核/在途 0.6.12 交接锚/
//	  遗留转移检查（改选接任者拒——遗留任务可循原目标收敛）
//	② 新任就位（ready——不占 active 位）
//	③ 排空边界（active → draining）：**停接新任务**（0.6.13 建单门在写事务
//	  内核对 active 任职）——收集清单不再"边收边进"（RFC §8.3）
//	④ 在办转移（全属收集：本实例非终态任务不论归属；逐任务单事务，归属锚
//	  防冒领；在途 Run 停止信号经 app 闭包先于翻转发出）
//	⑤ 旧任释放（draining → released；Resume 路径已释则跳过）
//	⑥ 新任激活 + 承接回执事件（**同一事务一次提交**——不存在"激活成功回执
//	  缺失"态）
//	⑦ 报告（重放：已完成为纯报告；若存在漂移任务（如 0.6.12 承接回流入
//	  上任名下）则补转并记该轮回执）
//
// 单写者不变量：边界先于收集（停接单）、转移先于释放（owner 翻转即剥夺上任
// 执行权——0.6.13 CreateRun 核对 owner + 在任绑定）、激活最后且与回执原子。
// 全程至多一人可写；并发换将经服务级互斥串行化（审校 #2——个人实例单进程，
// SQLite 单写者同界；跨实例亦串行，频度极低无争用之虞）。
package slot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// EventSlotHandover 换将承接回执事件（binding_events 流；落新任绑定——
// payload {from_agent, to_agent, transferred_count, slot_instance_id, notes}）。
const EventSlotHandover = "slot_handover"

// ErrHandoverConflict 换将编排冲突（409 族：无在任且无可恢复态 / 新任即在任 /
// 在途单任务交接 / 遗留转移（改选接任者拒） / 新任装机门未过 / 激活复核失败
// ——各分支消息注明收敛路径）。
var ErrHandoverConflict = errors.New("slot: 换 Agent 交接冲突")

// HandoverMission 在办任务投影（端口最小面——mission 域经 app 闭包投影；
// Transferable 由投影方按 mission.HandoverOfferable 域单源标注，slot 域不镜像
// 状态族规则）。
type HandoverMission struct {
	MissionID           string
	Objective           string
	OwnerAgentID        string
	Status              string
	PendingOwnerAgentID string
	SlotInstanceID      string
	// Transferable 是否在换将转移集（draft/ready/running/waiting/paused——
	// reviewing/canceling 不转：验收基线冻结 / 停止瞬态，回执如实列出）。
	Transferable bool
}

// HandoverTransferFact 换将转移事实（一次发生一条——任务 + 当时转出人；
// 补偿按 (任务, 转出人) 配对归档与去重——四审 P2）。
type HandoverTransferFact struct {
	MissionID string
	FromOwner string
}

// TransferredMission 已转移任务（回执）。
type TransferredMission struct {
	MissionID  string `json:"mission_id"`
	Objective  string `json:"objective"`
	FromStatus string `json:"from_status"`
	Status     string `json:"status"` // 转移后（ready/running/waiting→paused；draft/paused 保持）
}

// NotTransferredMission 未转移任务（回执——含原因）。
type NotTransferredMission struct {
	MissionID string `json:"mission_id"`
	Objective string `json:"objective"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
}

// HandoverReceipt 交接回执（schema: SlotHandoverReceipt——审计面：转移清单 /
// 职责知识清单 / 排除面 / 绑定变迁；正文不进回执）。
type HandoverReceipt struct {
	SlotInstanceID     string `json:"slot_instance_id"`
	SlotTitle          string `json:"slot_title"`
	KnowledgeNamespace string `json:"knowledge_namespace"`
	FromAgentID        string `json:"from_agent_id"`
	ToAgentID          string `json:"to_agent_id"`
	OccurredAt         string `json:"occurred_at"`
	// MissionsTransferred 已转移在办（含并发已移动按锚分类计入者）。
	MissionsTransferred []TransferredMission `json:"missions_transferred"`
	// MissionsNotTransferred 未转移（reviewing/canceling 状态族外 / 归属已变化）。
	MissionsNotTransferred []NotTransferredMission `json:"missions_not_transferred"`
	// KnowledgeInventory 职责知识清单（slot 命名空间按 status 计数——不搬动，
	// 以实例为 scope 天然跨任职，交接 = 确认 + 清单）。
	KnowledgeInventory map[string]int `json:"knowledge_inventory"`
	// ExcludedPrivateCount 上任 agent 命名空间记忆计数（排除面——仅计数不入正文）。
	ExcludedPrivateCount int `json:"excluded_private_count"`
	// Excluded 排除面固定注记（"不得复制整个人格"的可审计呈现）。
	Excluded           []string `json:"excluded"`
	ReleasedBindingID  string   `json:"released_binding_id"`
	ActivatedBindingID string   `json:"activated_binding_id"`
	Notes              string   `json:"notes"`
}

// HandoverPorts 交接编排端口（app 装配闭包连 mission/memory 域——方法级注入
// 避开构造环；测试 fake 承载域边界）。
type HandoverPorts struct {
	// MissionsOfSlot 挂本实例的**全部非终态**在办清单（不论归属——审校 #3：
	// 只收上任名下会漏"前轮未完成交接已转出"的遗留；reviewing/canceling 也在
	// 列，Transferable 标注分类）。
	MissionsOfSlot func(ctx context.Context, slotInstanceID string) ([]HandoverMission, error)
	// ReassignMission 单任务转移（mission 域单事务：归属锚 + 状态族 + 无在途 +
	// 承接人 active 内核对；app 闭包先核未终态 Run 并发停止信号——审校 #1）；
	// ok=false = 未转移（锚不符/状态族外/在途——调用方重读分类）；ok 时返回
	// 转移后状态。
	ReassignMission func(ctx context.Context, missionID, fromOwner, toOwner string) (postStatus string, ok bool, err error)
	// MissionStatusOf 重读分类（锚错配后判"已移动"还是"归属第三方/在途"）。
	MissionStatusOf func(ctx context.Context, missionID string) (HandoverMission, bool, error)
	// SlotKnowledgeCounts 职责知识清单（slot 命名空间 scope=实例按 status 计数）。
	SlotKnowledgeCounts func(ctx context.Context, scopeID string) (map[string]int, error)
	// AgentPrivateMemoryCount 上任私有记忆计数（agent 命名空间——排除面）。
	AgentPrivateMemoryCount func(ctx context.Context, agentID string) (int, error)
	// HandoverTransferFactsOfSlot 挂本实例且经换将转移至 owner 的**逐次转移
	// 事实**（每次发生一条，含**当时转出人**——四审 P2：跨任期补偿按原始
	// 转出人归档，不以当前轮 from 顶替；不依赖任务当前状态/归属——终态或
	// 再改派的历史转移事实仍在列）。
	HandoverTransferFactsOfSlot func(ctx context.Context, slotInstanceID, ownerAgentID string) ([]HandoverTransferFact, error)
}

// HandoverService 换将编排服务（持 slot.Service 内部件：绑定状态机、readiness
// 门、身份查询；Ports 外连 mission/memory）。mu 串行化全部交接（审校 #2——
// 并发换将不得交错分批：两请求同读旧任 A 各转不同目标是单 active 索引管不住
// 的批次分裂）。
type HandoverService struct {
	mu    sync.Mutex
	svc   *Service
	ports HandoverPorts
	now   func() string
}

// HandoverOption 装配选项。
type HandoverOption func(*HandoverService)

// WithHandoverNow 注入时钟（测试确定性）。
func WithHandoverNow(f func() string) HandoverOption {
	return func(h *HandoverService) { h.now = f }
}

// NewHandoverService 构造（缺省时钟同 Service 缺省）。
func NewHandoverService(svc *Service, ports HandoverPorts, opts ...HandoverOption) *HandoverService {
	h := &HandoverService{svc: svc, ports: ports, now: func() string { return nowUTC() }}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// excludedNotes 排除面固定注记（RFC §8.1 口径）。
var excludedNotes = []string{
	"上任 Agent 私有记忆（agent 命名空间）不随交接转移——不得复制整个人格冒充原负责人（RFC §8.1）",
	"职责知识以实例知识命名空间承载，随实例跨任职保留——交接不发生复制，仅确认与清单",
}

// handoverState 一次交接的绑定现势（① 解析产物）。
type handoverState struct {
	activeB *Binding // 在任（至多一，部分唯一索引不变量）
	// drainingB 最近 draining（上轮交接中断的旧任——Resume 用）。
	drainingB *Binding
	readyB    *Binding // 新任 ready（复用）
	blockedB  *Binding // 新任 blocked（重试）
	// releasedEA 最近释放且**曾任 active** 的绑定（审校 #5：不以创建序猜旧任
	// ——ready→released 直释候选从未在任，事件流曾任 active 才是真旧任）。
	releasedEA *Binding
}

// resolveHandoverState 解析实例绑定现势（含曾任 active 证据核）。
func (h *HandoverService) resolveHandoverState(ctx context.Context, slotInstanceID, newAgentID string) (handoverState, error) {
	bindings, err := h.svc.store.ListBindings(ctx, slotInstanceID)
	if err != nil {
		return handoverState{}, err
	}
	st := handoverState{}
	for i := range bindings {
		b := bindings[i]
		switch {
		case b.Status == BindingActive:
			st.activeB = &bindings[i]
		case b.Status == BindingDraining:
			if st.drainingB == nil || b.UpdatedAt > st.drainingB.UpdatedAt {
				st.drainingB = &bindings[i]
			}
		case b.AgentID == newAgentID && b.Status == BindingReady:
			st.readyB = &bindings[i]
		case b.AgentID == newAgentID && b.Status == BindingBlocked:
			st.blockedB = &bindings[i]
		}
	}
	// 曾任 active 证据核（审校 #5）：released 中事件流含 to=active 转移者才是
	// 真旧任；按释放时间（updated_at 降序）取最近。
	byUpdated := append([]Binding{}, bindings...)
	sort.Slice(byUpdated, func(i, j int) bool { return byUpdated[i].UpdatedAt > byUpdated[j].UpdatedAt })
	for i := range byUpdated {
		b := byUpdated[i]
		if b.Status != BindingReleased {
			continue
		}
		evs, err := h.svc.store.BindingEvents(ctx, b.BindingID)
		if err != nil {
			return handoverState{}, err
		}
		if wasEverActive(evs) {
			st.releasedEA = &byUpdated[i]
			break
		}
	}
	return st, nil
}

// wasEverActive 绑定是否曾任 active（status_changed 事件证据——ready→released
// 直释候选无此证据，不会被误认作交接旧任）。
func wasEverActive(events []BindingEvent) bool {
	for _, e := range events {
		if e.EventType != EventStatusChanged {
			continue
		}
		var p struct {
			To string `json:"to"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.To == BindingActive {
			return true
		}
	}
	return false
}

// Handover 换 Agent 交接（编排步骤见文件头；服务级互斥串行化）。
func (h *HandoverService) Handover(ctx context.Context, slotInstanceID, newAgentID, notes string) (HandoverReceipt, error) {
	if utf8.RuneCountInString(notes) > 4096 {
		return HandoverReceipt{}, fmt.Errorf("%w: notes ≤4096 字", ErrInvalidBinding)
	}
	newAgentID = strings.TrimSpace(newAgentID)
	if newAgentID == "" {
		return HandoverReceipt{}, fmt.Errorf("%w: new_agent_id 必填", ErrInvalidBinding)
	}

	// 审校 #2：全程互斥——并发换将（同实例或跨实例）串行执行，批次不交错。
	h.mu.Lock()
	defer h.mu.Unlock()

	// ① 前置核对（零变更）。
	inst, ok, err := h.svc.store.InstanceByID(ctx, slotInstanceID)
	if err != nil {
		return HandoverReceipt{}, err
	}
	if !ok {
		return HandoverReceipt{}, ErrInstanceNotFound
	}
	st, err := h.resolveHandoverState(ctx, slotInstanceID, newAgentID)
	if err != nil {
		return HandoverReceipt{}, err
	}

	// 来源（from）与形态分支：
	//   常态——在任旧任（active）；Resume——上轮中断（draining 旧任或已释旧任
	//   + 新任 ready/blocked）；重放——新任已在任（from = 最近真旧任）。
	var fromAgent, releasedID string
	var incumbentB *Binding // 待释放旧任（active/draining；nil = 已释或重放）
	switch {
	case st.activeB != nil && st.activeB.AgentID == newAgentID:
		// 重放：交接已完成——from 取最近真旧任（曾任 active 证据）；无 released
		// 旧任但存在 draining（历史绕行/中断遗留——守卫上线前的数据态）时回退
		// draining 作 from 并将其当作待释放旧任：补转其名下在办后释放，自愈。
		if st.releasedEA != nil {
			fromAgent = st.releasedEA.AgentID
			releasedID = st.releasedEA.BindingID
			incumbentB = st.drainingB
		} else if st.drainingB != nil {
			fromAgent = st.drainingB.AgentID
			incumbentB = st.drainingB
		} else {
			return HandoverReceipt{}, fmt.Errorf("%w: 新任即在任且无从任证据（手动装机非换将——无需交接）", ErrHandoverConflict)
		}
	case st.activeB != nil:
		fromAgent = st.activeB.AgentID
		incumbentB = st.activeB
	case st.drainingB != nil:
		// Resume（审校 #4 边界中断）：draining 旧任即来源。
		fromAgent = st.drainingB.AgentID
		incumbentB = st.drainingB
	case st.readyB != nil || st.blockedB != nil:
		if st.releasedEA == nil {
			return HandoverReceipt{}, fmt.Errorf("%w: 无在任任职且无从任线索——先经任职端点装任再换将", ErrHandoverConflict)
		}
		fromAgent = st.releasedEA.AgentID
		releasedID = st.releasedEA.BindingID
	default:
		return HandoverReceipt{}, fmt.Errorf("%w: 实例无在任任职——先经任职端点装任再换将", ErrHandoverConflict)
	}
	if fromAgent == newAgentID && incumbentB != nil {
		return HandoverReceipt{}, fmt.Errorf("%w: 新任即在任（%s）——无需交接", ErrHandoverConflict, newAgentID)
	}

	// 新任 active 两点之一（AgentLookup）。
	agentSt, ok2, err := h.svc.agentStatusOf(ctx, newAgentID)
	if err != nil {
		return HandoverReceipt{}, err
	}
	if !ok2 {
		return HandoverReceipt{}, ErrAgentNotFound
	}
	if agentSt != "active" {
		return HandoverReceipt{}, fmt.Errorf("%w: %s（%s）", ErrAgentNotActive, newAgentID, agentSt)
	}

	// readiness 预核（latest 版——与装机缺省钉版一致；不满足即止，零变更）。
	if h.svc.readiness != nil {
		latest, _, err := h.svc.store.Latest(ctx, inst.SlotSpecID)
		if err != nil {
			return HandoverReceipt{}, err
		}
		target, payload, err := h.svc.runAdmission(ctx, Binding{
			AgentID: newAgentID, SlotInstanceID: inst.SlotInstanceID,
			SlotSpecVersion: latest.Version, Scope: inst.Scope,
		})
		if err != nil {
			return HandoverReceipt{}, err
		}
		if target == BindingBlocked {
			return HandoverReceipt{}, fmt.Errorf("%w: %s", ErrReadinessNotMet, payload["missing"])
		}
	}

	// 预收集（零变更失败族——审校 #3/#4：在途 0.6.12 交接锚 + 遗留转移）。
	pre, err := h.ports.MissionsOfSlot(ctx, slotInstanceID)
	if err != nil {
		return HandoverReceipt{}, err
	}
	if msg, bad := validateBatch(pre, fromAgent, newAgentID); bad {
		return HandoverReceipt{}, fmt.Errorf("%w: %s", ErrHandoverConflict, msg)
	}

	// ② 新任就位：复用 ready / 重试 blocked / 装机（ready 不占 active 位）。
	// 重放分支（新任已在任）跳过。
	isReplay := st.activeB != nil && st.activeB.AgentID == newAgentID
	var newB Binding
	if isReplay {
		newB = *st.activeB
	} else {
		switch {
		case st.readyB != nil:
			newB = *st.readyB
		case st.blockedB != nil:
			b, err := h.svc.RetryPrepare(ctx, st.blockedB.BindingID)
			if err != nil {
				return HandoverReceipt{}, err
			}
			if b.Status != BindingReady {
				return HandoverReceipt{}, fmt.Errorf("%w: 新任装机门未过（blocked）——修复授权后重跑交接", ErrHandoverConflict)
			}
			newB = b
		default:
			b, err := h.svc.PrepareBinding(ctx, slotInstanceID, newAgentID, 0)
			if err != nil {
				return HandoverReceipt{}, err
			}
			if b.Status != BindingReady {
				return HandoverReceipt{}, fmt.Errorf("%w: 新任装机门未过（blocked）——修复授权后重跑交接（binding %s 留 blocked 可重试）", ErrHandoverConflict, b.BindingID)
			}
			newB = b
		}
	}

	// ③ 排空边界（审校 #4）：在任旧任 active → draining——**停接新任务**
	//（0.6.13 建单门事务内核对 active 任职，此后拒新单）；权威收集后置到边界
	// 之后，"边收边进"结构性消除。Resume 的 draining/已释旧任已处边界态。
	if incumbentB != nil && incumbentB.Status == BindingActive {
		if _, err := h.svc.BeginHandoverDraining(ctx, incumbentB.BindingID, notes); err != nil {
			return HandoverReceipt{}, fmt.Errorf("slot: 换将排空边界失败（零转移，可重试）: %w", err)
		}
	}

	// ④ 在办转移（全属收集——权威批在边界之后重读；预检后新进任务亦在此列）。
	missions, err := h.ports.MissionsOfSlot(ctx, slotInstanceID)
	if err != nil {
		return HandoverReceipt{}, err
	}
	// 边界后复验（预检与边界之间竞得在途交接/遗留——失败留 draining 安全态，
	// 消息注明收敛）。
	if msg, bad := validateBatch(missions, fromAgent, newAgentID); bad {
		return HandoverReceipt{}, fmt.Errorf("%w: %s（旧任已入排空边界 draining——处置后重跑交接收敛）", ErrHandoverConflict, msg)
	}
	transferred := []TransferredMission{}
	notTransferred := []NotTransferredMission{}
	for _, m := range missions {
		switch {
		case !m.Transferable:
			// reviewing/canceling——不转移（归属保留；非上任归属时注明）。
			reason := fmt.Sprintf("%s 不在换将转移集（验收基线冻结/停止瞬态——先了结；owner 不变）", m.Status)
			if m.OwnerAgentID != fromAgent {
				reason = fmt.Sprintf("%s（归属 %s）不在换将转移集——收束中任务归属保留", m.Status, m.OwnerAgentID)
			}
			notTransferred = append(notTransferred, NotTransferredMission{
				MissionID: m.MissionID, Objective: m.Objective, Status: m.Status, Reason: reason,
			})
		case m.OwnerAgentID == newAgentID && !isReplay:
			// 已在本任名下（前轮部分转移的既成事实）——非重跑批列入回执
			//（审校 #3：批完整性——重跑回执须含已转部分）；纯重放无本轮动作，
			// 不入清单（幂等返回当前态）。
			transferred = append(transferred, TransferredMission{
				MissionID: m.MissionID, Objective: m.Objective, FromStatus: m.Status, Status: m.Status,
			})
		case m.OwnerAgentID == newAgentID:
			// 纯重放：本任名下既成事实——状态一致，无动作无清单。
		case m.OwnerAgentID == fromAgent:
			post, done, err := h.ports.ReassignMission(ctx, m.MissionID, fromAgent, newAgentID)
			if err != nil {
				return HandoverReceipt{}, fmt.Errorf("slot: 换将在办转移中断（%s，已转 %d 项）——重跑交接按归属锚收敛: %w", m.MissionID, len(transferred), err)
			}
			if done {
				transferred = append(transferred, TransferredMission{
					MissionID: m.MissionID, Objective: m.Objective, FromStatus: m.Status, Status: post,
				})
				continue
			}
			// 锚错配分类：重读现势（已移动 → 计已转；在途/第三方 → 拒或列明）。
			cur, ok3, err := h.ports.MissionStatusOf(ctx, m.MissionID)
			if err != nil {
				return HandoverReceipt{}, err
			}
			switch {
			case ok3 && cur.OwnerAgentID == newAgentID:
				transferred = append(transferred, TransferredMission{
					MissionID: m.MissionID, Objective: m.Objective, FromStatus: cur.Status, Status: cur.Status,
				})
			case ok3 && cur.PendingOwnerAgentID != "":
				return HandoverReceipt{}, fmt.Errorf("%w: %s 出现在途交接（%s）——先了结再重跑（旧任已 draining）", ErrHandoverConflict, m.MissionID, cur.PendingOwnerAgentID)
			case ok3:
				notTransferred = append(notTransferred, NotTransferredMission{
					MissionID: m.MissionID, Objective: m.Objective, Status: cur.Status,
					Reason: fmt.Sprintf("归属已变化（owner=%s）——非本批转移", cur.OwnerAgentID),
				})
			default:
				notTransferred = append(notTransferred, NotTransferredMission{
					MissionID: m.MissionID, Objective: m.Objective, Status: m.Status,
					Reason: "任务已不存在",
				})
			}
		default:
			// 全属收集下的第三归属可转移任务——validateBatch 已拒（防御兜底）。
			return HandoverReceipt{}, fmt.Errorf("%w: %s 归属第三方 %s（遗留转移）——以原目标重跑收敛", ErrHandoverConflict, m.MissionID, m.OwnerAgentID)
		}
	}

	// ⑤ 旧任释放（draining → released；Resume 已释路径跳过）。
	if incumbentB != nil {
		if _, err := h.svc.ReleaseBinding(ctx, incumbentB.BindingID, notes); err != nil {
			return HandoverReceipt{}, fmt.Errorf("slot: 换将旧任释放失败（任务已转移 %d 项，旧任 draining）——重跑交接收敛: %w", len(transferred), err)
		}
		releasedID = incumbentB.BindingID
	}

	// ⑥ 新任激活 + 承接回执（**同一事务**——审校 #6：激活成功则回执必在，
	// 回执落流失败激活一并未遂，重跑收敛）。重放分支（已在任）无激活步——仅当
	// 本轮实际补转（漂移修复）时独立补记该轮回执事件。
	transferIDs := make([]string, 0, len(transferred))
	for _, tr := range transferred {
		transferIDs = append(transferIDs, tr.MissionID)
	}
	if !isReplay {
		receiptPayload, err := handoverReceiptPayload(fromAgent, newAgentID, transferIDs, slotInstanceID, notes)
		if err != nil {
			return HandoverReceipt{}, err
		}
		ab, err := h.svc.ActivateBindingWithReceipt(ctx, newB.BindingID, EventSlotHandover, receiptPayload)
		if err != nil {
			return HandoverReceipt{}, fmt.Errorf("%w: 新任激活复核失败（任务已转移、旧任已释放）——修复后重跑交接收敛: %v", ErrHandoverConflict, err)
		}
		newB = ab
	} else if len(transferred) > 0 {
		receiptPayload, err := handoverReceiptPayload(fromAgent, newAgentID, transferIDs, slotInstanceID, notes)
		if err != nil {
			return HandoverReceipt{}, err
		}
		if err := h.svc.store.AppendBindingEvent(ctx, newB.BindingID, EventSlotHandover, receiptPayload, h.now()); err != nil {
			return HandoverReceipt{}, fmt.Errorf("slot: 补转回执落流失败（下轮交接按 mission_events 事实补偿）: %w", err)
		}
	}

	// ⑦ 报告（先补偿——复审 P2：补转轮回执落流失败的持久愈合——按逐次
	// 转移计数对账，漂移再转移的第二次事实不因首轮回执存在而被视为已覆盖）。
	if err := h.compensateReceipt(ctx, newB.BindingID, newAgentID, slotInstanceID); err != nil {
		return HandoverReceipt{}, fmt.Errorf("slot: 回执补偿失败（重跑再补）: %w", err)
	}
	rep, err := h.report(ctx, inst, fromAgent, newAgentID, newB, releasedID, notes)
	if err != nil {
		return HandoverReceipt{}, err
	}
	rep.MissionsTransferred = transferred
	rep.MissionsNotTransferred = notTransferred
	return rep, nil
}

// validateBatch 批前校验（审校 #3/#4）：任一在途 0.6.12 交接（不论归属——换将
// 不静默吞承诺）或**可转移任务归属第三方**（前轮未完成交接的遗留——改选接任
// 者会使其悬空，拒绝并指明原目标收敛路径）→ 冲突。
func validateBatch(missions []HandoverMission, fromAgent, target string) (string, bool) {
	var pending, dangling []string
	for _, m := range missions {
		if m.PendingOwnerAgentID != "" {
			pending = append(pending, fmt.Sprintf("%s（→%s）", m.MissionID, m.PendingOwnerAgentID))
		}
		if m.Transferable && m.OwnerAgentID != fromAgent && m.OwnerAgentID != target {
			dangling = append(dangling, fmt.Sprintf("%s（归属 %s）", m.MissionID, m.OwnerAgentID))
		}
	}
	if len(pending) > 0 {
		return fmt.Sprintf("在途单任务交接未了结（%s）——先承接/婉拒/撤回再换将", strings.Join(pending, "、")), true
	}
	if len(dangling) > 0 {
		return fmt.Sprintf("存在遗留转移任务（%s）——前轮换将未完成，以原目标（遗留归属者）重跑收敛或先处置该归属", strings.Join(dangling, "、")), true
	}
	return "", false
}

// handoverReceiptPayload 承接回执事件体（missions 清单——复审 P2 补偿面的
// 覆盖判定依据：哪些任务经绑定流回执入档）。
func handoverReceiptPayload(fromAgent, toAgent string, missions []string, slotInstanceID, notes string) (json.RawMessage, error) {
	raw, err := json.Marshal(map[string]any{
		"from_agent": fromAgent, "to_agent": toAgent,
		"transferred_count": len(missions), "missions": missions,
		"slot_instance_id": slotInstanceID, "notes": notes,
	})
	if err != nil {
		return nil, fmt.Errorf("slot: 承接回执构造失败: %w", err)
	}
	return raw, nil
}

// compensateReceipt 回执补偿（复审 P2 + 三审 P2 + 四审 P2）：补转轮无绑定
// 态变化可作原子锚——其回执 append 可能失败且重试因任务已属新任而不再重发。
// 对账按 **(任务, 转出人) 配对计数**（四审 P2——跨任期补写不得把历史转移记
// 到当前轮双方名下：A→B 的第二次转移缺失，B→C→B 时期补写须记 from=A 而非
// 当前轮 from）：转移侧 = 逐次事实（HandoverTransferFactsOfSlot——含当时
// 转出人，不依赖任务状态/归属）；回执侧 = 实例全域各绑定 slot_handover 回执
// （to_agent == 新任）中 (mission × 回执轮 from_agent) 计次。缺额按配对补记
// （from = 配对转出人；多轮丢失一次补齐——missions 按缺额重复列）。0.6.12
// 承接不含 cause 不误记；对账平后零动作（幂等）。补偿失败上抛，重试再补。
func (h *HandoverService) compensateReceipt(ctx context.Context, bindingID, toAgent, slotInstanceID string) error {
	facts, err := h.ports.HandoverTransferFactsOfSlot(ctx, slotInstanceID, toAgent)
	if err != nil {
		return err
	}
	if len(facts) == 0 {
		return nil
	}
	transferPairs := map[[2]string]int{} // (missionID, fromOwner) → 发生次数
	for _, f := range facts {
		transferPairs[[2]string{f.MissionID, f.FromOwner}]++
	}
	// 实例全域回执覆盖：本实例各绑定的 slot_handover 回执（to_agent == 新任）
	// 的 (mission × 回执轮 from_agent) 计次——新任历史各任职的回执同属覆盖。
	receiptPairs := map[[2]string]int{}
	bindings, err := h.svc.store.ListBindings(ctx, slotInstanceID)
	if err != nil {
		return err
	}
	for _, b := range bindings {
		evs, err := h.svc.store.BindingEvents(ctx, b.BindingID)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if e.EventType != EventSlotHandover {
				continue
			}
			var p struct {
				ToAgent   string   `json:"to_agent"`
				FromAgent string   `json:"from_agent"`
				Missions  []string `json:"missions"`
			}
			if json.Unmarshal(e.Payload, &p) == nil && p.ToAgent == toAgent {
				for _, id := range p.Missions {
					receiptPairs[[2]string{id, p.FromAgent}]++
				}
			}
		}
	}
	// 缺额：按配对补记——from = 配对的**当时转出人**（非当前轮 from）。
	deficits := map[string][]string{} // fromOwner → missions（按缺额重复列）
	for pair, n := range transferPairs {
		for i := n - receiptPairs[pair]; i > 0; i-- {
			deficits[pair[1]] = append(deficits[pair[1]], pair[0])
		}
	}
	if len(deficits) == 0 {
		return nil
	}
	for from, missions := range deficits {
		payload, err := handoverReceiptPayload(from, toAgent, missions, slotInstanceID,
			fmt.Sprintf("补记（历史转移 %s→%s 的回执落流失败——按 mission_events 转移事实逐条补齐）", from, toAgent))
		if err != nil {
			return err
		}
		if err := h.svc.store.AppendBindingEvent(ctx, bindingID, EventSlotHandover, payload, h.now()); err != nil {
			return err
		}
	}
	return nil
}

// report 组装回执（知识清单 + 排除面 + 绑定变迁；转移清单由调用方填）。
func (h *HandoverService) report(ctx context.Context, inst SlotInstance, fromAgent, toAgent string, newB Binding, releasedID, notes string) (HandoverReceipt, error) {
	knowledge, err := h.ports.SlotKnowledgeCounts(ctx, inst.SlotInstanceID)
	if err != nil {
		return HandoverReceipt{}, err
	}
	private, err := h.ports.AgentPrivateMemoryCount(ctx, fromAgent)
	if err != nil {
		return HandoverReceipt{}, err
	}
	if knowledge == nil {
		knowledge = map[string]int{}
	}
	return HandoverReceipt{
		SlotInstanceID:         inst.SlotInstanceID,
		SlotTitle:              inst.Title,
		KnowledgeNamespace:     inst.KnowledgeNamespace,
		FromAgentID:            fromAgent,
		ToAgentID:              toAgent,
		OccurredAt:             h.now(),
		MissionsTransferred:    []TransferredMission{},
		MissionsNotTransferred: []NotTransferredMission{},
		KnowledgeInventory:     knowledge,
		ExcludedPrivateCount:   private,
		Excluded:               append([]string{}, excludedNotes...),
		ReleasedBindingID:      releasedID,
		ActivatedBindingID:     newB.BindingID,
		Notes:                  notes,
	}, nil
}
