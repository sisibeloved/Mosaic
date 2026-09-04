// 任务 Tab（M3-3 tasklist，RFC-0012 OQ-A 修订 / v1.45 负责人裁定；v1.49 派生改
// 显式申报协议）：带责任人的承诺追踪——agent 在回复里用 mosaic-todo 围栏块申报
// （确定性解析，零 LLM；v1.46 关键字匹配误报严重已废弃）。人工门控保留：
// delivered/dismissed 人类裁定优先，agent 打 x 也可自动结案。pending 按申报序
//（旧债在前）；波龄 ≥2 标 overdue。provenance 跳转：点任务定位申报消息。
import { useState } from "react";
import type { ParticipantView } from "../../api/client";
import type { TaskItem } from "../../api/room";
import { displayNameOf } from "../../lib/ui";
import { Avatar } from "./Avatar";

export function TasksTab({
  tasks,
  participants,
  busyTaskID,
  onResolve,
  onJumpToEvent,
}: {
  tasks: TaskItem[];
  participants: ParticipantView[];
  busyTaskID: string | null;
  onResolve: (taskID: string, resolution: "delivered" | "dismissed") => void;
  onJumpToEvent: (eventID: string) => void;
}) {
  const pending = tasks.filter((t) => t.status === "pending");
  const resolved = tasks.filter((t) => t.status !== "pending").slice(-8).reverse();
  return (
    <div className="py-1">
      <h3 className="px-3 pb-1 pt-2 text-xs font-medium text-dim">待交付承诺（{pending.length}）</h3>
      {pending.length === 0 ? (
        <p className="px-3 py-2 text-xs text-faint">
          Agent 在回复里用 mosaic-todo 围栏块申报待办（语境中已内置申报协议），按责任人跟踪到交付。
        </p>
      ) : (
        <ul className="divide-y divide-border">
          {pending.map((t) => (
            <li key={t.task_id} className="px-3 py-2 text-xs">
              <div className="flex items-center gap-2">
                <Avatar
                  participantID={t.owner}
                  displayName={displayNameOf(participants, t.owner)}
                  size={20}
                />
                <span className="font-medium text-text">{displayNameOf(participants, t.owner)}</span>
                {t.overdue && (
                  <span className="rounded bg-[color-mix(in_srgb,var(--warn)_14%,transparent)] px-1.5 text-[10px] leading-4 text-warn">
                    已 {t.waves_since} 波未交付
                  </span>
                )}
                <span className="ml-auto text-faint">{t.waves_since} 波</span>
              </div>
              <button
                type="button"
                onClick={() => onJumpToEvent(t.source_event_id)}
                className="mt-1 block w-full text-left text-dim transition-colors hover:text-text"
                title={`定位申报消息（${t.source_event_id}）`}
              >
                “{t.text}”
              </button>
              <div className="mt-1.5 flex gap-1.5">
                <button
                  type="button"
                  disabled={busyTaskID === t.task_id}
                  onClick={() => onResolve(t.task_id, "delivered")}
                  className="rounded-lg bg-surface-3 px-2.5 py-1 text-[11px] text-text transition-opacity hover:opacity-85 disabled:opacity-40"
                >
                  {busyTaskID === t.task_id ? "处理中…" : "已交付"}
                </button>
                <button
                  type="button"
                  disabled={busyTaskID === t.task_id}
                  onClick={() => onResolve(t.task_id, "dismissed")}
                  className="rounded-lg px-2.5 py-1 text-[11px] text-dim transition-colors hover:bg-surface-2 hover:text-text disabled:opacity-40"
                >
                  误报，移除
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
      {resolved.length > 0 && (
        <>
          <h3 className="px-3 pb-1 pt-3 text-xs font-medium text-dim">已裁定（近 8 条）</h3>
          <ul className="px-3">
            {resolved.map((t) => (
              <li key={t.task_id} className="py-1 text-xs text-faint">
                {displayNameOf(participants, t.owner)}：{t.resolution === "dismissed" ? "误报移除" : "已交付"}
                {t.note ? `（${t.note}）` : ""}
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

/** 任务面板本地态：当前处理中的 task_id（按钮禁用与文案）。 */
export function useTaskBusy() {
  return useState<string | null>(null);
}
