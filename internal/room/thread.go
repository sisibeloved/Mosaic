// Thread 生命周期（RFC-0004）：状态机 active/paused/closed/merged + reopen 新用；
// typed relations 已随 A 轨契约切片定稿（message.posted.relations）。本切片：
// 事件族 payload、命令校验（状态机转移合法性）、投影（threads + 显式图边）。
// M2 裁剪（如实登记）：archived/phase.changed 随 M3；merge 的 propose/accept 双步
// 在个人版单 owner 形态下并为直接命令（owner 即确认权，OQ-05）；上下文按线程
// 隔离的完整契约（RFC-0004 §3.5）随 M3 记忆层（近期窗口仍全局）。
package room

import (
	"encoding/json"
	"fmt"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// ThreadState 线程状态（RFC-0004 状态机）。
type ThreadState string

const (
	ThreadActive ThreadState = "active"
	ThreadPaused ThreadState = "paused"
	ThreadClosed ThreadState = "closed"
	ThreadMerged ThreadState = "merged"
)

// ThreadView 线程投影（快照 threads 区；graph 边单独投影）。
type ThreadView struct {
	ThreadID     string      `json:"thread_id"`
	State        ThreadState `json:"state"`
	Parent       string      `json:"parent,omitempty"`      // forked_from（显式边）
	Goal         string      `json:"goal,omitempty"`        // fork 目标
	MergedInto   string      `json:"merged_into,omitempty"` // merged（终态）
	MessageCount int         `json:"message_count"`
}

// GraphEdge 显式关系边（RFC-0004 四种：forked_from/responds_to/merged_into +
// message.posted.relations 类型化边）。推断边属 RFC-0006 结构投影（M3）——
// 双视图"显式 vs 推断区分"：推断边 M3 接入后标记 inferred=true，当前全显式。
type GraphEdge struct {
	Kind     string `json:"kind"` // forked_from | responds_to | merged_into | supports | challenges | ...
	From     string `json:"from"` // 事件/线程 ID
	To       string `json:"to"`
	Inferred bool   `json:"inferred"`
}

// threadStateTransition 合法转移表（RFC-0004 状态机；merged 终态不可再转移）。
func threadStateTransition(from ThreadState, event string) (ThreadState, error) {
	switch event {
	case protocol.EventThreadPaused:
		if from == ThreadActive {
			return ThreadPaused, nil
		}
	case protocol.EventThreadResumed:
		if from == ThreadPaused {
			return ThreadActive, nil
		}
	case protocol.EventThreadClosed:
		if from == ThreadActive || from == ThreadPaused {
			return ThreadClosed, nil
		}
	case protocol.EventThreadReopened:
		if from == ThreadClosed {
			return ThreadActive, nil // reopen 新用
		}
	case protocol.EventThreadMerged:
		if from == ThreadActive || from == ThreadPaused {
			return ThreadMerged, nil
		}
	}
	return from, fmt.Errorf("非法转移：%s 状态下 %s", from, event)
}

// RebuildThreads 线程投影：根线程（room.created 载）+ fork 链 + 生命周期事件。
func RebuildThreads(envs []protocol.Envelope) (map[string]*ThreadView, []GraphEdge) {
	threads := map[string]*ThreadView{}
	edges := []GraphEdge{}
	for _, env := range envs {
		switch env.Type {
		case protocol.EventRoomCreated:
			var p struct {
				ThreadID string `json:"thread_id"`
			}
			if json.Unmarshal(env.Payload, &p) == nil && p.ThreadID != "" {
				threads[p.ThreadID] = &ThreadView{ThreadID: p.ThreadID, State: ThreadActive}
			}
		case protocol.EventThreadForked:
			var p struct {
				ThreadID     string   `json:"thread_id"`
				ParentID     string   `json:"parent_thread_id"`
				Source       string   `json:"source_event_id"`
				Goal         string   `json:"goal"`
				Participants []string `json:"participants"`
			}
			if json.Unmarshal(env.Payload, &p) == nil && p.ThreadID != "" {
				threads[p.ThreadID] = &ThreadView{ThreadID: p.ThreadID, State: ThreadActive,
					Parent: p.ParentID, Goal: p.Goal}
				if p.ParentID != "" {
					edges = append(edges, GraphEdge{Kind: "forked_from", From: p.ThreadID, To: p.ParentID})
				}
			}
		case protocol.EventThreadPaused, protocol.EventThreadResumed,
			protocol.EventThreadClosed, protocol.EventThreadReopened, protocol.EventThreadMerged:
			var p struct {
				ThreadID   string `json:"thread_id"`
				MergedInto string `json:"merged_into"`
			}
			if json.Unmarshal(env.Payload, &p) != nil || p.ThreadID == "" {
				continue
			}
			th, ok := threads[p.ThreadID]
			if !ok {
				th = &ThreadView{ThreadID: p.ThreadID, State: ThreadActive}
				threads[p.ThreadID] = th
			}
			next, err := threadStateTransition(th.State, env.Type)
			if err != nil {
				continue // 投影容错：非法转移跳过（命令面已拒，双保险）
			}
			th.State = next
			if env.Type == protocol.EventThreadMerged && p.MergedInto != "" {
				th.MergedInto = p.MergedInto
				edges = append(edges, GraphEdge{Kind: "merged_into", From: p.ThreadID, To: p.MergedInto})
			}
		case protocol.EventMessagePosted:
			var p struct {
				ReplyTo   *string             `json:"reply_to"`
				Relations []typedRelationWire `json:"relations"`
			}
			if json.Unmarshal(env.Payload, &p) != nil {
				continue
			}
			if p.ReplyTo != nil && *p.ReplyTo != "" {
				edges = append(edges, GraphEdge{Kind: "responds_to", From: env.EventID, To: *p.ReplyTo})
			}
			for _, rel := range p.Relations {
				edges = append(edges, GraphEdge{Kind: rel.Kind, From: env.EventID, To: rel.TargetEventID})
			}
			if env.ThreadID != nil && *env.ThreadID != "" {
				if th, ok := threads[*env.ThreadID]; ok {
					th.MessageCount++
				}
			}
		}
	}
	return threads, edges
}

type typedRelationWire struct {
	TargetEventID string `json:"target_event_id"`
	Kind          string `json:"kind"`
}

// ThreadStateOf 线程当前状态（不存在 = 根外未登记线程按 active）。
func ThreadStateOf(envs []protocol.Envelope, threadID string) ThreadState {
	threads, _ := RebuildThreads(envs)
	if th, ok := threads[threadID]; ok {
		return th.State
	}
	return ThreadActive
}
