// 记忆 Tab（v1.70 重构——展示对齐：模型看得到的，开发者也能看得到）：
// ①策展记忆（agent 每波评审自助沉淀的条目，Hermes 同构——容量水位/编辑纠错）
// ②胶囊记忆（收束共识，编辑后视图与语境注入同源）
// ③模型视角（语境平面全景：近窗/召回命中/恒常平面/tasklist——零收束也有内容）
// ④运行回执（每次评估/生成/评审实际交付了什么，AR-16 正式面）
// ⑤房内全文检索（按需平面，FTS5 trigram）。
import { useCallback, useEffect, useState } from "react";
import {
  api,
  type ContextPanorama,
  type CuratedBudget,
  type CuratedEntry,
  type MemoryCapsule,
  type ParticipantView,
  type ReceiptItem,
  type SearchHit,
} from "../../api/client";
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
  onEdit: (memoryID: string, edits: { conclusions?: string[]; assumptions?: string[]; curatedContent?: string }, note: string) => void;
  onJumpToEvent: (eventID: string) => void;
}) {
  const [capsules, setCapsules] = useState<MemoryCapsule[] | null>(null);
  const [budget, setBudget] = useState<MemoryCapsuleBudget | null>(null);
  const [curated, setCurated] = useState<CuratedEntry[] | null>(null);
  const [curatedBudget, setCuratedBudget] = useState<CuratedBudget | null>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!roomID) return;
    setCapsules(null);
    setCurated(null);
    api
      .roomMemory(roomID)
      .then((d) => {
        setCapsules(d.capsules ?? []);
        setBudget(d.capsule_budget ?? null);
        setCurated(d.curated ?? []);
        setCuratedBudget(d.curated_budget ?? null);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [roomID]);

  if (!roomID) return null;
  return (
    <div className="py-1">
      <CuratedSection
        entries={curated}
        budget={curatedBudget}
        participants={participants}
        editing={editing}
        setEditing={setEditing}
        editBusy={editBusy}
        onEdit={onEdit}
      />
      <h3 className="px-3 pb-1 pt-3 text-xs font-medium text-dim">胶囊记忆（{capsules?.length ?? "…"}）</h3>
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
      <ModelViewSection roomID={roomID} />
      <ReceiptsSection roomID={roomID} />
      <RoomSearch roomID={roomID} onJumpToEvent={onJumpToEvent} />
    </div>
  );
}

