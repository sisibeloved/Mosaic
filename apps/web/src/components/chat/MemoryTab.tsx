// 记忆 Tab（M3-3 记忆查看/编辑/纠错闭环，RFC-0007 §7.4 裁定 5）：胶囊编辑后视图
//（与语境注入同源）+ 人工编辑（edit_memory，留 edit_history、生效于下次组装）+
// 容量水位（dropped>0 即超预算——倒逼合并/编辑）+ 房内全文检索（按需平面，
// FTS5 trigram）。数据自 GET /v1/rooms/{id}/memory 与 /search 拉取（Tab 打开时）。
import { useCallback, useEffect, useState } from "react";
import { api, type MemoryCapsule, type ParticipantView, type SearchHit } from "../../api/client";
import { displayNameOf, shortId, truncate } from "../../lib/ui";

export function MemoryTab({
  roomID,
  participants,
  editBusy,
  onEdit,
  onJumpToEvent,
}: {
  roomID: string | null;
  participants: ParticipantView[];
  editBusy: string | null;
  onEdit: (memoryID: string, edits: { conclusions?: string[]; assumptions?: string[] }, note: string) => void;
  onJumpToEvent: (eventID: string) => void;
}) {
  const [capsules, setCapsules] = useState<MemoryCapsule[] | null>(null);
  const [budget, setBudget] = useState<MemoryCapsuleBudget | null>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!roomID) return;
    setCapsules(null);
    api
      .roomMemory(roomID)
      .then((d) => {
        setCapsules(d.capsules ?? []);
        setBudget(d.capsule_budget ?? null);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [roomID]);

  if (!roomID) return null;
  return (
    <div className="py-1">
      <h3 className="px-3 pb-1 pt-2 text-xs font-medium text-dim">胶囊记忆（{capsules?.length ?? "…"}）</h3>
      {budget && budget.dropped_count > 0 && (
        <p className="mx-3 mb-2 rounded-lg border border-border bg-surface-2 px-2.5 py-2 text-[11px] text-warn">
          恒常平面超容量：{budget.injected_count} 条注入 / {budget.dropped_count} 条被挤出
          （{budget.injected_runes}/{budget.budget_runes} 字）——合并或精简旧胶囊后可恢复。
        </p>
      )}
      {error && <p className="px-3 py-2 text-xs text-danger">{error}</p>}
      {!error && capsules === null && <p className="px-3 py-2 text-xs text-faint">加载中…</p>}
      {capsules?.length === 0 && (
        <p className="px-3 py-2 text-xs text-faint">尚无已接受的收束胶囊——讨论收束并接受后，结论会作为房间长期记忆在此可查可编辑。</p>
      )}
      {capsules && capsules.length > 0 && (
        <ul className="divide-y divide-border">
          {capsules.map((c) => (
            <li key={c.closure_id} className="px-3 py-2 text-xs">
              <div className="flex items-center gap-2">
                <span className="font-mono text-text" title={c.closure_id}>
                  {shortId(c.closure_id)}
                </span>
                <span className="rounded bg-surface-3 px-1.5 text-[10px] leading-4 text-dim">
                  {c.closure_type === "bounded_disagreement" ? "有界分歧" : "共识"}
                </span>
                {c.edit_history?.length > 0 && (
                  <span className="text-faint">已编辑 ×{c.edit_history.length}</span>
                )}
                <button
                  type="button"
                  onClick={() => setEditing(editing === c.closure_id ? null : c.closure_id)}
                  className="ml-auto rounded-lg px-2 py-0.5 text-[11px] text-dim transition-colors hover:bg-surface-2 hover:text-text"
                >
                  {editing === c.closure_id ? "取消" : "编辑"}
                </button>
              </div>
              {editing === c.closure_id ? (
                <MemoryEditForm
                  capsule={c}
                  busy={editBusy === c.closure_id}
                  onSubmit={(edits, note) => onEdit(c.closure_id, edits, note)}
                />
              ) : (
                <CapsuleBody capsule={c} />
              )}
              {c.edit_history?.length > 0 && (
                <ul className="mt-1 text-faint">
                  {c.edit_history.slice(-3).reverse().map((h) => (
                    <li key={h.event_id}>
                      v{h.edit_version} {h.edited_by ? displayNameOf(participants, h.edited_by) : ""}：
                      {h.note || "（无备注）"}
                    </li>
                  ))}
                </ul>
              )}
            </li>
          ))}
        </ul>
      )}
      <RoomSearch roomID={roomID} onJumpToEvent={onJumpToEvent} />
    </div>
  );
}

interface MemoryCapsuleBudget {
  budget_runes: number;
  injected_runes: number;
  injected_count: number;
  dropped_count: number;
}

