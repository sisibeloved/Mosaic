// 开发者区（M1 v1.8 机制延续）：MOSAIC_DEV 由服务端 -dev 注入；trace 展示、
// 直读 /v1/debug 只读端点、预算水位（5s 轮询，仅开发者模式装配 debug 端点）。
import { useEffect, useState } from "react";
import { api, lastTrace } from "../api/client";

declare const MOSAIC_DEV: boolean;

interface DebugBudget {
  rounds?: number;
  utterances?: number;
  tokens?: number;
  level?: number;
  remaining_tokens?: number;
  limits?: { max_rounds?: number; max_utterances?: number; max_tokens?: number };
}

const BUDGET_LEVELS = ["正常", "70% 降发言", "90% 降座", "100% 硬停"];

export function DevPanel({ roomID }: { roomID: string | null }) {
  const [out, setOut] = useState<string>("");
  const [budget, setBudget] = useState<DebugBudget | null>(null);

  useEffect(() => {
    if (!MOSAIC_DEV || !roomID) {
      setBudget(null);
      return;
    }
    let alive = true;
    const load = async () => {
      try {
        const st = (await api.debugState(roomID)) as { budget?: DebugBudget };
        if (alive) setBudget(st.budget ?? null);
      } catch {
        if (alive) setBudget(null);
      }
    };
    void load();
    const timer = setInterval(() => void load(), 5000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [roomID]);

  if (!MOSAIC_DEV) return null;

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
          {roomID ? "预算水位读取中…" : "进入一个房间后此处显示实时预算水位。"}
        </p>
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
