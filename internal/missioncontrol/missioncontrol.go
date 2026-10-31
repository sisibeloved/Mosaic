// Package missioncontrol 任务控制编排（0.6.6 / ADR-0029 决策 3）：组合
// mission 状态机（CAS）与 run 停止面——
//
//	Pause：CAS ready|running → paused（持久先行即阻止新调度）→ 发停止
//	       信号（不等待——在途 Run 异步终理化，Mission 已 paused）
//	Resume：CAS paused → ready（调度器下一轮自动拾取；不重启已停 Run）
//	Cancel：CAS 非终态 → canceling → 发停止信号 → 有界等待未终态清零 →
//	       FinishCancel（canceling → canceled）；超时返回 canceling
//	       （“待停止”可见——可续驱；重启恢复分派兜底）
//
// 独立小包（import mission + run，不进任一域——域间不互相 import 的装配
// 约定）；httpapi 经接口消费（Deps.MissionControl），app 装配注入。
package missioncontrol

import (
	"context"
	"time"

	"github.com/sisibeloved/Mosaic/internal/mission"
	missionrun "github.com/sisibeloved/Mosaic/internal/run"
)

// 缺省有界等待：执行器不响应取消时不得挂起请求——超时返回 canceling
// （RFC §3.2“无法安全终止……显示待核验”）。
const defaultCancelWait = 10 * time.Second

// Service 控制编排服务。
type Service struct {
	missions   *mission.Service
	runs       *missionrun.Service
	cancelWait time.Duration
}

// Option 装配选项。
type Option func(*Service)

// WithCancelWait 取消有界等待时长（测试注入短等待）。
func WithCancelWait(d time.Duration) Option { return func(s *Service) { s.cancelWait = d } }

// New 构造编排服务。
func New(missions *mission.Service, runs *missionrun.Service, opts ...Option) *Service {
	s := &Service{missions: missions, runs: runs, cancelWait: defaultCancelWait}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// PauseMission 暂停（= pause_mission）：持久状态先行（CAS ready|running →
// paused——出调度列），再发停止信号（来自 ready 时为无在途无操作）。响应
// 不等在途 Run 停止完成——paused 已持久即已阻止新调度；Run 区随后显示
// canceled（stop_requested{pause} 审计）。
func (s *Service) PauseMission(ctx context.Context, missionID string, expectedVersion int, reason string) (mission.Mission, error) {
	out, err := s.missions.Pause(ctx, missionID, expectedVersion, reason)
	if err != nil {
		return mission.Mission{}, err
	}
	if _, err := s.runs.SignalStopByMission(ctx, missionID, "pause"); err != nil {
		// 信号面失败不回滚暂停（意图已持久——在途 Run 终理化交由续驱/
		// 恢复兜底）；错误上抛供调用方感知。
		return out, err
	}
	return out, nil
}

// ResumeMission 恢复（= resume_mission）：paused → ready——此后调度器自动
// 拾取；无在途处置。
func (s *Service) ResumeMission(ctx context.Context, missionID string, expectedVersion int) (mission.Mission, error) {
	return s.missions.Resume(ctx, missionID, expectedVersion)
}

// CancelMission 取消（= cancel_mission，两段）：停止意图持久化（canceling）
// → 停止信号 → 有界等待（独立上下文——请求断开不丢终态翻转）→ 收尾
// （canceled）。无在途时同步直达；超时返回 canceling（可重复 cancel 续驱）。
func (s *Service) CancelMission(ctx context.Context, missionID string, expectedVersion int, reason string) (mission.Mission, error) {
	cur, err := s.missions.MarkCanceling(ctx, missionID, expectedVersion, reason)
	if err != nil {
		return mission.Mission{}, err
	}
	if _, err := s.runs.SignalStopByMission(ctx, missionID, "cancel"); err != nil {
		return cur, err
	}
	// 有界等待用独立上下文：请求断开后等待/收尾照走（终态翻转不因客户端
	// 断开而丢失）。
	waitCtx, cancel := context.WithTimeout(context.Background(), s.cancelWait)
	defer cancel()
	deadline := time.Now().Add(s.cancelWait)
	for {
		has, err := s.runs.HasUnfinished(waitCtx, missionID)
		if err != nil {
			break
		}
		if !has {
			if out, ferr := s.missions.FinishCancel(waitCtx, missionID, reason); ferr == nil {
				return out, nil
			}
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 超时/中断：返回当前状态（canceling 可见——待停止）。
	got, ok, err := s.missions.Get(waitCtx, missionID)
	if err != nil || !ok {
		return cur, nil
	}
	return got, nil
}
