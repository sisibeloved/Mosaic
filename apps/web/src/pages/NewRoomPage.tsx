// 首页（/ 与 /new）：引导建房页——选择入房 Agent（拉人进群语义——dogfood 反馈 #1）后建房跳转。
// 不选 = 拉入当前全部在席 Agent（建房时点快照——之后新启用的 Agent 不自动入房，
// 走房间内邀请）；未启用的已发现项以灰芯片如实展示，指路设置。
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api, type AgentSeatInfo, type DisabledAgentInfo } from "../api/client";
import { AppLogo } from "../components/AppLogo";
import { adapterLabel, channelLabel } from "../lib/copy";
import { createRoom } from "../state/rooms";

export function NewRoomPage() {
  const navigate = useNavigate();
  const [agents, setAgents] = useState<AgentSeatInfo[] | null>(null);
  const [disabled, setDisabled] = useState<DisabledAgentInfo[]>([]);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .agents()
      .then(({ agents, disabled }) => {
        setAgents(agents);
        setDisabled(disabled ?? []);
      })
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

  // 座位未就绪（宿主扫描期）：禁建——否则会物化出空名单的退化房间。
  const seatsReady = agents !== null && agents.length > 0;

  return (
    <div className="flex h-full flex-col items-center justify-center gap-4 px-6">
      <AppLogo size={48} />
      <h1 className="text-xl font-semibold tracking-tight">Mosaic 讨论室</h1>
      <p className="max-w-md text-center text-sm text-dim">
        选择要拉进房间的 Agent（不选 = 当前全部在席；入房名单在创建时确定，之后新启用的
        Agent 经房间内邀请加入）；它们会自主评估发言权，你也可以随时 @ 点名。
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
        {disabled.map((d) => (
          <span
            key={`${d.adapter}:${d.channel}`}
            title={`未启用——设置 → Agent 实例 里开启后自动入座`}
            className="cursor-not-allowed rounded-full border border-line bg-panel px-4 py-1.5 text-sm text-faint"
          >
            {adapterLabel(d.adapter)}
            <span className="ml-1.5 text-xs opacity-60">{channelLabel(d.channel ?? "")} · 未启用</span>
          </span>
        ))}
      </div>
      {disabled.length > 0 && (
        <p className="max-w-md text-center text-xs text-faint">
          灰色项已发现但未启用：在 设置 → Agent 实例 开启后 ≤10 秒自动入座，届时可在此勾选或房间内邀请。
        </p>
      )}

      <button
        type="button"
        onClick={() => void start()}
        disabled={creating || !seatsReady}
        className="mt-2 rounded-xl bg-accent px-6 py-3 text-base font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-50"
      >
        {creating
          ? "创建中…"
          : !seatsReady
            ? "座位就绪中…"
            : selected.size === 0
              ? `开始新讨论（全部 ${agents!.length} 位 Agent）`
              : `开始新讨论（${selected.size} 位 Agent）`}
      </button>
      {error && <p className="text-sm text-danger">{error}</p>}
    </div>
  );
}
