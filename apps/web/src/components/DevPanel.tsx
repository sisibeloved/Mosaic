// 开发者面板（M1 v1.8 机制延续）：全局展示开关在设置页（state/dev，本地持久化）；
// 面板本体挂在房间抽屉"调试"Tab（调试数据是房间局部信息）。
// trace 展示、直读 /v1/debug 只读端点、预算水位（5s 轮询）、波链路档案
// （M3-1 持久化：自事件流重建，重启后历史波完整可复盘——[dev] 内联时间线是
// 实时 SSE 瞬态，本区是事实源视图）。端点仅当服务端 -dev 时装配——404 时给出
// 明确提示而非无限"读取中"。
import { useEffect, useState } from "react";
import { api, ApiError, lastTrace } from "../api/client";

interface DebugBudget {
  rounds?: number;
  utterances?: number;
  tokens?: number;
  level?: number;
  remaining_tokens?: number;
  limits?: { max_rounds?: number; max_utterances?: number; max_tokens?: number };
}

interface WaveIntent {
  event_id: string;
  participant_id: string;
  action: string;
  type?: string;
  score_band: string;
  public_rationale?: string;
  unselected_reason?: string;
  selected: boolean;
  endorsed: boolean;
}

interface WaveGrant {
  grant_id: string;
  participant_id: string;
  rank: number;
  published: boolean;
  revoked: boolean;
  revoke_reason?: string;
}

interface WaveTiming {
  total_ms: number;
  history_ms: number;
  assemble_ms: number;
  eval_ms: Record<string, number>;
  eval_total_ms: number;
  generate_ms: Record<string, number>;
}

interface Wave {
  round_id: string;
  stimulus_event_id: string;
  opened_seq: number;
  closed_seq?: number;
  outcome?: string;
  published: number;
  silent_count: number;
  window_ms?: number;
  timing?: WaveTiming;
  intents: WaveIntent[];
  grants: WaveGrant[];
}

interface WavesPage {
  waves: Wave[];
  next?: string;
}

const BUDGET_LEVELS = ["正常", "70% 降发言", "90% 降座", "100% 硬停"];

const OUTCOME_LABEL: Record<string, string> = {
  published: "有发言",
  quiescent: "意愿静默",
  revoked_all: "全部撤销",
};

const ACTION_LABEL: Record<string, string> = {
  speak: "发言",
  react: "回应",
  fork: "分叉",
  summarize: "总结",
  silent: "不发言",
};

const sec = (ms?: number) => (ms === undefined ? "—" : `${(ms / 1000).toFixed(1)}s`);

function waveOutcomeBadge(w: Wave): { text: string; cls: string } {
  if (!w.outcome) return { text: "未收波", cls: "text-warn" };
  const label = OUTCOME_LABEL[w.outcome] ?? w.outcome;
  return { text: label, cls: w.outcome === "published" ? "text-ok" : "text-dim" };
}

/** 近波均值（仅计带 timing 的波）——性能观测速览。 */
function avgTotalMs(waves: Wave[]): number {
  const t = waves.filter((w) => w.timing);
  return t.length ? t.reduce((s, w) => s + (w.timing?.total_ms ?? 0), 0) / t.length : 0;
}

function avgEvalMs(waves: Wave[]): number {
  const t = waves.filter((w) => w.timing);
  return t.length ? t.reduce((s, w) => s + (w.timing?.eval_total_ms ?? 0), 0) / t.length : 0;
}

