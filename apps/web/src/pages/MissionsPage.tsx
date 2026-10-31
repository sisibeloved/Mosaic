// 任务中心（0.6.0 / ADR-0023）：Mission 契约——创建（完备 → ready / 缺失 →
// draft）、列表、详情（当前契约 + 版本历史 + 事件流）、修订（CAS 冲突可见）、
// 取消。0.6.2 增执行上下文清单（装配存档 + 历史 + 层账本）。请求序号守卫与
// 错误态纪律沿用 0.5 系列教训。
import { useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  api,
  ApiError,
  type BindingView,
  type ContextManifestSummary,
  type ContextManifestView,
  type IdentityAgent,
  type MissionEvent,
  type MissionSummary,
  type MissionView,
  type SlotInstanceView,
} from "../api/client";
import { absoluteTime } from "../lib/ui";

const splitLines = (s: string): string[] =>
  s.split("\n").map((x) => x.trim()).filter(Boolean);

function statusClass(status: string): string {
  if (status === "ready") return "text-ok";
  if (status === "canceled") return "text-danger line-through";
  return "text-warn";
}

export function MissionsPage() {
  const [missions, setMissions] = useState<MissionSummary[] | null>(null);
  const [listErr, setListErr] = useState<string | null>(null);
  const [agents, setAgents] = useState<{ agent_id: string; display_name: string }[]>([]);
  const [formOpen, setFormOpen] = useState(false);
  const [searchParams] = useSearchParams();
  const [form, setForm] = useState(() => ({
    objective: "", scope: "", acceptance: "", deliverables: "", sources: "", returns: "",
    owner: "", reviewer: "",
    // 交办入口预填（任务卡「交办」跳转 ?room=&task=——ADR-0024 / 0.6.1）
    room: searchParams.get("room") ?? "", task: searchParams.get("task") ?? "",
  }));
  const [budgetOpen, setBudgetOpen] = useState(false);
  const [budget, setBudget] = useState({ concurrency: "1", max_runs: "32", max_wall_clock_seconds: "86400", max_retries: "0", max_delegation_depth: "0" });
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [rev, setRev] = useState(0);
  const seq = useRef(0);

  useEffect(() => {
    const cur = ++seq.current;
    setListErr(null);
    void Promise.all([api.listMissions(), api.identityAgents()])
      .then(([m, a]) => {
        if (seq.current !== cur) return;
        setMissions(m.missions);
        const active = a.agents.filter((x: IdentityAgent) => x.status === "active");
        setAgents(active.map((x) => ({ agent_id: x.agent_id, display_name: x.display_name })));
      })
      .catch((e) => {
        if (seq.current !== cur) return;
        setListErr(e instanceof Error ? e.message : String(e));
      });
  }, [rev]);

  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setMsg(null);
    try {
      await fn();
      setRev((r) => r + 1);
    } catch (e) {
      setMsg(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const submitCreate = () => {
    // 来源半填拦截（审校 P2#2）：两项须同时为空或同时非空——半填静默丢
    // 失来源不可事后补（映射为创建事实不可修订）
    const hasRoom = form.room.trim() !== "";
    const hasTask = form.task.trim() !== "";
    if (hasRoom !== hasTask) {
      setMsg("房间来源须同时填写 room_id 与 task_id（或同时留空）——来源创建后不可补");
      return;
    }
    const b = {
      concurrency: Number(budget.concurrency), max_runs: Number(budget.max_runs),
      max_wall_clock_seconds: Number(budget.max_wall_clock_seconds),
      max_retries: Number(budget.max_retries), max_delegation_depth: Number(budget.max_delegation_depth),
    };
    void act(async () => {
      const m = await api.createMission({
        objective: form.objective.trim(),
        scope: form.scope.trim(),
        ...(form.room.trim() && form.task.trim()
          ? { room_source: { room_id: form.room.trim(), task_id: form.task.trim() } }
          : {}),
        ...(form.owner ? { owner_agent_id: form.owner } : {}),
        acceptance_criteria: splitLines(form.acceptance),
        deliverable_specs: splitLines(form.deliverables),
        source_refs: splitLines(form.sources),
        ...(form.returns.trim() ? { return_targets: splitLines(form.returns) } : {}),
        ...(form.reviewer.trim() ? { reviewer: form.reviewer.trim() } : {}),
        ...(budgetOpen ? { budget: b } : {}),
      });
      setSelected(m.mission_id);
      setFormOpen(false);
      setForm({ objective: "", scope: "", acceptance: "", deliverables: "", sources: "", returns: "", owner: "", reviewer: "", room: "", task: "" });
    });
  };

  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-4 px-4 py-6">
      <header className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-medium">任务</h1>
          <p className="mt-0.5 text-xs text-faint">
            Mission 契约（ADR-0023 / 0.6.0）：可验收的一次交办——独立事实源，带版本；执行与验收随 0.6.5+。
          </p>
        </div>
        <button
          type="button"
          disabled={busy}
          onClick={() => setFormOpen((o) => !o)}
          className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
        >
          {formOpen ? "收起" : "新建交办"}
        </button>
      </header>

      {msg && <p className="text-xs text-danger">{msg}</p>}

      {formOpen && (
        <section className="flex flex-col gap-2 rounded-xl border border-border bg-surface p-3">
          <label className="flex flex-col gap-1 text-xs">
            <span className="text-faint">目标（1–512 字）</span>
            <input
              value={form.objective}
              onChange={(e) => setForm({ ...form, objective: e.target.value })}
              className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 text-sm outline-none focus:border-accent"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs">
            <span className="text-faint">范围（1–512 字）</span>
            <input
              value={form.scope}
              onChange={(e) => setForm({ ...form, scope: e.target.value })}
              className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 text-sm outline-none focus:border-accent"
            />
          </label>
          <div className="grid gap-2 sm:grid-cols-2">
            <label className="flex flex-col gap-1 text-xs">
              <span className="text-faint">验收标准（每行一项；空 = draft）</span>
              <textarea
                value={form.acceptance}
                onChange={(e) => setForm({ ...form, acceptance: e.target.value })}
                rows={3}
                className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
              />
            </label>
            <label className="flex flex-col gap-1 text-xs">
              <span className="text-faint">交付物（每行一项；空 = draft）</span>
              <textarea
                value={form.deliverables}
                onChange={(e) => setForm({ ...form, deliverables: e.target.value })}
                rows={3}
                className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
              />
            </label>
            <label className="flex flex-col gap-1 text-xs">
              <span className="text-faint">来源引用（每行一项，如 room:abc:seq1；空 = draft）</span>
              <textarea
                value={form.sources}
                onChange={(e) => setForm({ ...form, sources: e.target.value })}
                rows={2}
                className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
              />
            </label>
            <label className="flex flex-col gap-1 text-xs">
              <span className="text-faint">返回位置（每行一项，可空）</span>
              <textarea
                value={form.returns}
                onChange={(e) => setForm({ ...form, returns: e.target.value })}
                rows={2}
                className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
              />
            </label>
          </div>
          <div className="grid gap-2 sm:grid-cols-2">
            <label className="flex flex-col gap-1 text-xs">
              <span className="text-faint">房间来源 room_id（可空——填写后与任务卡唯一映射，同卡不可重复交办）</span>
              <input
                value={form.room}
                onChange={(e) => setForm({ ...form, room: e.target.value })}
                placeholder="如 rm_xxx"
                className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
              />
            </label>
            <label className="flex flex-col gap-1 text-xs">
              <span className="text-faint">任务卡 task_id（tsk_ 前缀，可空）</span>
              <input
                value={form.task}
                onChange={(e) => setForm({ ...form, task: e.target.value })}
                placeholder="如 tsk_xxx"
                className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
              />
            </label>
          </div>
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <label className="flex items-center gap-1">
              <span className="text-faint">负责人</span>
              <select
                value={form.owner}
                onChange={(e) => setForm({ ...form, owner: e.target.value })}
                className="rounded border border-border bg-surface-2 px-1.5 py-1 outline-none"
              >
                <option value="">未指派（draft）</option>
                {agents.map((a) => (
                  <option key={a.agent_id} value={a.agent_id}>
                    {a.display_name}
                  </option>
                ))}
              </select>
            </label>
            <label className="flex items-center gap-1">
              <span className="text-faint">验收人（可空 = 请求人自验）</span>
              <input
                value={form.reviewer}
                onChange={(e) => setForm({ ...form, reviewer: e.target.value })}
                placeholder="Agent ID 或留空"
                className="w-44 rounded border border-border bg-surface-2 px-1.5 py-1 outline-none"
              />
            </label>
            <button type="button" onClick={() => setBudgetOpen((o) => !o)} className="underline text-faint">
              {budgetOpen ? "收起预算" : "预算（缺省 1/32/一天/不重试/不委派）"}
            </button>
          </div>
          {budgetOpen && (
            <div className="grid grid-cols-2 gap-2 sm:grid-cols-5">
              {(
                [
                  ["concurrency", "并发"],
                  ["max_runs", "总 Run 数"],
                  ["max_wall_clock_seconds", "墙钟(秒)"],
                  ["max_retries", "重试"],
                  ["max_delegation_depth", "委派深度"],
                ] as const
              ).map(([k, label]) => (
                <label key={k} className="flex flex-col gap-1 text-xs">
                  <span className="text-faint">{label}</span>
                  <input
                    value={budget[k]}
                    onChange={(e) => setBudget({ ...budget, [k]: e.target.value })}
                    className="rounded border border-border bg-surface-2 px-1.5 py-1 font-mono text-xs outline-none"
                  />
                </label>
              ))}
            </div>
          )}
          <div>
            <button
              type="button"
              disabled={busy || !form.objective.trim() || !form.scope.trim()}
              onClick={submitCreate}
              className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
            >
              {busy ? "提交中…" : "创建契约"}
            </button>
          </div>
        </section>
      )}

      {listErr ? (
        <p className="text-xs text-danger">
          {listErr}
          <button type="button" onClick={() => setRev((r) => r + 1)} className="ml-2 underline">
            重试
          </button>
        </p>
      ) : missions === null ? (
        <p className="text-xs text-faint">加载中…</p>
      ) : missions.length === 0 ? (
        <p className="text-xs text-faint">尚无交办——新建后在此管理契约与版本。</p>
      ) : (
        <ul className="divide-y divide-border rounded-xl border border-border">
          {missions.map((m) => (
            <li key={m.mission_id} className="flex items-center gap-3 px-3 py-2.5">
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-1.5 text-sm">
                  <span className={statusClass(m.status)}>{m.status}</span>
                  <span className="font-medium">{m.objective}</span>
                  <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-dim">v{m.contract_version}</span>
                </div>
                <p className="mt-0.5 truncate font-mono text-[11px] text-faint" title={m.mission_id}>
                  {m.mission_id} · {m.owner_agent_id || "unassigned"} · 修订于 {absoluteTime(m.updated_at)}
                </p>
              </div>
              <button
                type="button"
                onClick={() => setSelected(selected === m.mission_id ? null : m.mission_id)}
                className="shrink-0 rounded-lg bg-surface-3 px-2 py-1 text-xs text-text transition-opacity hover:opacity-85"
              >
                {selected === m.mission_id ? "收起" : "详情"}
              </button>
            </li>
          ))}
        </ul>
      )}

      {selected && (
        <MissionDetail key={selected} missionID={selected} agents={agents} onChanged={() => setRev((r) => r + 1)} />
      )}
    </div>
  );
}

/** 契约详情：当前契约 + 修订（CAS）+ 取消 + 版本历史 + 事件流。 */
function MissionDetail({
  missionID,
  agents,
  onChanged,
}: {
  missionID: string;
  agents: { agent_id: string; display_name: string }[];
  onChanged: () => void;
}) {
  const [detail, setDetail] = useState<MissionView | null>(null);
  const [versions, setVersions] = useState<MissionView[] | null>(null);
  const [events, setEvents] = useState<MissionEvent[] | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [reviseOpen, setReviseOpen] = useState(false);
  const [form, setForm] = useState({ objective: "", scope: "", acceptance: "", deliverables: "", sources: "", owner: "" });
  const [cancelReason, setCancelReason] = useState("");
  const seq = useRef(0);

  const load = () => {
    const cur = ++seq.current;
    setLoadErr(null);
    void Promise.all([api.getMission(missionID), api.listMissionVersions(missionID), api.listMissionEvents(missionID)])
      .then(([d, v, e]) => {
        if (seq.current !== cur) return;
        setDetail(d);
        setVersions(v.versions);
        setEvents(e.events);
      })
      .catch((e) => {
        if (seq.current !== cur) return;
        setLoadErr(e instanceof Error ? e.message : String(e));
      });
  };

  useEffect(load, [missionID]); // 父层按 missionID key 重挂载；onChanged 后手动 load

  // 写入归属守卫（审校 P1#1）：详情加载完成且归属当前任务才允许修订/取消——
  // 迟到响应或异常状态下绝不把写请求发给错误目标
  const owned = detail !== null && detail.mission_id === missionID;

  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setMsg(null);
    try {
      await fn();
      load();
      onChanged();
    } catch (e) {
      setMsg(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  if (loadErr) {
    return (
      <section className="rounded-xl border border-border p-3 text-xs text-danger">
        {loadErr}
        <button type="button" onClick={load} className="ml-2 underline">
          重试
        </button>
      </section>
    );
  }
  if (!detail) {
    return <p className="text-xs text-faint">契约加载中…</p>;
  }

  return (
    <section className="flex flex-col gap-2 rounded-xl border border-border bg-surface p-3 text-xs">
      <div className="flex flex-wrap items-center gap-2">
        <span className={statusClass(detail.status)}>{detail.status}</span>
        <span className="font-mono text-faint">{detail.mission_id}</span>
        <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-dim">v{detail.contract_version}</span>
        <span className="text-faint">
          负责人：{detail.owner_agent_id || "unassigned"} · 验收人：{detail.reviewer || "请求人自验"}
          {detail.room_source && (
            <span className="ml-1 rounded bg-surface-3 px-1.5 py-px font-mono text-[10px]" title="房间交办来源（创建事实，不可修订）">
              {detail.room_source.room_id}/{detail.room_source.task_id}
            </span>
          )}
        </span>
      </div>
      <p className="text-sm">{detail.objective}</p>
      <p className="text-dim">范围：{detail.scope}</p>
      <div className="grid gap-1 sm:grid-cols-2">
        <p className="text-dim">验收：{detail.acceptance_criteria.join("；") || "（缺失 → draft）"}</p>
        <p className="text-dim">交付：{detail.deliverable_specs.join("；") || "（缺失）"}</p>
        <p className="font-mono text-faint">来源：{detail.source_refs.join("；") || "（缺失）"}</p>
        <p className="font-mono text-faint">
          预算：{detail.budget.concurrency} 并发 / {detail.budget.max_runs} Run / {detail.budget.max_wall_clock_seconds}s / 重试 {detail.budget.max_retries} / 委派 {detail.budget.max_delegation_depth}
        </p>
      </div>
      {msg && <p className="text-danger">{msg}</p>}

      {detail.status !== "canceled" && (
        <div className="flex flex-wrap items-center gap-2">
          <button
            type="button"
            disabled={busy || !owned}
            onClick={() => {
              setReviseOpen((o) => !o);
              setForm({
                objective: detail.objective,
                scope: detail.scope,
                acceptance: detail.acceptance_criteria.join("\n"),
                deliverables: detail.deliverable_specs.join("\n"),
                sources: detail.source_refs.join("\n"),
                owner: detail.owner_agent_id,
              });
            }}
            className="rounded-lg bg-surface-3 px-2 py-1 disabled:opacity-40"
          >
            {reviseOpen ? "收起修订" : "修订契约"}
          </button>
          <input
            value={cancelReason}
            onChange={(e) => setCancelReason(e.target.value)}
            placeholder="取消原因（可选，≤200 字）"
            className="min-w-0 flex-1 rounded border border-border bg-surface-2 px-1.5 py-1 outline-none"
          />
          <button
            type="button"
            disabled={busy || !owned}
            onClick={() =>
              void act(async () => {
                if (detail.mission_id !== missionID) throw new Error("详情未就绪，已阻止写入");
                await api.cancelMission(missionID, detail.contract_version, cancelReason.trim() || undefined);
              })
            }
            className="rounded-lg bg-surface-3 px-2 py-1 text-danger disabled:opacity-40"
          >
            取消交办
          </button>
        </div>
      )}

      {reviseOpen && detail.status !== "canceled" && (
        <div className="flex flex-col gap-2 rounded-lg bg-surface-2 p-2">
          <p className="text-faint">
            全量替换 + 版本 v{detail.contract_version + 1}；基于 v{detail.contract_version}（CAS——并发修订冲突 409 可见）。
          </p>
          <div className="grid gap-2 sm:grid-cols-2">
            <input
              value={form.objective}
              onChange={(e) => setForm({ ...form, objective: e.target.value })}
              placeholder="目标"
              className="rounded border border-border bg-surface px-1.5 py-1 outline-none"
            />
            <input
              value={form.scope}
              onChange={(e) => setForm({ ...form, scope: e.target.value })}
              placeholder="范围"
              className="rounded border border-border bg-surface px-1.5 py-1 outline-none"
            />
            <textarea
              value={form.acceptance}
              onChange={(e) => setForm({ ...form, acceptance: e.target.value })}
              rows={2}
              placeholder="验收标准（每行一项）"
              className="rounded border border-border bg-surface px-1.5 py-1 font-mono outline-none"
            />
            <textarea
              value={form.deliverables}
              onChange={(e) => setForm({ ...form, deliverables: e.target.value })}
              rows={2}
              placeholder="交付物（每行一项）"
              className="rounded border border-border bg-surface px-1.5 py-1 font-mono outline-none"
            />
            <textarea
              value={form.sources}
              onChange={(e) => setForm({ ...form, sources: e.target.value })}
              rows={2}
              placeholder="来源引用（每行一项）"
              className="rounded border border-border bg-surface px-1.5 py-1 font-mono outline-none"
            />
            <select
              value={form.owner}
              onChange={(e) => setForm({ ...form, owner: e.target.value })}
              className="rounded border border-border bg-surface px-1.5 py-1 outline-none"
            >
              <option value="">未指派（draft）</option>
              {agents.map((a) => (
                <option key={a.agent_id} value={a.agent_id}>
                  {a.display_name}
                </option>
              ))}
            </select>
          </div>
          <div>
            <button
              type="button"
              disabled={busy || !owned || !form.objective.trim() || !form.scope.trim()}
              onClick={() =>
                void act(async () => {
                  if (detail.mission_id !== missionID) throw new Error("详情未就绪，已阻止写入");
                  // 全量替换语义：未编辑字段从当前契约快照保留（审校 P1#2——
                  // 漏传即清空，预算/约束/返回位/验收人/委派不得被缺省重置）
                  await api.reviseMission(missionID, {
                    expected_version: detail.contract_version,
                    ...(form.owner ? { owner_agent_id: form.owner } : {}),
                    objective: form.objective.trim(),
                    scope: form.scope.trim(),
                    acceptance_criteria: splitLines(form.acceptance),
                    deliverable_specs: splitLines(form.deliverables),
                    source_refs: splitLines(form.sources),
                    constraints: detail.constraints,
                    return_targets: detail.return_targets,
                    budget: detail.budget,
                    delegation: detail.delegation,
                    ...(detail.reviewer ? { reviewer: detail.reviewer } : {}),
                  });
                })
              }
              className="rounded-lg bg-accent px-3 py-1.5 text-accent-contrast disabled:opacity-40"
            >
              {busy ? "提交中…" : `提交修订（v${detail.contract_version} → v${detail.contract_version + 1}）`}
            </button>
          </div>
        </div>
      )}

      <details className="mt-1">
        <summary className="cursor-pointer select-none text-faint">
          版本历史{versions === null ? "" : `（${versions.length} 版）`}
        </summary>
        <ul className="mt-1 flex flex-col gap-0.5">
          {(versions ?? []).map((v) => (
            <li key={v.contract_version} className="flex items-center gap-2 text-faint">
              <span className="rounded bg-surface-3 px-1.5 py-px text-[10px]">v{v.contract_version}</span>
              <span className={statusClass(v.status)}>{v.status}</span>
              <span className="truncate">{v.objective}</span>
              <span className="ml-auto shrink-0">{absoluteTime(v.created_at)}</span>
            </li>
          ))}
        </ul>
      </details>

      <details className="mt-1">
        <summary className="cursor-pointer select-none text-faint">
          事件流{events === null ? "" : `（${events.length} 条）`}
        </summary>
        <ul className="mt-1 flex flex-col gap-0.5 font-mono text-[11px]">
          {(events ?? []).map((e) => (
            <li key={e.seq} className="flex items-center gap-2 text-faint">
              <span className="shrink-0">#{e.seq}</span>
              <span className="shrink-0 text-dim">{e.event_type}</span>
              <span className="truncate">{JSON.stringify(e.payload)}</span>
              <span className="ml-auto shrink-0">{absoluteTime(e.occurred_at)}</span>
            </li>
          ))}
        </ul>
      </details>

      {/* 审校 P7：取消后仍渲染（只读——canceled 时仅禁新装配，历史/层内容可查） */}
      <ExecContextCard missionID={missionID} ownerAgentID={detail.owner_agent_id} canceled={detail.status === "canceled"} />
    </section>
  );
}

/** 执行上下文清单（0.6.2 / ADR-0025）：选任职装配存档、层账本、装配历史。 */
function ExecContextCard({ missionID, ownerAgentID, canceled }: { missionID: string; ownerAgentID: string; canceled: boolean }) {
  const [manifests, setManifests] = useState<ContextManifestSummary[] | null>(null);
  const [instances, setInstances] = useState<SlotInstanceView[]>([]);
  const [bindings, setBindings] = useState<BindingView[]>([]);
  const [bindingID, setBindingID] = useState("");
  const [result, setResult] = useState<ContextManifestView | null>(null);
  const [replayed, setReplayed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const seq = useRef(0);
  const resultSeq = useRef(0); // 结果请求序号（审校 P3）：装配/历史查看共用——只接纳最新响应

  const load = () => {
    const cur = ++seq.current;
    setErr(null);
    void Promise.all([api.listContextManifests(missionID), api.listSlotInstances(), api.listSlotBindings()])
      .then(([m, i, b]) => {
        if (seq.current !== cur) return;
        setManifests(m.manifests);
        setInstances(i.instances);
        setBindings(b.bindings);
      })
      .catch((e) => {
        if (seq.current !== cur) return;
        setErr(e instanceof Error ? e.message : String(e));
      });
  };
  useEffect(load, [missionID]); // 父层按 missionID key 重挂载
  // 负责人变化清空任职选择（审校 P2）：旧 owner 的选择残留不可提交
  useEffect(() => {
    setBindingID("");
  }, [ownerAgentID]);

  // 执行者须为契约负责人（owner_mismatch 只是 blocked 预演）——默认只列
  // owner 的任职，便于直接选对；无 owner 时提示先指派。
  const owned = bindings.filter((b) => ownerAgentID !== "" && b.agent_id === ownerAgentID);
  const instTitle = (id: string) => instances.find((x) => x.slot_instance_id === id)?.title ?? id;

  const build = async (id: string) => {
    // 提交前核对任职仍属于当前负责人（审校 P2）：迟到列表/并发修订防护
    if (!owned.some((b) => b.binding_id === id)) {
      setErr("所选任职已不属于当前负责人——请重新选择");
      setBindingID("");
      return;
    }
    setBusy(true);
    setErr(null);
    const cur = ++resultSeq.current; // 出发取号（审校 P3）
    try {
      const r = await api.buildContextManifest(missionID, id);
      if (resultSeq.current !== cur) return; // 过期响应弃用
      setResult(r.manifest);
      setReplayed(r.replayed);
      load();
    } catch (e) {
      if (resultSeq.current !== cur) return;
      setErr(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const viewHistory = (manifestID: string) => {
    const cur = ++resultSeq.current; // 与装配共用结果序号（审校 P3）
    void api
      .getContextManifest(manifestID)
      .then((v) => {
        if (resultSeq.current !== cur) return; // 迟到响应不覆盖最新查看/装配结果
        setResult(v);
        setReplayed(false);
      })
      .catch((e) => {
        if (resultSeq.current !== cur) return;
        setErr(e instanceof Error ? e.message : String(e));
      });
  };

  return (
    <details className="mt-1">
      <summary className="cursor-pointer select-none text-faint">
        执行上下文{manifests === null ? "" : `（${manifests.length} 份清单）`}
      </summary>
      <div className="mt-1 flex flex-col gap-2">
        {err && <p className="text-danger">{err}</p>}
        {canceled ? (
          // 审校 P7：取消后保留只读卡——既有层内容与历史仍可查（数据保留，
          // API 仍可读），仅不能再装配（后端 409 mission_canceled）
          <p className="text-faint">任务已取消——不能再装配；既有清单与层内容仍可查看。</p>
        ) : ownerAgentID === "" ? (
          <p className="text-faint">未指派负责人——先修订契约指派 owner 再装配。</p>
        ) : (
          <div className="flex flex-wrap items-center gap-2">
            <select
              value={bindingID}
              onChange={(e) => setBindingID(e.target.value)}
              className="min-w-0 max-w-full rounded border border-border bg-surface-2 px-1.5 py-1 outline-none"
            >
              <option value="">选择负责人任职（{owned.length}）</option>
              {owned.map((b) => (
                <option key={b.binding_id} value={b.binding_id}>
                  {instTitle(b.slot_instance_id)} · {b.status} · v{b.slot_spec_version}
                </option>
              ))}
            </select>
            <button
              type="button"
              disabled={busy || bindingID === ""}
              onClick={() => void build(bindingID)}
              className="rounded-lg bg-accent px-2.5 py-1 text-accent-contrast disabled:opacity-40"
            >
              {busy ? "装配中…" : "装配清单"}
            </button>
            <span className="text-faint">
              确定性装配存档——同状态重建重放原清单（digest 即身份）；执行随 0.6.5。
            </span>
          </div>
        )}

        {result && (
          <div className="flex flex-col gap-1 rounded-lg bg-surface-2 p-2">
            <div className="flex flex-wrap items-center gap-2">
              <span className={result.state === "assembled" ? "text-ok" : "text-warn"}>{result.state}</span>
              {result.reason_code && <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-warn">{result.reason_code}</span>}
              {replayed && <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-dim">重放（同状态重建）</span>}
              <span className="font-mono text-[10px] text-faint" title={result.digest}>
                {result.digest.slice(0, 16)}…
              </span>
              <span className="text-faint">
                契约 v{result.contract_version} · {(result.total_bytes / 1024).toFixed(1)} KiB · {result.policy_version}
              </span>
              {result.watermarks.map((w) => (
                <span key={w.stream} className="rounded bg-surface-3 px-1.5 py-px font-mono text-[10px] text-dim">
                  {w.stream}@{w.position}
                </span>
              ))}
            </div>
            <ul className="flex flex-col gap-0.5 font-mono text-[11px]">
              {result.layers.map((l) => (
                <li key={l.name} className="flex flex-wrap items-center gap-2 text-faint">
                  <span className="w-36 shrink-0 text-dim">{l.name}</span>
                  <span className={l.state === "included" ? "text-ok" : "text-warn"}>{l.state}</span>
                  <span>{l.role}</span>
                  {l.state === "included" && <span>{l.bytes} B</span>}
                  {l.reason_code && <span className="text-warn">{l.reason_code}</span>}
                  {l.dropped ? <span>裁 {l.dropped} 条</span> : null}
                  {l.truncated_messages ? <span>截 {l.truncated_messages} 条</span> : null}
                  {l.content && (
                    <details className="ml-auto">
                      <summary className="cursor-pointer select-none text-[10px]">内容块</summary>
                      <pre className="mt-1 max-h-48 max-w-full overflow-auto rounded bg-surface-3 p-1.5 text-[10px]">
                        {JSON.stringify(l.content, null, 2)}
                      </pre>
                    </details>
                  )}
                </li>
              ))}
            </ul>
          </div>
        )}

        {manifests !== null && manifests.length > 0 && (
          <ul className="flex flex-col gap-0.5">
            {manifests.map((m) => (
              <li key={m.manifest_id} className="flex items-center gap-2 text-faint">
                <button
                  type="button"
                  onClick={() => viewHistory(m.manifest_id)}
                  className="shrink-0 rounded bg-surface-3 px-1.5 py-px text-[10px] hover:opacity-85"
                >
                  查看
                </button>
                <span className={m.state === "assembled" ? "text-ok" : "text-warn"}>{m.state}</span>
                {m.reason_code && <span className="text-warn">{m.reason_code}</span>}
                <span className="font-mono text-[10px]" title={m.digest}>
                  {m.digest.slice(0, 12)}…
                </span>
                <span>v{m.contract_version}</span>
                <span className="ml-auto shrink-0">{absoluteTime(m.created_at)}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </details>
  );
}
