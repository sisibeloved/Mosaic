// 任务 Tab（M3-3 tasklist，RFC-0012 OQ-A / 附录 G）：完整任务清单视图——
// 进行中 / 已完成 / 已收束三段（v1.50 重做：此前只列"待交付承诺"，与 tasklist
// 概念不符且已完成不可见——负责人指出）。提出方与负责人分离显示（v1.50：
// @负责人 指派——A 派 B 时显示"B ← A 指派"）。派生为显式申报协议（mosaic-todo
// 围栏块，v1.49），完成有两条路：agent 申报 [x] 自动结案、人类裁定；已收束
// = 全量替换后未再申报（自动）或人工移除。provenance：点任务定位申报消息。
import { useState } from "react";
import type { ParticipantView } from "../../api/client";
import type { RunItem, TaskItem } from "../../api/room";
import { copyText } from "../../lib/clipboard";
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

/** M4-1 切片 B：run 状态 chip——完成态可跳结果消息；迟到态携未发布正文
 *（审计留存面，人类可复制救济）；失败/未知携原因提示。 */
function RunChip({ run, onJumpToEvent }: { run: RunItem; onJumpToEvent: (eventID: string) => void }) {
  const [copied, setCopied] = useState(false);
  const resultID = run.result_event_id; // const 捕获：闭包内保持收窄
  const label =
    run.status === "running"
      ? "执行中…"
      : run.status === "requested"
        ? "排队中"
        : run.status === "completed"
          ? run.late
            ? "迟到结果（未发布）"
            : "已执行 ✓"
          : run.status === "unknown"
            ? "结果未知"
            : run.status === "canceled"
              ? run.late
                ? "已取消（有迟到结果）"
                : "已取消"
              : "执行失败";
  const tone =
    run.status === "failed" || run.status === "unknown"
      ? "text-danger"
      : run.status === "running" || run.status === "requested"
        ? "text-warn"
        : run.late
          ? "text-warn"
          : "text-dim";
  const hint =
    run.error ||
    (run.late ? run.result_body || "（迟到正文未留存）" : run.run_id);
  return (
    <>
      {run.status === "completed" && !run.late && resultID ? (
        <button
          type="button"
          onClick={() => onJumpToEvent(resultID)}
          title={`定位结果消息（${resultID}）`}
          className={`rounded-lg border border-border px-2 py-1 text-[11px] transition-colors hover:border-faint ${tone}`}
        >
          {label}
        </button>
      ) : (
        <span className={`rounded-lg border border-border px-2 py-1 text-[11px] ${tone}`} title={hint}>
          {label}
        </span>
      )}
      {run.late && run.result_body && (
        <details className="w-full rounded-lg border border-border px-2 py-1">
          <summary className="cursor-pointer text-[11px] text-dim">迟到结果正文（未发布——引擎不代发，由你决定去向）</summary>
          <pre className="mt-1 max-h-40 overflow-y-auto whitespace-pre-wrap break-words text-[11px] leading-4 text-dim">
            {run.result_body}
          </pre>
          <button
            type="button"
            onClick={() =>
              void copyText(run.result_body ?? "").then((ok) => {
                if (ok) {
                  setCopied(true);
                  window.setTimeout(() => setCopied(false), 1500);
                }
              })
            }
            className="mt-1 rounded-lg bg-surface-3 px-2 py-0.5 text-[11px] text-text transition-opacity hover:opacity-85"
          >
            {copied ? "已复制" : "复制正文"}
          </button>
        </details>
      )}
    </>
  );
}

