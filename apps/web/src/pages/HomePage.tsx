// 工作台首页（v1.72 裁定：房间是长期资产——/ 是"回到工作的地方"，建房降级为
// 次级入口）：最近会话（私聊/群聊分型展示，点击进入）+「开始新讨论」次级按钮；
// 零房间时退化为建房引导页（NewRoomPage——首次使用的 onboarding 不缺位）。
import { useEffect } from "react";
import { useNavigate } from "react-router-dom";
import { Avatar } from "../components/chat/Avatar";
import { relativeTime } from "../lib/ui";
import { refreshRooms, useRooms } from "../state/rooms";
import { NewRoomPage } from "./NewRoomPage";

const RECENT_CAP = 8;

export function HomePage() {
  const { rooms } = useRooms();
  const navigate = useNavigate();

  useEffect(() => {
    void refreshRooms();
  }, []);

  if (rooms === null) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-faint">加载中…</div>
    );
  }
  if (rooms.length === 0) {
    return <NewRoomPage />;
  }

  const recent = rooms.slice(0, RECENT_CAP); // 列表本身按 last_event_at 倒序
  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto flex max-w-2xl flex-col gap-5 px-6 py-10">
        <div className="flex items-end justify-between gap-3">
          <div>
            <h1 className="text-xl font-semibold tracking-tight">继续工作</h1>
            <p className="mt-1 text-xs text-faint">最近活跃的讨论；记忆与任务都在房间里沉淀。</p>
          </div>
          <button
            type="button"
            onClick={() => navigate("/new")}
            className="shrink-0 rounded-xl border border-border bg-surface-2 px-3.5 py-2 text-xs text-dim transition-colors hover:border-faint hover:text-text"
          >
            + 开始新讨论
          </button>
        </div>
        <ul className="flex flex-col gap-1.5">
          {recent.map((r) => {
            const dm = (r.agents?.length ?? 0) === 1;
            return (
              <li key={r.room_id}>
                <button
                  type="button"
                  onClick={() => navigate(`/rooms/${r.room_id}`)}
                  className="flex w-full items-center gap-3 rounded-xl border border-border bg-surface px-3.5 py-3 text-left transition-colors hover:border-faint"
                >
                  {dm ? (
                    <Avatar participantID={r.agents![0]} displayName={r.display_name} size={30} />
                  ) : (
                    <span className="flex h-[30px] w-[30px] shrink-0 items-center justify-center rounded bg-surface-3 text-dim">
                      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
                        <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
                        <circle cx="9" cy="7" r="4" />
                        <path d="M23 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75" />
                      </svg>
                    </span>
                  )}
                  <span className="min-w-0 flex-1">
                    <span className="flex items-center gap-2 text-sm font-medium text-text">
                      <span className="truncate">{r.display_name}</span>
                      <span className="shrink-0 rounded bg-surface-3 px-1.5 text-[10px] leading-4 text-dim">
                        {dm ? "私聊" : "群聊"}
                      </span>
                      {r.paused && (
                        <span className="shrink-0 text-[10px] text-warn">已暂停</span>
                      )}
                    </span>
                    <span className="mt-0.5 block text-xs text-faint">
                      {relativeTime(r.last_event_at)} · {r.message_count} 条消息
                      {!dm && r.agents ? ` · ${r.agents.length} 位 Agent` : ""}
                    </span>
                  </span>
                </button>
              </li>
            );
          })}
        </ul>
        {rooms.length > RECENT_CAP && (
          <p className="text-xs text-faint">更早的房间在左侧列表。</p>
        )}
      </div>
    </div>
  );
}
