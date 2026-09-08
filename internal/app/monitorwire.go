// Package app 内文件：M4-5 监控装配——调度器、run_task 发起面（版本重试）、
// run 终态解析（run.requested 事件引用 → 投影状态）。
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sisibeloved/Mosaic/internal/monitor"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/storage/sqlite"
)

// monitorLauncher 经命令面发起监控 run（run_task）：读版本 → 执行 → 冲突重试。
// 返回 run.requested 事件引用（monitor 的在途跟踪面——事件 id 是命令回执的
// 稳定锚点；run_id 在事件载荷里，由状态解析面解出）。
type monitorLauncher struct {
	svc   *room.Service
	store room.AtomicStore
	newTx func() string
}

func uuidv7() string {
	var b [9]byte
	_, _ = rand.Read(b[:])
	hexs := hex.EncodeToString(b[:])
	t := fmt.Sprintf("%012x", time.Now().UnixMilli())
	return t[:8] + "-" + t[8:12] + "-7" + hexs[:3] + "-9" + hexs[3:6] + "-" + hexs[6:18]
}

func (l monitorLauncher) Launch(ctx context.Context, roomID, assignee, instruction string) (string, error) {
	actor := room.Actor{ParticipantID: "par_owner", Kind: "human"} // 本地 owner（监控为用户登记的自动化）
	for attempt := 0; attempt < 3; attempt++ {
		version, err := l.store.RoomVersion(ctx, roomID)
		if err != nil {
			return "", fmt.Errorf("monitor: room version: %w", err)
		}
		res, err := l.svc.ExecuteCommand(ctx, actor, room.Command{
			RoomID: roomID, CommandKind: "run_task", ExpectedRoomVersion: version,
			IdempotencyKey: uuidv7(), IssuedAt: time.Now().UTC().Format(time.RFC3339),
			Payload: mustJSONApp(map[string]any{"assignee": assignee, "instruction": instruction}),
		})
		if err != nil {
			if strings.Contains(err.Error(), "版本冲突") && attempt < 2 {
				continue // 良性交错：换期位重试
			}
			return "", err
		}
		return res.EventID, nil
	}
	return "", fmt.Errorf("monitor: 版本冲突重试耗尽")
}

// monitorRunStatus run.requested 事件引用 → run 投影状态（completed/failed/…
// "" = 事件尚未可见）。单源单在途——全量读面成本可接受（本地单用户房间规模）。
func monitorRunStatus(reader room.EventReader) monitor.RunStatus {
	return func(ctx context.Context, roomID, ref string) string {
		events, _, err := reader.EventsAfter(ctx, roomID, "", 100000)
		if err != nil {
			return ""
		}
		runID := ""
		for _, ev := range events {
			if ev.Envelope.EventID == ref && ev.Envelope.Type == protocol.EventRunRequested {
				if p, ok := ev.Envelope.DecodePayload().(protocol.RunRequestedPayload); ok {
					runID = p.RunID
				}
			}
		}
		if runID == "" {
			return ""
		}
		for _, v := range room.RunsOf(events) {
			if v.RunID == runID {
				return v.Status
			}
		}
		return ""
	}
}

// startMonitor 装配并启动监控调度（10s 调度刻；随主 ctx 终止）。
func startMonitor(ctx context.Context, dataDir string, svc *room.Service, store *sqlite.Store, logger *slog.Logger, newTx func(string) string) (*monitor.Manager, error) {
	mgr, err := monitor.Load(dataDir + "/monitors.json")
	if err != nil {
		return nil, fmt.Errorf("app: monitor: %w", err)
	}
	mgr.Launcher = monitorLauncher{svc: svc, store: store}
	mgr.Status = monitorRunStatus(store)
	mgr.Fetch = monitor.FetchOf
	mgr.Logger = logger
	mgr.NewID = newTx
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				mgr.Tick(tctx)
				cancel()
			}
		}
	}()
	return mgr, nil
}

func mustJSONApp(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return raw
}