export function DevPanel({
  roomID,
  nameOf,
  describeEvent,
}: {
  roomID: string | null;
  /** participant_id → 展示名（成员表解析，回退原 id）。 */
  nameOf: (participantID: string) => string;
  /** event_id → 可读引用（波锚点摘要），解析不到回退短 id。 */
  describeEvent: (eventID: string) => string | null;
}) {
  const [out, setOut] = useState<string>("");
  const [budget, setBudget] = useState<DebugBudget | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [waves, setWaves] = useState<Wave[]>([]);
  const [wavesNext, setWavesNext] = useState<string | null>(null);
  const [wavesErr, setWavesErr] = useState<string | null>(null);

  useEffect(() => {
    if (!roomID) {
      setBudget(null);
      setUnavailable(false);
      return;
    }
    let alive = true;
    const load = async () => {
      try {
        const st = (await api.debugState(roomID)) as { budget?: DebugBudget };
        if (alive) {
          setBudget(st.budget ?? null);
          setUnavailable(false);
        }
      } catch (e) {
        if (!alive) return;
        setBudget(null);
        setUnavailable(e instanceof ApiError && e.status === 404);
      }
    };
    void load();
    const timer = setInterval(() => void load(), 5000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [roomID]);

  // 波链路档案：进房加载最新一页；失败不重试（手动刷新兜底）。
  useEffect(() => {
    setWaves([]);
    setWavesNext(null);
    setWavesErr(null);
    if (!roomID) return;
    let alive = true;
    void (async () => {
      try {
        const page = (await api.debugWaves(roomID)) as WavesPage;
        if (!alive) return;
        setWaves(page.waves ?? []);
        setWavesNext(page.next || null);
      } catch (e) {
        if (alive) setWavesErr(String(e));
      }
    })();
    return () => {
      alive = false;
    };
  }, [roomID]);

  const loadOlderWaves = async () => {
    if (!roomID || !wavesNext) return;
    try {
      const page = (await api.debugWaves(roomID, wavesNext)) as WavesPage;
      setWaves((prev) => [...(page.waves ?? []), ...prev]); // 页间更老在前
      setWavesNext(page.next || null);
    } catch (e) {
      setWavesErr(String(e));
    }
  };

  const refreshWaves = async () => {
    if (!roomID) return;
    try {
      const page = (await api.debugWaves(roomID)) as WavesPage;
      setWaves(page.waves ?? []);
      setWavesNext(page.next || null);
      setWavesErr(null);
    } catch (e) {
      setWavesErr(String(e));
    }
  };

  const load = async (fn: () => Promise<unknown>) => {
    try {
      setOut(JSON.stringify(await fn(), null, 2));
    } catch (e) {
      setOut(String(e));
    }
  };

  return (
    <div className="flex flex-col gap-3">
      <p className="text-xs text-faint">trace: {lastTrace || "—"}</p>
      {budget ? (
        <table className="w-full border-collapse text-xs">
          <tbody>
            {[
              ["轮次", `${budget.rounds ?? "—"} / ${budget.limits?.max_rounds ?? "—"}`],
              ["发言", `${budget.utterances ?? "—"} / ${budget.limits?.max_utterances ?? "—"}`],
              [
                "Token",
                `${budget.tokens ?? "—"} / ${budget.limits?.max_tokens ?? "—"}` +
                  (budget.remaining_tokens !== undefined
                    ? `（剩余 ${budget.remaining_tokens.toLocaleString()}）`
                    : ""),
              ],
              ["梯度", BUDGET_LEVELS[budget.level ?? 0] ?? String(budget.level)],
            ].map(([k, v]) => (
              <tr key={k} className="border-b border-border last:border-0">
                <th className="py-1.5 pr-3 text-left font-normal text-dim">{k}</th>
                <td className="py-1.5">{v}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        <p className="text-xs text-faint">
          {unavailable
            ? "调试端点未装配——服务端需以 -dev 启动（桌面端默认启用）。"
            : roomID
              ? "预算水位读取中…"
              : "进入一个房间后此处显示实时预算水位。"}
        </p>
      )}

      {/* 波链路档案（M3-1 持久化）：事件流重建，重启不丢；实时增量走聊天内 [dev] 内联时间线 */}
      <div className="flex items-center justify-between">
        <h4 className="text-xs font-medium text-dim">
          波链路（重启可复盘）
          {waves.some((w) => w.timing) && (
            <span className="ml-1 text-faint">
              · 均全程 {sec(avgTotalMs(waves))} / 评估Σ {sec(avgEvalMs(waves))}
            </span>
          )}
        </h4>
        <button
          type="button"
          disabled={!roomID}
          onClick={() => void refreshWaves()}
          className="rounded-lg bg-surface-3 px-2.5 py-1 text-xs text-text transition-opacity hover:opacity-85 disabled:opacity-40"
        >
          刷新
        </button>
      </div>
      {wavesErr && <p className="text-xs text-warn">波链路读取失败：{wavesErr}</p>}
      {!roomID ? (
        <p className="text-xs text-faint">进入一个房间后此处显示历史波链路。</p>
      ) : waves.length === 0 && !wavesErr ? (
        <p className="text-xs text-faint">尚无反应波（任何成员消息后的观察→判断→回复周期）。</p>
      ) : (
        <div className="flex flex-col gap-2">
          {[...waves].reverse().map((w) => {
            const badge = waveOutcomeBadge(w);
            return (
              <div key={w.round_id} className="rounded-lg bg-surface p-2.5 text-[11px] leading-5">
                <div className="flex items-center justify-between gap-2">
                  <span className="text-dim">
                    波 {w.round_id.slice(-8)} · seq {w.opened_seq}
                    {w.closed_seq ? `→${w.closed_seq}` : "→…"}
                  </span>
                  <span className={badge.cls}>{badge.text}</span>
                </div>
                <p className="text-faint">
                  锚点：{describeEvent(w.stimulus_event_id) ?? w.stimulus_event_id.slice(-10)}
                </p>
                {/* 性能定位套件 v1：分段耗时（窗口/评估逐座/生成/全程） */}
                <p className="text-faint">
                  耗时 窗口 {sec(w.window_ms)}
                  {w.timing
                    ? ` ｜ 评估Σ ${sec(w.timing.eval_total_ms)}（${Object.entries(w.timing.eval_ms)
                        .map(([pid, ms]) => `${nameOf(pid).slice(0, 8)} ${sec(ms)}`)
                        .join(" / ")}）` +
                      `｜ 生成 ${Object.entries(w.timing.generate_ms)
                        .map(([pid, ms]) => `${nameOf(pid).slice(0, 8)} ${sec(ms)}`)
                        .join(" / ") || "—"} ｜ 全程 ${sec(w.timing.total_ms)}`
                    : " ｜（无计时：旧版本波）"}
                </p>
                {w.intents.map((it) => (
                  <p key={it.event_id} className="text-dim">
                    意图 {nameOf(it.participant_id)}：{ACTION_LABEL[it.action] ?? it.action}
                    {it.type ? ` · ${it.type}` : ""}
                    {it.score_band ? ` · ${it.score_band}` : ""}
                    {it.selected ? " · 放行" : ""}
                    {it.endorsed ? " · 保送" : ""}
                    {it.public_rationale ? `（${it.public_rationale}）` : ""}
                    {!it.selected && it.unselected_reason ? `〔未选：${it.unselected_reason}〕` : ""}
                  </p>
                ))}
                {w.grants.map((g) => (
                  <p key={g.grant_id} className="text-dim">
                    发授 {nameOf(g.participant_id)} · rank {g.rank} →{" "}
                    {g.published ? "已发布" : g.revoked ? `已撤销（${g.revoke_reason ?? "?"}）` : "未结"}
                  </p>
                ))}
              </div>
            );
          })}
          {wavesNext && (
            <button
              type="button"
              onClick={() => void loadOlderWaves()}
              className="self-start rounded-lg bg-surface-3 px-2.5 py-1 text-xs text-text transition-opacity hover:opacity-85"
            >
              加载更早
            </button>
          )}
        </div>
      )}

      <div className="flex gap-2">
        <button
          type="button"
          disabled={!roomID}
          onClick={() => void load(() => api.debugState(roomID!))}
          className="rounded-lg bg-surface-3 px-2.5 py-1 text-xs text-text transition-opacity hover:opacity-85 disabled:opacity-40"
        >
          房间状态
        </button>
        <button
          type="button"
          disabled={!roomID}
          onClick={() => void load(() => api.debugEvents(roomID!))}
          className="rounded-lg bg-surface-3 px-2.5 py-1 text-xs text-text transition-opacity hover:opacity-85 disabled:opacity-40"
        >
          事件日志
        </button>
      </div>
      {out && (
        <pre className="max-h-72 overflow-auto rounded-lg bg-surface p-3 text-[11px] leading-5 text-dim">
          {out}
        </pre>
      )}
    </div>
  );
}
