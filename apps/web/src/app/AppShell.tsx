// App 壳（v1.74 联系人收敛——裁定"私聊组即联系人"）：
// 侧栏 = 品牌（+ 新群聊）/ 会话列表（私聊·群聊）——私聊组直接列出全部在席
// Agent：有会话的显示最近活跃（点击回到长期私聊），无会话的显示弱化的"发起
// 私聊"（点击 find-or-create）；未启用项组尾灰芯片指路设置。联系人页退役
// （/contacts 移除）——"人"与"私聊"本就一一对应，一个列表零冗余。
// 群聊组 = 多员房间与旧版全席房；建房从大按钮降级为品牌行 + 号（长期资产回访 >>
// 建房频率）。底部 = 设置 + 主题（个人中心已并入设置）。
import { useEffect, useState } from "react";
import { Link, NavLink, Outlet, useNavigate } from "react-router-dom";
import { api, type AgentSeatInfo, type RoomSummary } from "../api/client";
import { AppLogo } from "../components/AppLogo";
import { Avatar } from "../components/chat/Avatar";
import { useContextMenu, type ContextMenuItem } from "../components/ContextMenu";
import { copyText } from "../lib/clipboard";
import { relativeTime } from "../lib/ui";
import { execRoomCommand, findOrCreateDM, refreshAgentSeats, refreshRooms, useRooms } from "../state/rooms";
import { toggleTheme, useTheme } from "../state/theme";

/** 群聊条目：群聊图标 + 名称 + 活跃摘要。 */
function RoomEntry({
  room,
  onContextMenu,
}: {
  room: RoomSummary;
  onContextMenu: (e: React.MouseEvent, room: RoomSummary) => void;
}) {
  return (
    <NavLink
      to={`/rooms/${room.room_id}`}
      onContextMenu={(e) => onContextMenu(e, room)}
      className={({ isActive }) =>
        `block rounded-lg px-2.5 py-2 transition-colors ${isActive ? "bg-surface-3" : "hover:bg-surface-2"}`
      }
    >
      <span className="flex items-center gap-2">
        <span className="flex h-5 w-5 shrink-0 items-center justify-center rounded bg-surface-3 text-faint">
          <IconGroup />
        </span>
        <span className="min-w-0 flex-1 truncate text-sm text-text">{room.display_name}</span>
        {room.paused && (
          <span className="shrink-0 text-[10px] text-warn" title="已暂停">
            ⏸
          </span>
        )}
      </span>
      <span className="mt-0.5 block pl-7 text-xs text-faint">
        {relativeTime(room.last_event_at)} · {room.message_count} 条
      </span>
    </NavLink>
  );
}

/** 私聊条目（v1.74 = 联系人）：Agent 头像锚定身份；有会话显活跃摘要，无会话
 * 弱化为发起入口（find-or-create——删除后重建即新会话，身份不复活旧记忆）。 */
function SeatEntry({
  agent,
  dm,
  opening,
  onOpen,
  onContextMenu,
}: {
  agent: AgentSeatInfo;
  dm: RoomSummary | null;
  opening: boolean;
  onOpen: (agent: AgentSeatInfo) => void;
  onContextMenu: (e: React.MouseEvent, agent: AgentSeatInfo, dm: RoomSummary | null) => void;
}) {
  const active = dm ? window.location.pathname === `/rooms/${dm.room_id}` : false;
  return (
    <button
      type="button"
      onClick={() => onOpen(agent)}
      onContextMenu={(e) => onContextMenu(e, agent, dm)}
      title={dm ? `回到与 ${agent.display_name} 的私聊` : `与 ${agent.display_name} 发起私聊`}
      className={`block w-full rounded-lg px-2.5 py-2 text-left transition-colors ${
        active ? "bg-surface-3" : "hover:bg-surface-2"
      }`}
    >
      <span className="flex items-center gap-2">
        <Avatar participantID={agent.participant_id} displayName={agent.display_name} size={20} />
        <span className="min-w-0 flex-1 truncate text-sm text-text">{agent.display_name}</span>
        {dm?.paused && (
          <span className="shrink-0 text-[10px] text-warn" title="已暂停">
            ⏸
          </span>
        )}
      </span>
      <span className="mt-0.5 block pl-7 text-xs text-faint">
        {opening
          ? "打开中…"
          : dm
            ? `${relativeTime(dm.last_event_at)} · ${dm.message_count} 条`
            : "发起私聊…"}
      </span>
    </button>
  );
}