/** 策展记忆区：agent 每波评审沉淀的条目（最新在后；注入时倒序取新）。 */
function CuratedSection({
  entries,
  budget,
  participants,
  editing,
  setEditing,
  editBusy,
  onEdit,
}: {
  entries: CuratedEntry[] | null;
  budget: CuratedBudget | null;
  participants: ParticipantView[];
  editing: string | null;
  setEditing: (id: string | null) => void;
  editBusy: string | null;
  onEdit: (memoryID: string, edits: { curatedContent?: string }, note: string) => void;
}) {
  const [draft, setDraft] = useState("");
  const [note, setNote] = useState("");
  const editingEntry = entries?.find((e) => e.id === editing) ?? null;
  return (
    <section>
      <h3 className="px-3 pb-1 pt-2 text-xs font-medium text-dim">策展记忆（{entries?.length ?? "…"}）</h3>
      <p className="px-3 pb-1.5 text-[11px] text-faint">
        Agent 每波结束后评审本波对话，自助沉淀值得长期记住的事实与偏好（Hermes 同构：免审批、
        容量硬门、拒绝留痕）。人工纠错立即生效于下一次组装。
        {budget && (
          <> 水位 {budget.curated_runes}/{budget.budget_runes} 字{budget.over_budget_runes > 0 && <span className="text-warn">（超限 {budget.over_budget_runes} 字——注入裁剪中，建议合并）</span>}。</>
        )}
      </p>
      {entries?.length === 0 && (
        <p className="px-3 py-1 text-xs text-faint">尚无策展条目——有发布的波结束后评审座位会自动沉淀（无合适内容则跳过）。</p>
      )}
      {editingEntry ? (
        <div className="mx-3 mb-2 space-y-1.5 rounded-lg border border-border bg-surface-2 p-2">
          <div className="text-[11px] text-faint">
            编辑 <span className="font-mono">{editingEntry.id}</span>（{displayNameOf(participants, editingEntry.author)} 沉淀）
          </div>
          <textarea
            value={draft || editingEntry.content}
            onChange={(e) => setDraft(e.target.value)}
            rows={3}
            className="w-full rounded-lg border border-border bg-surface-2 px-2 py-1 text-xs text-text"
          />
          <div className="flex gap-1.5">
            <input
              value={note}
              onChange={(e) => setNote(e.target.value)}
              placeholder="编辑理由（留痕）"
              className="min-w-0 flex-1 rounded-lg border border-border bg-surface-2 px-2 py-1 text-xs text-text"
            />
            <button
              type="button"
              disabled={editBusy === editingEntry.id}
              onClick={() => {
                const content = (draft || editingEntry.content).trim();
                if (!content) return;
                onEdit(editingEntry.id, { curatedContent: content }, note || "人工纠错");
                setEditing(null);
                setDraft("");
                setNote("");
              }}
              className="rounded-lg bg-surface-3 px-2.5 py-1 text-[11px] text-text transition-opacity hover:opacity-85 disabled:opacity-40"
            >
              {editBusy === editingEntry.id ? "提交中…" : "保存"}
            </button>
            <button
              type="button"
              onClick={() => {
                setEditing(null);
                setDraft("");
                setNote("");
              }}
              className="rounded-lg px-2.5 py-1 text-[11px] text-dim hover:text-text"
            >
              取消
            </button>
          </div>
        </div>
      ) : (
        <ul className="divide-y divide-border">
          {(entries ?? []).map((e) => (
            <li key={e.id} className="flex items-start gap-2 px-3 py-2 text-xs">
              <div className="min-w-0 flex-1">
                <p className="whitespace-pre-wrap text-text">{e.content}</p>
                <p className="mt-0.5 text-faint">
                  {displayNameOf(participants, e.author)} · {e.id}
                  {e.edited && " · 已人工纠错"}
                </p>
              </div>
              <button
                type="button"
                onClick={() => setEditing(e.id)}
                className="rounded-lg px-2 py-0.5 text-[11px] text-dim transition-colors hover:bg-surface-2 hover:text-text"
              >
                编辑
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/** 模型视角：语境平面全景（与引擎组装同源——近窗/召回/恒常平面/tasklist）。 */
function ModelViewSection({ roomID }: { roomID: string }) {
  const [pano, setPano] = useState<ContextPanorama | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(() => {
    setPano(null);
    setError(null);
    api
      .roomContext(roomID)
      .then(setPano)
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [roomID]);
  useEffect(load, [load]);
  return (
    <section className="px-3 pt-3">
      <div className="flex items-center gap-2 pb-1">
        <h3 className="text-xs font-medium text-dim">模型视角（最新波平面全景）</h3>
        <button type="button" onClick={load} className="rounded-lg px-1.5 py-0.5 text-[11px] text-dim hover:text-text">
          刷新
        </button>
      </div>
      {error && <p className="text-[11px] text-danger">{error}</p>}
      {!pano && !error && <p className="text-xs text-faint">加载中…</p>}
      {pano && (
        <div className="space-y-2 rounded-lg border border-border bg-surface-2 p-2.5 text-[11px]">
          <div>
            <span className="text-faint">近窗（最新 {pano.near_window.length} 条）：</span>
            <ul className="mt-1 space-y-0.5">
              {pano.near_window.map((m) => (
                <li key={m.event_id} className="truncate text-dim">
                  <span className="text-text">{m.actor_kind === "human" ? "人类" : shortId(m.actor)}</span>
                  ：{truncate(m.body, 60)}
                </li>
              ))}
              {pano.near_window.length === 0 && <li className="text-faint">（空）</li>}
            </ul>
          </div>
          <div>
            <span className="text-faint">
              按需召回（关键词：{pano.retrieval_keywords.length > 0 ? pano.retrieval_keywords.join(" / ") : "无"}）：
            </span>
            <ul className="mt-1 space-y-0.5">
              {pano.retrieved.map((r) => (
                <li key={r.event_id} className="truncate text-dim">
                  <span className="text-text">{shortId(r.actor)}</span>：{truncate(r.body, 60)}
                </li>
              ))}
              {pano.retrieved.length === 0 && <li className="text-faint">（本波无命中）</li>}
            </ul>
          </div>
          <div>
            <span className="text-faint">恒常平面：</span>
            <span className="text-dim">
              策展 {pano.budget.curated_count} 条（{pano.budget.curated_runes} 字）+ 胶囊 {pano.budget.capsule_count} 个
              （{pano.budget.capsule_runes} 字）/ 预算 {pano.budget.budget_runes} 字
            </span>
          </div>
          <div>
            <span className="text-faint">未交付承诺：</span>
            <span className="text-dim">
              {pano.tasklist.length === 0
                ? "无"
                : pano.tasklist.map((t) => `${shortId(t.owner)}：${truncate(t.text, 24)}${t.overdue ? "（逾期）" : ""}`).join("；")}
            </span>
          </div>
        </div>
      )}
    </section>
  );
}

/** 运行回执：每次评估/生成/评审实际交付了什么（AR-16 正式面）。 */
function ReceiptsSection({ roomID }: { roomID: string }) {
  const [receipts, setReceipts] = useState<ReceiptItem[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  const load = useCallback(() => {
    setError(null);
    api
      .roomReceipts(roomID, 20)
      .then((d) => setReceipts(d.receipts ?? []))
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [roomID]);
  useEffect(() => {
    if (open && receipts === null) load();
  }, [open, receipts, load]);
  return (
    <section className="px-3 pt-3">
      <div className="flex items-center gap-2 pb-1">
        <h3 className="text-xs font-medium text-dim">运行回执（{receipts?.length ?? "…"}）</h3>
        <button
          type="button"
          onClick={() => {
            setOpen(!open);
            if (!open) load();
          }}
          className="rounded-lg px-1.5 py-0.5 text-[11px] text-dim hover:text-text"
        >
          {open ? "收起" : "展开"}
        </button>
        {open && (
          <button type="button" onClick={load} className="rounded-lg px-1.5 py-0.5 text-[11px] text-dim hover:text-text">
            刷新
          </button>
        )}
      </div>
      {open && (
        <>
          {error && <p className="text-[11px] text-danger">{error}</p>}
          {!receipts && !error && <p className="text-xs text-faint">加载中…</p>}
          {receipts && (
            <ul className="space-y-1 rounded-lg border border-border bg-surface-2 p-2.5 text-[11px]">
              {receipts.length === 0 && <li className="text-faint">尚无运行回执。</li>}
              {receipts.map((r) => (
                <li key={r.receipt_id} className="text-dim" title={r.receipt_id}>
                  <span className="font-mono text-text">{r.task_id}</span> · 水位 {shortId(r.watermark)} · {r.layer_digests.length} 层
                </li>
              ))}
            </ul>
          )}
        </>
      )}
    </section>
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