export function TasksTab({
  tasks,
  runs,
  participants,
  busyTaskID,
  onResolve,
  onRun,
  onCancelRun,
  onJumpToEvent,
}: {
  tasks: TaskItem[];
  /** M4-1 任务执行通道：task_id 关联的 run 状态（执行按钮/状态 chip）。 */
  runs: RunItem[];
  participants: ParticipantView[];
  busyTaskID: string | null;
  onResolve: (taskID: string, resolution: "delivered" | "dismissed") => void;
  /** 发起独立执行（assignee = 任务负责人；指令 = 任务文本）。 */
  onRun: (taskID: string, assignee: string, instruction: string) => void;
  /** 取消在途执行（M4-1 切片 B：理由 1..280 留痕）。 */
  onCancelRun: (runID: string, reason: string) => void;
  onJumpToEvent: (eventID: string) => void;
}) {
  const pending = tasks.filter((t) => t.status === "pending");
  // M4-1 切片 B：取消在途执行——理由 1..280 留痕（事件载荷可追溯）
  const [canceling, setCanceling] = useState<RunItem | null>(null);
  const [cancelReason, setCancelReason] = useState("");
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
                <div className="mt-1.5 flex flex-wrap items-center gap-1.5">
                  {(() => {
                    // M4-1 切片 B：最新关联 run 的 chip + 操作面——在途（排队/执行中）
                    // 给"取消"；终态（完成/失败/取消/未知）后"执行"重新可用（重发路径：
                    // 结果未知的 run 不自动重跑，由人显式重发）。
                    const linked = runs.filter((r) => r.task_id === t.task_id);
                    const latest = linked[linked.length - 1];
                    const active =
                      !!latest && (latest.status === "running" || latest.status === "requested");                    return (
                      <>
                        {latest && <RunChip run={latest} onJumpToEvent={onJumpToEvent} />}
                        {active ? (
                          <button
                            type="button"
                            onClick={() => {
                              setCanceling(latest ?? null);
                              setCancelReason("");
                            }}
                            title="取消在途执行（进程组击杀；理由留痕）"
                            className="rounded-lg px-2.5 py-1 text-[11px] text-dim transition-colors hover:bg-[color-mix(in_srgb,var(--danger)_12%,transparent)] hover:text-danger"
                          >
                            取消
                          </button>
                        ) : (
                          <button
                            type="button"
                            disabled={busyTaskID === t.task_id}
                            onClick={() => onRun(t.task_id, t.owner, t.text)}
                            title="发起独立任务执行（run_task）——负责人在专用通道执行并把结果发回房间"
                            className="rounded-lg bg-accent-soft px-2.5 py-1 text-[11px] text-accent transition-opacity hover:opacity-85 disabled:opacity-40"
                          >
                            执行
                          </button>
                        )}
                        {canceling && canceling.run_id === latest?.run_id && (
                          <span className="flex w-full items-center gap-1.5">
                            <input
                              autoFocus
                              value={cancelReason}
                              onChange={(e) => setCancelReason(e.target.value)}
                              placeholder="取消理由（必填，留痕）"
                              className="min-w-0 flex-1 rounded-lg border border-border bg-surface-2 px-2 py-1 text-[11px] outline-none focus:border-accent"
                            />
                            <button
                              type="button"
                              disabled={!cancelReason.trim() || cancelReason.length > 280}
                              onClick={() => {
                                onCancelRun(canceling.run_id, cancelReason.trim());
                                setCanceling(null);
                              }}
                              className="rounded-lg bg-danger px-2 py-1 text-[11px] font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-40"
                            >
                              确认取消
                            </button>
                            <button
                              type="button"
                              onClick={() => setCanceling(null)}
                              className="rounded-lg px-2 py-1 text-[11px] text-dim hover:text-text"
                            >
                              放弃
                            </button>
                          </span>
                        )}
                      </>
                    );
                  })()}
                  {(() => {
                    // M4-2 收尾：执行记录——本任务全部 run 的折叠历史（可追溯定位：
                    // 完成态跳结果消息；迟到/失败携原因）
                    const linked = runs.filter((r) => r.task_id === t.task_id);
                    if (linked.length <= 1) return null;
                    return (
                      <details className="w-full rounded-lg border border-border px-2 py-1">
                        <summary className="cursor-pointer text-[11px] text-faint">
                          执行记录（{linked.length} 次）
                        </summary>
                        <ul className="mt-1 flex flex-col gap-0.5">
                          {[...linked].reverse().map((r) => (
                            <li key={r.run_id} className="flex items-baseline gap-2 text-[11px]">
                              <span className="text-faint">{relativeTime(r.updated_at)}</span>
                              <span
                                className={
                                  r.status === "failed" || r.status === "unknown"
                                    ? "text-danger"
                                    : r.status === "running" || r.status === "requested"
                                      ? "text-warn"
                                      : "text-dim"
                                }
                              >
                                {r.status === "requested"
                                  ? "排队"
                                  : r.status === "running"
                                    ? "执行中"
                                    : r.status === "completed"
                                      ? r.late
                                        ? "迟到结果"
                                        : "完成"
                                      : r.status === "canceled"
                                        ? "已取消"
                                        : r.status === "unknown"
                                          ? "结果未知"
                                          : "失败"}
                              </span>
                              {r.status === "completed" && !r.late && r.result_event_id ? (
                                <button
                                  type="button"
                                  onClick={() => onJumpToEvent(r.result_event_id ?? "")}
                                  className="text-accent hover:underline"
                                >
                                  结果
                                </button>
                              ) : (
                                (r.error || (r.late ? "迟到未发布" : "")) && (
                                  <span className="min-w-0 truncate text-faint" title={r.error || undefined}>
                                    {r.error ? truncateRun(r.error, 50) : "迟到未发布"}
                                  </span>
                                )
                              )}
                            </li>
                          ))}
                        </ul>
                      </details>
                    );
                  })()}
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

function truncateRun(s: string, n: number): string {
  return s.length > n ? s.slice(0, n) + "…" : s;
}