function CapsuleBody({ capsule }: { capsule: MemoryCapsule }) {
  return (
    <div className="mt-1 space-y-1">
      {capsule.conclusions?.length > 0 && (
        <div className="text-dim">
          <span className="text-faint">结论：</span>
          <ul className="list-disc pl-4">
            {capsule.conclusions.map((s, i) => (
              <li key={i}>{s}</li>
            ))}
          </ul>
        </div>
      )}
      {capsule.assumptions?.length > 0 && (
        <div className="text-dim">
          <span className="text-faint">假设：</span>
          {capsule.assumptions.join("；")}
        </div>
      )}
      {capsule.named_dissent?.length > 0 && (
        <div className="text-dim">
          <span className="text-faint">异议：</span>
          {capsule.named_dissent.map((d, i) => (
            <span key={i}>
              {i > 0 && "；"}
              {d.participant_id}：{d.basis}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

/** 编辑表单：conclusions/assumptions 各一行一条（换行分隔）；提交走 onEdit。 */
function MemoryEditForm({
  capsule,
  busy,
  onSubmit,
}: {
  capsule: MemoryCapsule;
  busy: boolean;
  onSubmit: (edits: { conclusions?: string[]; assumptions?: string[] }, note: string) => void;
}) {
  const [conclusions, setConclusions] = useState((capsule.conclusions ?? []).join("\n"));
  const [assumptions, setAssumptions] = useState((capsule.assumptions ?? []).join("\n"));
  const [note, setNote] = useState("");
  return (
    <div className="mt-1.5 space-y-1.5">
      <label className="block text-[11px] text-faint">
        结论（一行一条，整组替换）
        <textarea
          value={conclusions}
          onChange={(e) => setConclusions(e.target.value)}
          rows={3}
          className="mt-0.5 w-full rounded-lg border border-border bg-surface-2 px-2 py-1 text-xs text-text"
        />
      </label>
      <label className="block text-[11px] text-faint">
        假设（一行一条，可留空保持不变）
        <textarea
          value={assumptions}
          onChange={(e) => setAssumptions(e.target.value)}
          rows={2}
          className="mt-0.5 w-full rounded-lg border border-border bg-surface-2 px-2 py-1 text-xs text-text"
        />
      </label>
      <label className="block text-[11px] text-faint">
        编辑理由（留痕）
        <input
          value={note}
          onChange={(e) => setNote(e.target.value)}
          placeholder="如：原结论表述有误"
          className="mt-0.5 w-full rounded-lg border border-border bg-surface-2 px-2 py-1 text-xs text-text"
        />
      </label>
      <button
        type="button"
        disabled={busy}
        onClick={() => {
          const cl = conclusions.split("\n").map((s) => s.trim()).filter(Boolean);
          const as_ = assumptions.split("\n").map((s) => s.trim()).filter(Boolean);
          if (cl.length === 0 && as_.length === 0) return;
          onSubmit({ conclusions: cl, ...(as_.length > 0 ? { assumptions: as_ } : {}) }, note || "人工编辑");
        }}
        className="rounded-lg bg-surface-3 px-2.5 py-1 text-[11px] text-text transition-opacity hover:opacity-85 disabled:opacity-40"
      >
        {busy ? "提交中…" : "保存编辑"}
      </button>
      <p className="text-faint">编辑立即生效于 agent 的下一次上下文组装（注入同源视图）。</p>
    </div>
  );
}

/** 房内全文检索（按需平面）：FTS5 trigram，命中点击跳转时间线。 */
function RoomSearch({
  roomID,
  onJumpToEvent,
}: {
  roomID: string;
  onJumpToEvent: (eventID: string) => void;
}) {
  const [q, setQ] = useState("");
  const [hits, setHits] = useState<SearchHit[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const run = useCallback(() => {
    const query = q.trim();
    if (!query) return;
    setBusy(true);
    setError(null);
    api
      .searchMessages(roomID, query)
      .then((d) => setHits(d.hits ?? []))
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setBusy(false));
  }, [roomID, q]);

  return (
    <div className="px-3 pb-3 pt-3">
      <h3 className="pb-1 text-xs font-medium text-dim">房内检索</h3>
      <div className="flex gap-1.5">
        <input
          value={q}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && run()}
          placeholder="关键词（如：预算 超限）"
          className="min-w-0 flex-1 rounded-lg border border-border bg-surface-2 px-2 py-1 text-xs text-text"
        />
        <button
          type="button"
          disabled={busy || !q.trim()}
          onClick={run}
          className="rounded-lg bg-surface-3 px-2.5 py-1 text-[11px] text-text transition-opacity hover:opacity-85 disabled:opacity-40"
        >
          {busy ? "…" : "搜索"}
        </button>
      </div>
      {error && <p className="mt-1 text-[11px] text-danger">{error}</p>}
      {hits !== null && (
        <ul className="mt-1.5">
          {hits.length === 0 && <li className="py-1 text-[11px] text-faint">无命中。</li>}
          {hits.map((h) => (
            <li key={h.event_id}>
              <button
                type="button"
                onClick={() => onJumpToEvent(h.event_id)}
                className="w-full rounded-lg px-1.5 py-1 text-left text-[11px] text-dim transition-colors hover:bg-surface-2 hover:text-text"
              >
                <span className="text-text">{h.actor}</span>：{truncate(h.body, 48)}
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
