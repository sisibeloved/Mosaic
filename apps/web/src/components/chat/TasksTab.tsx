// 任务 Tab（M3-3 tasklist，RFC-0012 OQ-A / 附录 G）：完整任务清单视图——
// 进行中 / 已完成 / 已收束三段（v1.50 重做：此前只列"待交付承诺"，与 tasklist
// 概念不符且已完成不可见——负责人指出）。提出方与负责人分离显示（v1.50：
// @负责人 指派——A 派 B 时显示"B ← A 指派"）。派生为显式申报协议（mosaic-todo
// 围栏块，v1.49），完成有两条路：agent 申报 [x] 自动结案、人类裁定；已收束
// = 全量替换后未再申报（自动）或人工移除。provenance：点任务定位申报消息。
import { useState } from "react";
import type { ParticipantView } from "../../api/client";
import type { TaskItem } from "../../api/room";
import { displayNameOf, relativeTime } from "../../lib/ui";
import { Avatar } from "./Avatar";

/** 每段展示上限（个人版房间规模；更早的折叠计数）。 */
const SECTION_CAP = 30;

/** 提出方徽标：负责人 ≠ 提出方（A 指派 B）时显示"← A 指派"。 */
function RequesterBadge({
  task,
  participants,
}: {
  task: TaskItem;
  participants: ParticipantView[];
}) {
  if (!task.requester || task.requester === task.owner) return null;
  return (
    <span
      className="rounded bg-surface-3 px-1.5 text-[10px] leading-4 text-dim"
      title={`提出方 ${task.requester}`}
    >
      ← {displayNameOf(participants, task.requester)} 指派
    </span>
  );
}

/** 结案方式：resolved_by 可能是负责人、提出方（agent 路径）或人类（人工门控）。 */
function resolutionLabel(t: TaskItem): string {
  if (t.status === "delivered") {
    if (t.resolved_by === t.owner) {
      return !t.requester || t.requester === t.owner ? "agent 申报完成" : "负责人申报完成";
    }
    if (t.resolved_by && t.resolved_by === t.requester) return "提出方申报完成";
    return "人工裁定交付";
  }
  if (t.resolved_by === t.owner) return "未再申报，自动收束";
  if (t.resolved_by && t.resolved_by === t.requester) return "提出方撤回";
  return "人工移除";
}

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
  // 已完成/已收束按结案时间倒序（最新在前），截 SECTION_CAP
  const delivered = tasks
    .filter((t) => t.status === "delivered")
    .slice(-SECTION_CAP)
    .reverse();
  const dismissed = tasks
    .filter((t) => t.status === "dismissed")
    .slice(-SECTION_CAP)
    .reverse();

  return (
    <div className="py-1">
      {tasks.length === 0 && (
        <p className="px-3 py-2 text-xs text-faint">
          尚无任务。Agent 在回复里用 mosaic-todo 围栏块申报待办（语境中已内置申报协议），按责任人跟踪到交付。
        </p>
      )}

      {pending.length > 0 && (
        <>
          <h3 className="px-3 pb-1 pt-2 text-xs font-medium text-dim">进行中（{pending.length}）</h3>
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
                  <RequesterBadge task={t} participants={participants} />
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
                  {t.text}
                </button>
                <div className="mt-1.5 flex gap-1.5">
                  <button
                    type="button"
                    disabled={busyTaskID === t.task_id}
                    onClick={() => onResolve(t.task_id, "delivered")}
                    className="rounded-lg bg-surface-3 px-2.5 py-1 text-[11px] text-text transition-opacity hover:opacity-85 disabled:opacity-40"
                  >
                    {busyTaskID === t.task_id ? "处理中…" : "确认交付"}
                  </button>
                  <button
                    type="button"
                    disabled={busyTaskID === t.task_id}
                    onClick={() => onResolve(t.task_id, "dismissed")}
                    className="rounded-lg px-2.5 py-1 text-[11px] text-dim transition-colors hover:bg-surface-2 hover:text-text disabled:opacity-40"
                  >
                    移除
                  </button>
                </div>
              </li>
            ))}
          </ul>
        </>
      )}

      {delivered.length > 0 && (
        <>
          <h3 className="px-3 pb-1 pt-3 text-xs font-medium text-dim">
            已完成（{tasks.filter((t) => t.status === "delivered").length}）
          </h3>
          <ul className="divide-y divide-border">
            {delivered.map((t) => (
              <li key={t.task_id} className="flex items-start gap-2 px-3 py-2 text-xs">
                <span className="mt-0.5 shrink-0 text-ok">✓</span>
                <div className="min-w-0 flex-1">
                  <button
                    type="button"
                    onClick={() => onJumpToEvent(t.source_event_id)}
                    className="block w-full text-left text-dim transition-colors hover:text-text"
                    title={`定位申报消息（${t.source_event_id}）`}
                  >
                    {t.text}
                  </button>
                  <p className="mt-0.5 text-[11px] text-faint">
                    {displayNameOf(participants, t.owner)} ·{" "}
                    {t.requester !== t.owner &&
                      t.requester &&
                      `${displayNameOf(participants, t.requester)} 提出 · `}
                    {resolutionLabel(t)}
                    {t.resolved_at ? ` · ${relativeTime(t.resolved_at)}` : ""}
                    {t.note && t.resolved_by !== t.owner && t.resolved_by !== t.requester ? `（${t.note}）` : ""}
                  </p>
                </div>
              </li>
            ))}
          </ul>
        </>
      )}

      {dismissed.length > 0 && (
        <>
          <h3 className="px-3 pb-1 pt-3 text-xs font-medium text-dim">
            已收束（{tasks.filter((t) => t.status === "dismissed").length}）
          </h3>
          <ul className="divide-y divide-border">
            {dismissed.map((t) => (
              <li key={t.task_id} className="flex items-start gap-2 px-3 py-2 text-xs">
                <span className="mt-0.5 shrink-0 text-faint">—</span>
                <div className="min-w-0 flex-1">
                  <button
                    type="button"
                    onClick={() => onJumpToEvent(t.source_event_id)}
                    className="block w-full text-left text-faint line-through decoration-border transition-colors hover:text-dim"
                    title={`定位申报消息（${t.source_event_id}）`}
                  >
                    {t.text}
                  </button>
                  <p className="mt-0.5 text-[11px] text-faint">
                    {displayNameOf(participants, t.owner)} ·{" "}
                    {t.requester !== t.owner &&
                      t.requester &&
                      `${displayNameOf(participants, t.requester)} 提出 · `}
                    {resolutionLabel(t)}
                    {t.resolved_at ? ` · ${relativeTime(t.resolved_at)}` : ""}
                    {t.note && t.resolved_by !== t.owner && t.resolved_by !== t.requester ? `（${t.note}）` : ""}
                  </p>
                </div>
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
