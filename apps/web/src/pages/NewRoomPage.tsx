// /new 空态引导页：选择入房 Agent（拉人进群语义——dogfood 反馈 #1）后建房跳转。
// 空选择 = 全部在席（向后兼容）；勾选 = 恰好所选，后续可在房间内 invite_agent 拉人。
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api, type AgentSeatInfo } from "../api/client";
import { adapterLabel } from "../lib/copy";
import { createRoom } from "../state/rooms";

export function NewRoomPage() {
  const navigate = useNavigate();
  const [agents, setAgents] = useState<AgentSeatInfo[] | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .agents()
      .then(({ agents }) => setAgents(agents))
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, []);

  const toggle = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  const start = async () => {
    if (creating) return;
    setCreating(true);
    setError(null);
    try {
      const roomID = await createRoom("新房间", [...selected]);
      navigate(`/rooms/${roomID}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setCreating(false);
    }
  };

  return (
    <div className="flex h-full flex-col items-center justify-center gap-4 px-6">
      <span className="inline-block h-12 w-12 rounded-2xl bg-accent" aria-hidden />
      <h1 className="text-xl font-semibold tracking-tight">Mosaic 讨论室</h1>
      <p className="max-w-md text-center text-sm text-dim">
        选择要拉进房间的 Agent（不选 = 全部在席）；它们会自主评估发言权，你也可以随时 @ 点名。
      </p>

      <div className="flex max-w-lg flex-wrap justify-center gap-2" role="group" aria-label="选择入房 Agent">
        {agents === null ? (
          <span className="text-sm text-dim">座位加载中…</span>
        ) : (
          agents.map((a) => {
            const on = selected.has(a.participant_id);
            return (
              <button
                key={a.participant_id}
                type="button"
                aria-pressed={on}
                onClick={() => toggle(a.participant_id)}
                title={`${a.display_name}（${adapterLabel(a.adapter)}）`}
                className={
                  "rounded-full border px-4 py-1.5 text-sm transition-colors " +
                  (on
                    ? "border-accent bg-accent text-accent-contrast"
                    : "border-line bg-panel text-fg hover:border-accent")
                }
              >
                {a.display_name}
                <span className="ml-1.5 text-xs opacity-60">{adapterLabel(a.adapter)}</span>
              </button>
            );
          })
        )}
      </div>

      <button
        type="button"
        onClick={() => void start()}
        disabled={creating}
        className="mt-2 rounded-xl bg-accent px-6 py-3 text-base font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-50"
      >
        {creating ? "创建中…" : selected.size === 0 ? "开始新讨论（全部 Agent）" : `开始新讨论（${selected.size} 位 Agent）`}
      </button>
      {error && <p className="text-sm text-danger">{error}</p>}
    </div>
  );
}
