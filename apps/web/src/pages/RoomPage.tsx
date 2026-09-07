// 房间页：顶栏（房名双击/编辑图标改名、连接态、暂停/恢复/删除、抽屉开关）
// + 消息流 + 正在输入区 + 输入框 + 右侧可折叠抽屉（成员/发言评估/话题线）。
// M4-0：消息复制/引用回复接线（引用状态在此持有）；删除房间确认（reason 必填留痕）。
import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useRoom, type Connection } from "../api/room";
import { Composer, type QuotedMessage } from "../components/chat/Composer";
import { MemberPanel } from "../components/chat/MemberPanel";
import { MessageList } from "../components/chat/MessageList";
import { TypingBar } from "../components/chat/TypingBar";
import { displayNameOf, truncate } from "../lib/ui";
import { refreshRooms } from "../state/rooms";

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
  const navigate = useNavigate();
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [endorseBusy, setEndorseBusy] = useState<string | null>(null);
  const [inviteBusy, setInviteBusy] = useState<string | null>(null);
  const [taskBusy, setTaskBusy] = useState<string | null>(null);
  const [memoryBusy, setMemoryBusy] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [nameDraft, setNameDraft] = useState("");
  // M4-0 引用回复：被引消息（发送成功后清除）；删除房间：确认弹层 + 留痕理由。
  const [quoted, setQuoted] = useState<QuotedMessage | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [deleteReason, setDeleteReason] = useState("");
  const [deleteBusy, setDeleteBusy] = useState(false);

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

  // M4-0 引用回复：从时间线条目构造引用卡片（作者 + 摘要）；发送成功才清除。
  const onQuote = useCallback(
    (eventID: string) => {
      const e = room.entries.find((x) => x.key === eventID);
      if (!e || e.kind !== "message") return;
      setQuoted({
        eventID,
        author: displayNameOf(room.participants, e.actorID),
        excerpt: truncate((e.body ?? "").replace(/\s+/g, " ").trim(), 64),
      });
    },
    [room.entries, room.participants],
  );

  // M4-0 删除房间：M3-6 命令（reason 必填 1..280 字）→ 级联清库 → 回列表。
  const onDeleteConfirm = useCallback(async () => {
    const reason = deleteReason.trim();
    if (!reason || deleteBusy) return;
    setDeleteBusy(true);
    try {
      await room.del(reason);
      await refreshRooms();
      navigate("/");
    } catch {
      // 失败信息已在 room.error 条展示；弹层保留供重试或取消
      setDeleteBusy(false);
    }
  }, [deleteReason, deleteBusy, room, navigate]);

  // M3-3 任务裁定 / 记忆编辑：SSE 事件驱动快照重投影；SSE 未达时兜底手刷一次。
  const onResolveTask = useCallback(
    (taskID: string, resolution: "delivered" | "dismissed") => {
      setTaskBusy(taskID);
      void room
        .resolveTask(taskID, resolution)
        .then(() => room.refreshProjections())
        .catch(() => {})
        .finally(() => setTaskBusy(null));
    },
    [room],
  );
  const onEditMemory = useCallback(
    (memoryID: string, edits: { conclusions?: string[]; assumptions?: string[] }, note: string) => {
      setMemoryBusy(memoryID);
      void room
        .editMemory(memoryID, edits, note)
        .then(() => room.refreshProjections())
        .catch(() => {})
        .finally(() => setMemoryBusy(null));
    },
    [room],
  );

  // provenance 跳转：时间线按 event_id 定位（消息流 DOM 无 id 锚点，最小版用
  // 文本检索找不到则静默——消息本体始终可按内容回看）。
  const onJumpToEvent = useCallback(
    (eventID: string) => {
      const el = document.querySelector(`[data-event-id="${CSS.escape(eventID)}"]`);
      if (el) {
        el.scrollIntoView({ behavior: "smooth", block: "center" });
        el.classList.add("jump-flash");
        setTimeout(() => el.classList.remove("jump-flash"), 1600);
      }
    },
    [],
  );

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
          onClick={() => {
            setDeleteReason("");
            setDeleting(true);
          }}
          title="删除房间（不可逆，事件与任务记录一并清除）"
          className="rounded-lg px-2.5 py-1 text-xs text-dim transition-colors hover:bg-[color-mix(in_srgb,var(--danger)_12%,transparent)] hover:text-danger"
        >
          删除
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
          <MessageList
            entries={room.entries}
            participants={room.participants}
            onQuote={(e) => onQuote(e.key)}
            onJumpToEvent={onJumpToEvent}
          />
          <TypingBar typing={room.typing} participants={room.participants} />
          <Composer
            disabled={!room.roomID}
            paused={room.paused}
            agents={agents}
            quoted={quoted}
            onCancelQuote={() => setQuoted(null)}
            onSend={(body, addressedTo, replyTo) => {
              void room
                .send(body, addressedTo, replyTo)
                .then(() => setQuoted(null)) // 发送成功才弃引用（失败保留可重试）
                .catch(() => {});
            }}
          />
        </div>
        {drawerOpen && (
          <MemberPanel
            roomID={roomId}
            participants={room.participants}
            roster={room.roster}
            scorecard={room.scorecard}
            threads={room.threads}
            edges={room.edges}
            closures={room.closures}
            tasks={room.tasks}
            endorseBusy={endorseBusy}
            onEndorse={onEndorse}
            inviteBusy={inviteBusy}
            onInvite={onInvite}
            onProposeClosure={(threadID) => void room.proposeClosure(threadID)}
            onAcceptClosure={(closureID) => void room.acceptClosure(closureID)}
            closureBusy={false}
            onResolveTask={onResolveTask}
            taskBusy={taskBusy}
            onEditMemory={onEditMemory}
            memoryBusy={memoryBusy}
            onJumpToEvent={onJumpToEvent}
            onTabActive={onTabActive}
            onClose={() => setDrawerOpen(false)}
            describeEvent={describeEvent}
          />
        )}
      </div>
      {deleting && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/40"
          role="dialog"
          aria-modal="true"
          aria-label="删除房间确认"
          onClick={() => !deleteBusy && setDeleting(false)}
          onKeyDown={(e) => {
            if (e.key === "Escape" && !deleteBusy) setDeleting(false);
          }}
        >
          <div
            className="mx-4 w-full max-w-md rounded-2xl border border-border bg-surface p-5 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <h2 className="text-sm font-semibold text-text">删除房间「{room.displayName}」</h2>
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
            <div className="mt-3 flex justify-end gap-2">
              <button
                type="button"
                disabled={deleteBusy}
                onClick={() => setDeleting(false)}
                className="rounded-lg px-3 py-1.5 text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text disabled:opacity-40"
              >
                取消
              </button>
              <button
                type="button"
                disabled={deleteBusy || !deleteReason.trim()}
                onClick={() => void onDeleteConfirm()}
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

function IconPencil() {
  return (
    <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
      <path d="M17 3a2.85 2.83 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5z" />
    </svg>
  );
}
