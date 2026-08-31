// 房间运行态检视（开发者模式，M1 v1.8）：从事件全量重建的只读内部状态。
// 与投影同纪律——纯函数、确定性：同事件流必同检视结果（回放可验证）。
package room

// StateInspection 房间内部运行态快照（调试面专用：含引擎视角的 epoch/暂停态，
// 属内部语义，不进对外契约——对外仍是快照四元组）。
type StateInspection struct {
	Version int64 `json:"version"` // 房间当前版本（最新 seq；空房 0）
	Epoch   int64 `json:"epoch"`   // 已开轮数（round.opened 计数，即当前 epoch）
	Paused  bool  `json:"paused"`  // 暂停态（最后 room.paused 之后无 room.started）
}

// InspectState 从房间事件重建内部运行态（与引擎门控读同一份事件、同一套语义：
// epoch 与 paused 直接复用引擎的判定函数，避免调试面与执行面口径分叉）。
func InspectState(events []StoredEvent) StateInspection {
	insp := StateInspection{}
	for _, ev := range events {
		if ev.Envelope.Seq > insp.Version {
			insp.Version = ev.Envelope.Seq
		}
	}
	insp.Epoch = countRounds(events)
	insp.Paused = roomPaused(events)
	return insp
}
