// 房间页：顶栏（房名双击/编辑图标改名、连接态、暂停/恢复、抽屉开关）
// + 消息流 + 正在输入区 + 输入框 + 右侧可折叠抽屉（成员/发言评估/话题线）。
import { useCallback, useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useRoom, type Connection } from "../api/room";
import { Composer } from "../components/chat/Composer";
import { MemberPanel } from "../components/chat/MemberPanel";
import { MessageList } from "../components/chat/MessageList";
import { TypingBar } from "../components/chat/TypingBar";
import { displayNameOf, truncate } from "../lib/ui";
import { refreshRooms, setLastRoomId } from "../state/rooms";

const CONNECTION_TEXT: Record<Connection, string> = {
  idle: "未连接",
  connecting: "连接中",
  live: "实时",
  reconnecting: "重连中",
  resync: "恢复中",
};

export function RoomPage() {
  const { roomId = null } = useParams();
  const room = useRoom(roomId);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [endorseBusy, setEndorseBusy] = useState<string | null>(null);
  const [inviteBusy, setInviteBusy] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [nameDraft, setNameDraft] = useState("");

  useEffect(() => {
    if (roomId) setLastRoomId(roomId);
  }, [roomId]);

  // 房间内有新事件 → 防抖轻量刷新侧栏列表（last_event_at 排序；不轮询）
  const entryCount = room.entries.length;
  useEffect(() => {
    if (entryCount === 0) return;
    const t = setTimeout(() => void refreshRooms(), 800);
    return () => clearTimeout(t);
  }, [entryCount]);

  const startRename = () => {
    setNameDraft(room.displayName);
    setEditing(true);
  };

  const submitRename = async () => {
    const name = nameDraft.trim();
    setEditing(false);
    if (!name || name === room.displayName) return;
    try {
      await room.rename(name);
      await refreshRooms();
    } catch {
      // 失败信息已在 room.error 条展示
    }
  };

  const onEndorse = useCallback(
    (intentID: string) => {
      setEndorseBusy(intentID);
      void room
        .endorse(intentID)
        .catch(() => {})
        .finally(() => setEndorseBusy(null));
    },
    [room],
  );

  // invite_agent：participant.admitted SSE 事件驱动投影刷新；SSE 未达时兜底手刷一次。
  const onInvite = useCallback(
    (participantID: string) => {
      setInviteBusy(participantID);
      void room
        .invite(participantID)
        .then(() => room.refreshProjections())
        .catch(() => {})
        .finally(() => setInviteBusy(null));
    },
    [room],
  );

  // refreshProjections 在 useRoom 内为稳定引用（useCallback 空依赖），首帧捕获即可。
  const onTabActive = useCallback(() => {
    void room.refreshProjections();
  }, []);

  // 观点关系边的可读引用：TimelineEntry.key 即 event_id（快照与 SSE 两路同键），
  // 能命中消息就解析成"名字：摘要"（摘要截 24 字），否则返回 null 由面板回退短 hash。
  const describeEvent = useCallback(
    (eventID: string): string | null => {
      const e = room.entries.find((x) => x.key === eventID);
      if (!e || e.kind !== "message" || !e.body) return null;
      const summary = truncate(e.body.replace(/\s+/g, " ").trim(), 24);
      return `${displayNameOf(room.participants, e.actorID)}：${summary}`;
    },
    [room.entries, room.participants],
  );

  if (roomId && room.error && !room.roomID) {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-3 text-sm">
        <p className="text-danger">{room.error}</p>
        <Link to="/" className="text-accent hover:underline">
          返回房间列表
        </Link>
      </div>
    );
  }

  const agents = room.participants.filter((p) => p.kind === "agent");

  return (
    <div className="flex h-full flex-col">
      <header className="flex items-center gap-2 border-b border-border px-4 py-2.5">
        {editing ? (
          <input
            autoFocus
            value={nameDraft}
            onChange={(e) => setNameDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void submitRename();
              if (e.key === "Escape") setEditing(false);
            }}
            onBlur={() => void submitRename()}
            maxLength={120}
            aria-label="房间名"
            className="w-64 rounded-lg border border-border bg-surface-2 px-2 py-1 text-sm outline-none focus:border-accent"
          />
        ) : (
          <>
            <h1
              className="cursor-text truncate text-sm font-medium tracking-tight"
              title="双击改名"
              onDoubleClick={startRename}
            >
              {room.displayName || "加载中…"}
            </h1>
            <button
              type="button"
              onClick={startRename}
              aria-label="改名"
              title="改名"
              className="rounded-lg p-1 text-faint transition-colors hover:bg-surface-2 hover:text-text"
            >
              <IconPencil />
            </button>
          </>
        )}
        <span className="flex items-center gap-1.5 text-[11px] text-faint" title={`连接状态：${CONNECTION_TEXT[room.connection]}`}>
          <span
            className={`inline-block h-1.5 w-1.5 rounded-full ${
              room.connection === "live"
                ? "bg-ok"
                : room.connection === "idle"
                  ? "bg-faint"
                  : "bg-warn"
            }`}
          />
          {CONNECTION_TEXT[room.connection]}
        </span>
        {room.paused && (
          <span className="rounded bg-[color-mix(in_srgb,var(--warn)_14%,transparent)] px-1.5 py-px text-[10px] leading-4 text-warn">
            已暂停
          </span>
        )}
        <div className="flex-1" />
        <button
          type="button"
          onClick={() => void (room.paused ? room.resume() : room.pause()).catch(() => {})}
          className="rounded-lg px-2.5 py-1 text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text"
        >
          {room.paused ? "恢复讨论" : "暂停"}
        </button>
        <button
          type="button"
          onClick={() => setDrawerOpen((v) => !v)}
          aria-pressed={drawerOpen}
          className={`rounded-lg px-2.5 py-1 text-xs transition-colors ${
            drawerOpen ? "bg-surface-3 text-text" : "text-dim hover:bg-surface-2 hover:text-text"
          }`}
        >
          成员
        </button>
      </header>
      {room.error && (
        <p className="border-b border-border bg-[color-mix(in_srgb,var(--danger)_8%,transparent)] px-4 py-1.5 text-xs text-danger">
          {room.error}
        </p>
      )}
      <div className="flex min-h-0 flex-1">
        <div className="flex min-w-0 flex-1 flex-col">
          <MessageList entries={room.entries} participants={room.participants} />
          <TypingBar typing={room.typing} participants={room.participants} />
          <Composer
            disabled={!room.roomID}
            paused={room.paused}
            agents={agents}
            onSend={(body, addressedTo) => void room.send(body, addressedTo).catch(() => {})}
          />
        </div>
        {drawerOpen && (
          <MemberPanel
            participants={room.participants}
            roster={room.roster}
            scorecard={room.scorecard}
            threads={room.threads}
            edges={room.edges}
            endorseBusy={endorseBusy}
            onEndorse={onEndorse}
            inviteBusy={inviteBusy}
            onInvite={onInvite}
            onTabActive={onTabActive}
            onClose={() => setDrawerOpen(false)}
            describeEvent={describeEvent}
          />
        )}
      </div>
    </div>
  );
}

function IconPencil() {
  return (
    <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
      <path d="M17 3a2.85 2.83 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5z" />
    </svg>
  );
}
