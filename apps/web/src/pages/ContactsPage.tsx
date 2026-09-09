// 联系人页（M4-4 固定会话入口）：在席 agent = 联系人；"打开会话"回到该身份的
// 长期私聊（DM）——按名册事实 find-or-create（roster 恰为该 bot 的房间），
// 不以可编辑房间名承担身份。删除会话后再进入 = 新房间新事件流（旧内容与
// 记忆不复活——房间记忆 room-scoped）；群聊与私聊各持各的 CLI 线程
//（ADR-0013 会话按 (profile, room) 独立映射，服务端语义）。
import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api, ApiError, type AgentSeatInfo } from "../api/client";
import { refreshRooms } from "../state/rooms";
import { adapterLabel } from "../lib/copy";
import { Avatar } from "../components/chat/Avatar";

type DMTarget = { roomID: string; lastEventAt: string } | null;

/** find-or-create：名册恰为 [bot] 的房间里取最近活跃者；没有则建房。 */
async function findOrCreateDM(bot: AgentSeatInfo): Promise<string> {
  const { rooms } = await api.listRooms();
  const snaps = await Promise.all(
    rooms.map((r) => api.snapshot(r.room_id).catch(() => null)),
  );
  let best: DMTarget = null;
  rooms.forEach((r, i) => {
    const snap = snaps[i];
    const roster = snap?.roster;
    if (!roster || roster.length !== 1 || roster[0] !== bot.participant_id) return;
    if (!best || r.last_event_at > best.lastEventAt) {
      best = { roomID: r.room_id, lastEventAt: r.last_event_at };
    }
  });
  if (best) return (best as NonNullable<DMTarget>).roomID;
  const name = bot.display_name || bot.participant_id;
  const created = await api.createRoom(name, [bot.participant_id]);
  await refreshRooms();
  return created.room_id;
}

export function ContactsPage() {
  const navigate = useNavigate();
  const [agents, setAgents] = useState<AgentSeatInfo[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [opening, setOpening] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    void api
      .agents()
      .then(({ agents: seats }) => {
        if (alive) setAgents(seats.filter((a) => a.participant_id !== "par_echo"));
      })
      .catch((e) => {
        if (alive) setError(e instanceof Error ? e.message : String(e));
      });
    return () => {
      alive = false;
    };
  }, []);

  const openDM = useCallback(
    (bot: AgentSeatInfo) => {
      setOpening(bot.participant_id);
      setError(null);
      void findOrCreateDM(bot)
        .then((roomID) => navigate(`/rooms/${roomID}`))
        .catch((e) => {
          setError(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
          setOpening(null);
        });
    },
    [navigate],
  );

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto flex max-w-2xl flex-col gap-6 px-6 py-8">
        <div>
          <h1 className="text-lg font-semibold tracking-tight">联系人</h1>
          <p className="mt-1 text-xs text-faint">
            每个在席 Agent 是一个稳定联系人（身份不随 CLI 升级/换路径变化）。"打开会话"
            总是回到与它的长期私聊；私聊与群聊的上下文互相独立。
          </p>
        </div>
        {error && <p className="text-xs text-danger">{error}</p>}
        {agents === null ? (
          <p className="text-xs text-faint">加载中…</p>
        ) : agents.length === 0 ? (
          <p className="text-xs text-faint">
            还没有在席 Agent——到 <Link className="text-accent" to="/settings">设置</Link> 启用已安装的 CLI（Codex / Kimi / MiniMax）。
          </p>
        ) : (
          <ul className="divide-y divide-border rounded-xl border border-border">
            {agents.map((a) => (
              <li key={a.participant_id} className="flex items-center gap-3 px-3 py-2.5">
                <Avatar participantID={a.participant_id} displayName={a.display_name} size={30} />
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium">{a.display_name || a.participant_id}</p>
                  <p className="truncate text-xs text-faint">
                    {adapterLabel(a.adapter)} · <span className="font-mono">{a.participant_id}</span>
                  </p>
                </div>
                <button
                  type="button"
                  disabled={opening === a.participant_id}
                  onClick={() => openDM(a)}
                  className="shrink-0 rounded-lg bg-accent-soft px-3 py-1.5 text-xs text-accent transition-opacity hover:opacity-85 disabled:opacity-40"
                >
                  {opening === a.participant_id ? "打开中…" : "打开会话"}
                </button>
              </li>
            ))}
          </ul>
        )}
        <p className="text-[11px] leading-4 text-faint">
          私聊房间与普通群聊同一套协议（反应波/任务/记忆按房间独立）；任务执行通道的
          指派与结果归属按稳定身份记录，CLI 换安装位置不中断。
        </p>
      </div>
    </div>
  );
}
