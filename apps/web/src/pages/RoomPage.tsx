// 房间页（v1.71 顶栏重排——功能与交互逻辑先行）：
//   顶栏左侧：房名（双击改名）+ 连接态 + 暂停徽标（信息，只读）
//   顶栏右侧：搜索（房间级检索浮层）、暂停/恢复（中频操作）、⋯ 菜单（改名/删除
//   ——危险低频操作收进菜单）、信息面板开关（高频）
//   主区：消息流 + 正在输入/失败条 + 输入框（结构健康，不动）
//   右侧：RoomPanel 常驻信息面板（五分段，默认展开）
// M4-0：消息复制/引用回复接线；删除房间确认（reason 必填留痕）；RFC-0013 附件。
import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api } from "../api/client";
import { useRoom, type Connection } from "../api/room";
import { Composer, type QuotedMessage } from "../components/chat/Composer";
import { MessageList } from "../components/chat/MessageList";
import { RoomPanel } from "../components/chat/RoomPanel";
import { RoomSearch } from "../components/chat/RoomSearch";
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
  // v1.71：右侧信息面板默认展开（信息常驻；高频开关收顶栏图标）。
  const [panelOpen, setPanelOpen] = useState(true);
  const [searchOpen, setSearchOpen] = useState(false);
  const [endorseBusy, setEndorseBusy] = useState<string | null>(null);
  const [inviteBusy, setInviteBusy] = useState<string | null>(null);
  const [taskBusy, setTaskBusy] = useState<string | null>(null);
  const [memoryBusy, setMemoryBusy] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [nameDraft, setNameDraft] = useState("");
  // M4-0 引用回复：被引消息（发送成功后清除）；删除房间：确认弹层 + 留痕理由。
  const [quoted, setQuoted] = useState<QuotedMessage | null>(null);
  // v1.73 右键点名注入：{pid, nonce}——nonce 使同目标可重复触发；Composer 消费后清零。
  const [mentionRequest, setMentionRequest] = useState<{ pid: string; nonce: number } | null>(null);
  const mentionNonce = useRef(0);
  // RFC-0013 附件：已上传待发送的令牌集（上传即时、发送定稿）。
  const [pendingAttachments, setPendingAttachments] = useState<{ token: string; name: string; sizeBytes: number }[]>([]);
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

  // RFC-0013 附件：选中即上传（令牌 24h 有效），发送时随消息定稿；失败留提示。
  const onAddAttachment = useCallback(
    (file: File) => {
      if (!roomId) return;
      void api
        .uploadAttachment(roomId, file)
        .then((meta) =>
          setPendingAttachments((prev) =>
            prev.length >= 4 ? prev : [...prev, { token: meta.token, name: meta.name, sizeBytes: meta.size_bytes }],
          ),
        )
        .catch(() => {});
    },
    [roomId],
  );

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

  // v1.73 右键"引用并 @ 作者"：引用卡片 + 点名注入（agent 消息专用）。
  const onQuoteMention = useCallback(
    (entry: { key: string; actorID: string; body?: string | null; kind: string }) => {
      if (entry.kind !== "message") return;
      setQuoted({
        eventID: entry.key,
        author: displayNameOf(room.participants, entry.actorID),
        excerpt: truncate((entry.body ?? "").replace(/\s+/g, " ").trim(), 64),
      });
      mentionNonce.current += 1;
      setMentionRequest({ pid: entry.actorID, nonce: mentionNonce.current });
    },
    [room.participants],
  );

  // v1.73 成员行右键 @：仅点名注入（不携带引用）。
  const onMention = useCallback((pid: string) => {
    mentionNonce.current += 1;
    setMentionRequest({ pid, nonce: mentionNonce.current });
  }, []);

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
  // M4-1：任务段"执行"按钮 → run_task（负责人 + 任务文本 + task_id 关联）。
  const onRunTask = useCallback(
    (taskID: string, assignee: string, instruction: string) => {
      setTaskBusy(taskID);
      void room
        .runTask(assignee, instruction, taskID)
        .catch(() => {})
        .finally(() => setTaskBusy(null));
    },
    [room],
  );

  // M4-1 切片 B：取消在途执行（理由 1..280 留痕；迟到结果只审计不发布）
  const onCancelRun = useCallback(
    (runID: string, reason: string) => {
      void room.cancelRun(runID, reason).catch(() => {});
    },
    [room],
  );

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
    (memoryID: string, edits: { conclusions?: string[]; assumptions?: string[]; curatedContent?: string }, note: string) => {
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
      <header className="relative z-20 flex items-center gap-2 border-b border-border px-4 py-2.5">
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
          <h1 className="cursor-text truncate text-sm font-medium tracking-tight" title="双击改名" onDoubleClick={startRename}>
            {room.displayName || "加载中…"}
          </h1>
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
          onClick={() => setSearchOpen((v) => !v)}
          aria-pressed={searchOpen}
          title="搜索房内消息"
          aria-label="搜索房内消息"
          className={`rounded-lg p-2 transition-colors ${
            searchOpen ? "bg-surface-3 text-text" : "text-dim hover:bg-surface-2 hover:text-text"
          }`}
        >
          <IconSearch />
        </button>
        <button
          type="button"
          onClick={() => void (room.paused ? room.resume() : room.pause()).catch(() => {})}
          title={room.paused ? "恢复讨论（Agent 重新开始评估发言）" : "暂停讨论（Agent 停止自主评估）"}
          className="rounded-lg px-2.5 py-1.5 text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text"
        >
          {room.paused ? "恢复讨论" : "暂停"}
        </button>
        <RoomMenu
          onRename={startRename}
          onDelete={() => {
            setDeleteReason("");
            setDeleting(true);
          }}
        />
        <button
          type="button"
          onClick={() => setPanelOpen((v) => !v)}
          aria-pressed={panelOpen}
          title={panelOpen ? "收起信息面板" : "展开信息面板（成员/任务/记忆/讨论）"}
          aria-label="信息面板开关"
          className={`rounded-lg p-2 transition-colors ${
            panelOpen ? "bg-surface-3 text-text" : "text-dim hover:bg-surface-2 hover:text-text"
          }`}
        >
          <IconPanel />
        </button>
        {searchOpen && roomId && (
          <RoomSearch roomID={roomId} onJumpToEvent={onJumpToEvent} onClose={() => setSearchOpen(false)} />
        )}
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
            roomID={roomId}
            onQuote={(e) => onQuote(e.key)}
            onQuoteMention={onQuoteMention}
            onJumpToEvent={onJumpToEvent}
          />
          <TypingBar typing={room.typing} participants={room.participants} failures={room.seatFailures} />
          <Composer
            disabled={!room.roomID}
            paused={room.paused}
            agents={agents}
            quoted={quoted}
            onCancelQuote={() => setQuoted(null)}
            attachments={pendingAttachments}
            onAddAttachment={onAddAttachment}
            onRemoveAttachment={(token) => setPendingAttachments((prev) => prev.filter((a) => a.token !== token))}
            mentionRequest={mentionRequest}
            onMentionConsumed={() => setMentionRequest(null)}
            onSend={(body, addressedTo, replyTo, attachments) => {
              void room
                .send(body, addressedTo, replyTo, attachments)
                .then(() => {
                  setQuoted(null); // 发送成功才弃引用（失败保留可重试）
                  setPendingAttachments([]); // 附件令牌已被服务端消费
                })
                .catch(() => {});
            }}
          />
        </div>
        {panelOpen && (
          <RoomPanel
            roomID={roomId}
            participants={room.participants}
            roster={room.roster}
            scorecard={room.scorecard}
            threads={room.threads}
            edges={room.edges}
            closures={room.closures}
            tasks={room.tasks}
            runs={room.runs}
            seatFailures={room.seatFailures}
            endorseBusy={endorseBusy}
            onEndorse={onEndorse}
            inviteBusy={inviteBusy}
            onInvite={onInvite}
            onProposeClosure={(threadID) => void room.proposeClosure(threadID)}
            onAcceptClosure={(closureID) => void room.acceptClosure(closureID)}
            closureBusy={false}
            onResolveTask={onResolveTask}
            onRun={onRunTask}
            onCancelRun={onCancelRun}
            taskBusy={taskBusy}
            onEditMemory={onEditMemory}
            memoryBusy={memoryBusy}
            onJumpToEvent={onJumpToEvent}
            onMention={onMention}
            onTabActive={onTabActive}
            onClose={() => setPanelOpen(false)}
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

/** ⋯ 菜单：改名 / 删除房间（危险低频操作收进菜单，不占顶栏常驻位）。 */
function RoomMenu({ onRename, onDelete }: { onRename: () => void; onDelete: () => void }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        aria-haspopup="menu"
        title="更多操作"
        aria-label="更多操作"
        className={`rounded-lg p-2 transition-colors ${
          open ? "bg-surface-3 text-text" : "text-dim hover:bg-surface-2 hover:text-text"
        }`}
      >
        <IconMore />
      </button>
      {open && (
        <div
          role="menu"
          className="animate-fade-in absolute right-0 top-full z-40 mt-1 w-44 overflow-hidden rounded-xl border border-border bg-surface py-1 shadow-xl"
        >
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              setOpen(false);
              onRename();
            }}
            className="block w-full px-3 py-1.5 text-left text-xs text-dim transition-colors hover:bg-surface-2 hover:text-text"
          >
            重命名房间
          </button>
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              setOpen(false);
              onDelete();
            }}
            className="block w-full px-3 py-1.5 text-left text-xs text-dim transition-colors hover:bg-[color-mix(in_srgb,var(--danger)_12%,transparent)] hover:text-danger"
          >
            删除房间…
          </button>
        </div>
      )}
    </div>
  );
}

function IconSearch() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
      <circle cx="11" cy="11" r="7" />
      <path d="M21 21l-4.35-4.35" />
    </svg>
  );
}

function IconMore() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="currentColor">
      <circle cx="5" cy="12" r="1.6" />
      <circle cx="12" cy="12" r="1.6" />
      <circle cx="19" cy="12" r="1.6" />
    </svg>
  );
}

function IconPanel() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
      <rect x="3" y="4" width="18" height="16" rx="2" />
      <path d="M15 4v16" />
    </svg>
  );
}