export function AppShell() {
  const { rooms, error, seats, disabledSeats } = useRooms();
  const navigate = useNavigate();
  const [theme, setTheme] = useTheme();
  const menu = useContextMenu();
  // 侧栏右键操作弹层：重命名 / 删除（危险，理由必填留痕——与房间页 ⋯ 菜单同契约）。
  const [renaming, setRenaming] = useState<{ roomID: string; name: string } | null>(null);
  const [renameBusy, setRenameBusy] = useState(false);
  const [renameError, setRenameError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<{ roomID: string; name: string } | null>(null);
  const [deleteReason, setDeleteReason] = useState("");
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [openingPID, setOpeningPID] = useState<string | null>(null);

  useEffect(() => {
    void refreshRooms();
    void refreshAgentSeats();
  }, []);

  // 私聊判定：roster 恰为单员（与 findOrCreateDM 同语义）；组序 = 有会话按最近
  // 活跃降序，无会话按显示名序排后（人是入口，会话是状态）。
  const dmByPid = new Map<string, RoomSummary>();
  for (const r of rooms ?? []) {
    if ((r.agents?.length ?? 0) === 1) dmByPid.set(r.agents![0], r);
  }
  const seatRows = (seats ?? []).map((agent) => ({ agent, dm: dmByPid.get(agent.participant_id) ?? null }));
  seatRows.sort((a, b) => {
    if (a.dm && b.dm) return a.dm.last_event_at < b.dm.last_event_at ? 1 : -1;
    if (a.dm) return -1;
    if (b.dm) return 1;
    return a.agent.display_name.localeCompare(b.agent.display_name);
  });
  const groups = (rooms ?? []).filter((r) => (r.agents?.length ?? 0) !== 1);

  const openDM = (agent: AgentSeatInfo) => {
    if (openingPID) return;
    setOpeningPID(agent.participant_id);
    void findOrCreateDM(agent)
      .then((roomID) => navigate(`/rooms/${roomID}`))
      .catch(() => {})
      .finally(() => setOpeningPID(null));
  };

  const roomMenuItems = (room: RoomSummary): ContextMenuItem[] => [
    { label: "打开房间", onSelect: () => navigate(`/rooms/${room.room_id}`) },
    {
      label: "重命名…",
      onSelect: () => {
        setRenameError(null);
        setRenaming({ roomID: room.room_id, name: room.display_name });
      },
    },
    {
      label: room.paused ? "恢复讨论" : "暂停讨论",
      onSelect: () =>
        void execRoomCommand(room.room_id, (v) =>
          room.paused ? api.resumeRoom(room.room_id, v) : api.pauseRoom(room.room_id, v, "侧栏操作"),
        ).catch(() => {}),
    },
    { kind: "separator", label: "" },
    { label: "复制房间 ID", hint: room.room_id.slice(-8), onSelect: () => void copyText(room.room_id) },
    {
      label: "删除房间…",
      danger: true,
      onSelect: () => {
        setDeleteError(null);
        setDeleteReason("");
        setDeleting({ roomID: room.room_id, name: room.display_name });
      },
    },
  ];

  const openRoomMenu = (e: React.MouseEvent, room: RoomSummary) => {
    menu.open(e, roomMenuItems(room));
  };

  /** 私聊行右键：身份项（参与者 ID）+ 会话项（有会话时）。 */
  const openSeatMenu = (e: React.MouseEvent, agent: AgentSeatInfo, dm: RoomSummary | null) => {
    const items: ContextMenuItem[] = [
      {
        label: dm ? "复制参与者 ID" : `发起私聊（${agent.display_name}）`,
        hint: dm ? agent.participant_id.slice(-8) : undefined,
        onSelect: () => (dm ? void copyText(agent.participant_id) : openDM(agent)),
      },
    ];
    if (dm) {
      items.push({ kind: "separator", label: "" }, ...roomMenuItems(dm));
    }
    menu.open(e, items);
  };

  const submitRename = async () => {
    if (!renaming || renameBusy) return;
    const name = renaming.name.trim();
    if (!name) return;
    setRenameBusy(true);
    setRenameError(null);
    try {
      await execRoomCommand(renaming.roomID, (v) => api.renameRoom(renaming.roomID, v, name));
      setRenaming(null);
    } catch (e) {
      setRenameError(e instanceof Error ? e.message : String(e));
    } finally {
      setRenameBusy(false);
    }
  };

  const submitDelete = async () => {
    if (!deleting || deleteBusy) return;
    const reason = deleteReason.trim();
    if (!reason) return;
    setDeleteBusy(true);
    setDeleteError(null);
    try {
      await execRoomCommand(deleting.roomID, (v) => api.deleteRoom(deleting.roomID, v, reason));
      setDeleting(null);
      // 删除的是当前房间时回到工作台（旧路由若仍停留会拿到 room_not_found）
      if (window.location.pathname.includes(deleting.roomID)) navigate("/");
    } catch (e) {
      setDeleteError(e instanceof Error ? e.message : String(e));
    } finally {
      setDeleteBusy(false);
    }
  };

  return (
    <div className="flex h-full">
      <aside className="flex w-64 shrink-0 flex-col border-r border-border bg-surface">
        <div className="flex items-center gap-2 px-4 pb-1 pt-4">
          <Link to="/" className="flex min-w-0 flex-1 items-center gap-2.5" title="回工作台">
            <AppLogo size={26} />
            <span className="text-base font-semibold tracking-wide">Mosaic</span>
          </Link>
          <button
            type="button"
            onClick={() => navigate("/new")}
            title="开始新讨论（建群聊房间）"
            aria-label="开始新讨论"
            className="rounded-lg p-1.5 text-dim transition-colors hover:bg-surface-2 hover:text-text"
          >
            <IconPlus />
          </button>
        </div>
        <nav className="flex-1 overflow-y-auto px-2 py-1" aria-label="会话列表">
          {rooms === null || seats === null ? (
            <p className="px-2 py-3 text-xs text-faint">加载中…</p>
          ) : (
            <>
              <section aria-label="私聊">
                <h3 className="px-2.5 pb-1 pt-2 text-[11px] font-medium text-faint">私聊</h3>
                {seatRows.map(({ agent, dm }) => (
                  <SeatEntry
                    key={agent.participant_id}
                    agent={agent}
                    dm={dm}
                    opening={openingPID === agent.participant_id}
                    onOpen={openDM}
                    onContextMenu={openSeatMenu}
                  />
                ))}
                {disabledSeats.length > 0 && (
                  <div className="flex flex-wrap gap-1.5 px-2.5 pb-1 pt-1.5">
                    {disabledSeats.map((d) => (
                      <span
                        key={`${d.adapter}:${d.channel}`}
                        title="未启用——设置 → Agent 里开启后 ≤10 秒自动入座"
                        className="cursor-not-allowed rounded-full border border-border bg-surface px-2.5 py-1 text-[11px] text-faint"
                      >
                        {d.adapter}
                        {d.channel && d.channel !== "cli" ? ` · ${d.channel}` : ""} · 未启用
                      </span>
                    ))}
                  </div>
                )}
              </section>
              {groups.length > 0 && (
                <section aria-label="群聊">
                  <h3 className="px-2.5 pb-1 pt-2 text-[11px] font-medium text-faint">群聊</h3>
                  {groups.map((r) => (
                    <RoomEntry key={r.room_id} room={r} onContextMenu={openRoomMenu} />
                  ))}
                </section>
              )}
              {rooms.length === 0 && seatRows.length === 0 && (
                <p className="px-2 py-3 text-xs text-faint">
                  还没有会话——点上方 + 开始第一场讨论。
                </p>
              )}
            </>
          )}
          {error && <p className="px-2 py-2 text-xs text-danger">{error}</p>}
        </nav>
        <div className="flex items-center gap-1 border-t border-border px-2 py-2">
          <NavLink
            to="/settings"
            title="设置（Agent 实例 / 自动化 / 数据 / 外观 / 开发者）"
            className={({ isActive }) =>
              `flex min-w-0 flex-1 items-center gap-2 rounded-lg px-2 py-1.5 transition-colors ${
                isActive ? "bg-surface-3" : "hover:bg-surface-2"
              }`
            }
          >
            <IconGear />
            <span className="truncate text-sm">设置</span>
          </NavLink>
          <button
            type="button"
            onClick={() => setTheme(toggleTheme(theme))}
            title={theme === "dark" ? "切换亮色" : "切换暗色"}
            aria-label="切换主题"
            className="rounded-lg p-2 text-dim transition-colors hover:bg-surface-2 hover:text-text"
          >
            {theme === "dark" ? <IconSun /> : <IconMoon />}
          </button>
        </div>
      </aside>
      <main className="min-w-0 flex-1">
        <Outlet />
      </main>

      {menu.element}

      {renaming && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/40"
          role="dialog"
          aria-modal="true"
          aria-label="重命名房间"
          onClick={() => !renameBusy && setRenaming(null)}
          onKeyDown={(e) => {
            if (e.key === "Escape" && !renameBusy) setRenaming(null);
          }}
        >
          <div
            className="mx-4 w-full max-w-md rounded-2xl border border-border bg-surface p-5 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <h2 className="text-sm font-semibold text-text">重命名房间</h2>
            <input
              autoFocus
              value={renaming.name}
              onChange={(e) => setRenaming({ ...renaming, name: e.target.value })}
              onKeyDown={(e) => {
                if (e.key === "Enter") void submitRename();
              }}
              maxLength={120}
              disabled={renameBusy}
              aria-label="房间名"
              className="mt-3 w-full rounded-lg border border-border bg-surface-2 px-2.5 py-1.5 text-sm outline-none focus:border-accent"
            />
            {renameError && <p className="mt-2 text-xs text-danger">{renameError}</p>}
            <div className="mt-3 flex justify-end gap-2">
              <button
                type="button"
                disabled={renameBusy}
                onClick={() => setRenaming(null)}
                className="rounded-lg px-3 py-1.5 text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text disabled:opacity-40"
              >
                取消
              </button>
              <button
                type="button"
                disabled={renameBusy || !renaming.name.trim()}
                onClick={() => void submitRename()}
                className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
              >
                {renameBusy ? "保存中…" : "保存"}
              </button>
            </div>
          </div>
        </div>
      )}

      {deleting && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/40"
          role="dialog"
          aria-modal="true"
          aria-label="删除房间确认"
          onClick={() => !deleteBusy && setDeleting(null)}
          onKeyDown={(e) => {
            if (e.key === "Escape" && !deleteBusy) setDeleting(null);
          }}
        >
          <div
            className="mx-4 w-full max-w-md rounded-2xl border border-border bg-surface p-5 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <h2 className="text-sm font-semibold text-text">删除房间「{deleting.name}」</h2>
            <p className="mt-2 text-xs leading-5 text-dim">
              删除不可逆：本房间的全部消息、任务与记忆记录将被清除（事件日志级联删除，仅保留墓碑与删除理由）。
            </p>
            <textarea
              autoFocus
              value={deleteReason}
              onChange={(e) => setDeleteReason(e.target.value)}
              maxLength={280}
              rows={2}
              disabled={deleteBusy}
              placeholder="删除理由（必填，1–280 字，留痕审计）"
              aria-label="删除理由"
              className="mt-3 w-full resize-none rounded-lg border border-border bg-surface-2 px-2.5 py-1.5 text-sm outline-none placeholder:text-faint focus:border-danger"
            />
            {deleteError && <p className="mt-2 text-xs text-danger">{deleteError}</p>}
            <div className="mt-3 flex justify-end gap-2">
              <button
                type="button"
                disabled={deleteBusy}
                onClick={() => setDeleting(null)}
                className="rounded-lg px-3 py-1.5 text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text disabled:opacity-40"
              >
                取消
              </button>
              <button
                type="button"
                disabled={deleteBusy || !deleteReason.trim()}
                onClick={() => void submitDelete()}
                className="rounded-lg bg-danger px-3 py-1.5 text-xs font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-40"
              >
                {deleteBusy ? "删除中…" : "确认删除"}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function IconPlus() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round">
      <path d="M12 5v14M5 12h14" />
    </svg>
  );
}

function IconGroup() {
  return (
    <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
      <circle cx="9" cy="7" r="4" />
      <path d="M23 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75" />
    </svg>
  );
}

function IconGear() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
    </svg>
  );
}

function IconSun() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41" />
    </svg>
  );
}

function IconMoon() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
      <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z" />
    </svg>
  );
}
