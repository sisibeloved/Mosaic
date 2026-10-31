// 运行环境（0.6.3 / ADR-0026；0.6.4 / ADR-0027 增租约诊断区）：本地
// Environment / Runtime——发现同步、实时探测（unhealthy 是数据不是错误）、
// 准备（绑环境 → ready）、事件历史与环境管理；租约（占用/互斥/围栏代次）
// 归诊断视图——领取是 0.6.5 调度行为，不做人工入口。请求序号守卫沿用。
import { useEffect, useRef, useState } from "react";
import {
  api,
  ApiError,
  type EnvironmentView,
  type RuntimeEvent,
  type RuntimeLeaseView,
  type RuntimeView,
} from "../api/client";
import { absoluteTime } from "../lib/ui";

function stateClass(state: string): string {
  if (state === "ready") return "text-ok";
  if (state === "unhealthy") return "text-danger";
  return "text-warn";
}

function leaseClass(state: string): string {
  if (state === "active") return "text-ok";
  if (state === "released") return "text-faint";
  return "text-warn";
}

export function RuntimesPage() {
  const [runtimes, setRuntimes] = useState<RuntimeView[] | null>(null);
  const [environments, setEnvironments] = useState<EnvironmentView[]>([]);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [envForm, setEnvForm] = useState({ name: "", dir: "" });
  const [prepareEnv, setPrepareEnv] = useState<Record<string, string>>({});
  const [leases, setLeases] = useState<RuntimeLeaseView[] | null>(null);
  const [rev, setRev] = useState(0);
  const seq = useRef(0);

  useEffect(() => {
    const cur = ++seq.current;
    setLoadErr(null);
    void Promise.all([api.listRuntimes(), api.listEnvironments(), api.listRuntimeLeases()])
      .then(([r, e, l]) => {
        if (seq.current !== cur) return;
        setRuntimes(r.runtimes);
        setEnvironments(e.environments);
        setLeases(l.leases);
      })
      .catch((e) => {
        if (seq.current !== cur) return;
        setLoadErr(e instanceof Error ? e.message : String(e));
      });
  }, [rev]);

  const act = async (fn: () => Promise<string | void>) => {
    setBusy(true);
    setMsg(null);
    try {
      const note = await fn();
      if (note) setMsg(note);
      setRev((r) => r + 1);
    } catch (e) {
      setMsg(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const envName = (id: string | undefined) =>
    environments.find((e) => e.environment_id === id)?.name ?? (id ? id : "未绑定");

  return (
    <div className="mx-auto flex w-full max-w-3xl flex-col gap-3 p-4">
      <header className="flex flex-wrap items-center gap-2">
        <h1 className="text-base font-semibold">运行环境</h1>
        <span className="text-xs text-faint">
          本地 Environment / Runtime（发现 · 探测 · 准备 · 健康检查；执行随 0.6.5）
        </span>
        <button
          type="button"
          disabled={busy}
          onClick={() =>
            void act(async () => {
              const r = await api.syncRuntimes();
              return `发现同步：新增 ${r.created}，刷新 ${r.refreshed}`;
            })
          }
          className="ml-auto rounded-lg bg-surface-3 px-2.5 py-1 text-xs disabled:opacity-40"
        >
          {busy ? "处理中…" : "发现同步"}
        </button>
      </header>
      {msg && <p className="text-xs text-danger">{msg}</p>}
      {loadErr ? (
        <p className="text-xs text-danger">
          {loadErr}
          <button type="button" onClick={() => setRev((r) => r + 1)} className="ml-2 underline">
            重试
          </button>
        </p>
      ) : runtimes === null ? (
        <p className="text-xs text-faint">加载中…</p>
      ) : runtimes.length === 0 ? (
        <p className="text-xs text-faint">尚无运行时——点「发现同步」从宿主扫描登记可执行程序。</p>
      ) : (
        <ul className="divide-y divide-border rounded-xl border border-border">
          {runtimes.map((rt) => (
            <RuntimeRow
              key={rt.runtime_id}
              rt={rt}
              environments={environments}
              envName={envName(rt.environment_id ?? "")}
              prepareEnvID={prepareEnv[rt.runtime_id] ?? ""}
              onPrepareEnv={(id) => setPrepareEnv((m) => ({ ...m, [rt.runtime_id]: id }))}
              busy={busy}
              act={act}
              refreshKey={rev}
            />
          ))}
        </ul>
      )}

      <section className="mt-2 flex flex-col gap-2 rounded-xl border border-border bg-surface p-3">
        <h2 className="text-sm font-medium">环境（命名工作区目录）</h2>
        <ul className="flex flex-col gap-1 text-xs">
          {environments.map((e) => (
            <li key={e.environment_id} className="flex flex-wrap items-center gap-2 text-faint">
              <span className="font-medium text-text">{e.name}</span>
              {e.is_default && <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-dim">默认</span>}
              <span className="font-mono text-[11px]">{e.workspace_dir}</span>
            </li>
          ))}
          {environments.length === 0 && <li className="text-faint">尚无环境（默认环境由应用启动确保）。</li>}
        </ul>
        <div className="flex flex-wrap items-center gap-2 text-xs">
          <input
            value={envForm.name}
            onChange={(e) => setEnvForm({ ...envForm, name: e.target.value })}
            placeholder="环境名（1–64 字）"
            className="min-w-0 flex-1 rounded border border-border bg-surface-2 px-1.5 py-1 outline-none"
          />
          <input
            value={envForm.dir}
            onChange={(e) => setEnvForm({ ...envForm, dir: e.target.value })}
            placeholder="工作区目录（绝对路径，自动创建）"
            className="min-w-0 flex-1 rounded border border-border bg-surface-2 px-1.5 py-1 font-mono outline-none"
          />
          <button
            type="button"
            disabled={busy || !envForm.name.trim() || !envForm.dir.trim()}
            onClick={() =>
              void act(async () => {
                // 提交快照（审校 P3：等待期间改填的输入不被旧回调清空）
                const snap = { name: envForm.name.trim(), dir: envForm.dir.trim() };
                await api.createEnvironment({ name: snap.name, workspace_dir: snap.dir });
                setEnvForm((cur) =>
                  cur.name.trim() === snap.name && cur.dir.trim() === snap.dir ? { name: "", dir: "" } : cur,
                );
              })
            }
            className="rounded-lg bg-accent px-3 py-1.5 text-accent-contrast disabled:opacity-40"
          >
            建环境
          </button>
        </div>
      </section>

      {/* 租约诊断区（RFC §13：Lease/generation 细节归诊断视图——领取是
          0.6.5 调度行为，不做人工入口；此处只看与释放） */}
      <section className="mt-2 flex flex-col gap-2 rounded-xl border border-border bg-surface p-3">
        <h2 className="text-sm font-medium">租约（诊断）</h2>
        {leases === null ? (
          <p className="text-xs text-faint">加载中…</p>
        ) : leases.length === 0 ? (
          <p className="text-xs text-faint">尚无租约（领取随 0.6.5 调度）。</p>
        ) : (
          <ul className="flex flex-col gap-1 text-xs">
            {leases.map((l) => (
              <li key={l.lease_id} className="flex flex-wrap items-center gap-2 text-faint">
                <span className={leaseClass(l.state)}>{l.state}</span>
                <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-dim">gen {l.generation}</span>
                <span className="font-mono text-[11px]" title={l.lease_id}>
                  {l.runtime_id} · {l.mission_id}
                </span>
                <span className="font-mono text-[10px] text-dim" title={l.fence_token}>
                  {l.workspace_ref}
                </span>
                <span>{l.state === "active" ? `到期 ${absoluteTime(l.expires_at)}` : `领取 ${absoluteTime(l.acquired_at)}`}</span>
                {l.state === "active" && (
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() =>
                      void act(async () => {
                        await api.releaseRuntimeLease(l.lease_id);
                        return "租约已释放（代次自此失效）";
                      })
                    }
                    className="ml-auto shrink-0 rounded bg-surface-3 px-2 py-0.5 text-danger disabled:opacity-40"
                  >
                    释放
                  </button>
                )}
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}

function RuntimeRow({
  rt,
  environments,
  envName,
  prepareEnvID,
  onPrepareEnv,
  busy,
  act,
  refreshKey,
}: {
  rt: RuntimeView;
  environments: EnvironmentView[];
  envName: string;
  prepareEnvID: string;
  onPrepareEnv: (id: string) => void;
  busy: boolean;
  act: (fn: () => Promise<string | void>) => Promise<void>;
  refreshKey: number;
}) {
  // 事件三态（审校 P2#2）：null=加载中 / err=失败（可重试）/ []=成功空列表
  const [events, setEvents] = useState<RuntimeEvent[] | null>(null);
  const [eventsErr, setEventsErr] = useState<string | null>(null);
  const [eventsRetry, setEventsRetry] = useState(0);
  const [eventsOpen, setEventsOpen] = useState(false);
  const evSeq = useRef(0);

  useEffect(() => {
    if (!eventsOpen) return;
    const cur = ++evSeq.current;
    setEventsErr(null);
    void api
      .getRuntimeEvents(rt.runtime_id)
      .then((e) => {
        if (evSeq.current !== cur) return;
        setEvents(e.events);
      })
      .catch((e) => {
        if (evSeq.current !== cur) return;
        setEvents(null);
        setEventsErr(e instanceof Error ? e.message : String(e));
      });
    // refreshKey：成功操作（准备/健康检查）后已展开的历史重取（审校 P2#8）
  }, [eventsOpen, rt.runtime_id, eventsRetry, refreshKey]);

  return (
    <li className="flex flex-col gap-1.5 px-3 py-2.5">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className={stateClass(rt.state)}>{rt.state}</span>
        <span className="font-medium">{rt.adapter}</span>
        <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-dim">{rt.host}{rt.distro ? `:${rt.distro}` : ""}</span>
        {rt.version && <span className="font-mono text-[11px] text-dim">v{rt.version}</span>}
        <span className="text-[11px] text-faint">登录：{rt.login ?? "unknown"}</span>
        <span className="ml-auto shrink-0 text-[11px] text-faint">发现于 {absoluteTime(rt.discovered_at)}</span>
      </div>
      <p className="truncate font-mono text-[11px] text-faint" title={rt.path}>
        {rt.path} · 环境：{envName}
        {rt.last_health_at && ` · 探测 ${absoluteTime(rt.last_health_at)}`}
        {rt.last_health_note && `（${rt.last_health_note}）`}
      </p>
      <div className="flex flex-wrap items-center gap-2 text-xs">
        <select
          value={prepareEnvID}
          onChange={(e) => onPrepareEnv(e.target.value)}
          className="rounded border border-border bg-surface-2 px-1.5 py-1 outline-none"
        >
          <option value="">环境（缺省默认）</option>
          {environments.map((e) => (
            <option key={e.environment_id} value={e.environment_id}>
              {e.name}
              {e.is_default ? "（默认）" : ""}
            </option>
          ))}
        </select>
        <button
          type="button"
          disabled={busy}
          onClick={() =>
            void act(async () => {
              const r = await api.prepareRuntime(rt.runtime_id, prepareEnvID || undefined);
              return `准备：${r.state}${r.state === "unhealthy" ? `（${r.last_health_note}）` : ""}`;
            })
          }
          className="rounded-lg bg-accent px-2.5 py-1 text-accent-contrast disabled:opacity-40"
        >
          准备
        </button>
        <button
          type="button"
          disabled={busy}
          onClick={() =>
            void act(async () => {
              const r = await api.healthCheckRuntime(rt.runtime_id);
              return `探测：${r.state}${r.state === "unhealthy" ? `（${r.last_health_note}）` : ""}`;
            })
          }
          className="rounded-lg bg-surface-3 px-2.5 py-1 disabled:opacity-40"
        >
          健康检查
        </button>
        <button type="button" onClick={() => setEventsOpen((o) => !o)} className="underline text-faint">
          {eventsOpen ? "收起历史" : "健康与状态历史"}
        </button>
      </div>
      {eventsOpen && (
        <ul className="flex flex-col gap-0.5 border-t border-border pt-1 font-mono text-[11px]">
          {eventsErr && (
            <li className="flex items-center gap-2 text-danger">
              历史读取失败：{eventsErr}
              <button type="button" onClick={() => setEventsRetry((n) => n + 1)} className="underline">
                重试
              </button>
            </li>
          )}
          {!eventsErr && events === null && <li className="text-faint">历史加载中…</li>}
          {events !== null && events.length === 0 && <li className="text-faint">（暂无事件）</li>}
          {[...(events ?? [])].reverse().map((e) => (
            <li key={e.seq} className="flex items-center gap-2 text-faint">
              <span className="shrink-0">#{e.seq}</span>
              <span className="shrink-0 text-dim">{e.event_type}</span>
              <span className="truncate">{JSON.stringify(e.payload)}</span>
              <span className="ml-auto shrink-0">{absoluteTime(e.occurred_at)}</span>
            </li>
          ))}
        </ul>
      )}
    </li>
  );
}
