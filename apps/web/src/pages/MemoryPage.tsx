// 分域记忆页（0.7.0 / ADR-0037；0.7.1 / ADR-0038 策展扩展）：五命名空间
// tab + scope 过滤 + 状态口径（active 默认/proposed 候选/superseded 历史）
// + 时效开关 + 记忆卡操作（接受/否决与撤回/修订/替代）+ 版本留痕链。
// 房间收束胶囊（RFC-0007）仍是房间事件流事实——本页是分域 MemoryItem 的
// 独立读写面。
import { useEffect, useRef, useState } from "react";
import { api, Schemas } from "../api/client";

type MemoryItemView = Schemas["MemoryItemView"];
type Namespace = "agent" | "room" | "workspace" | "mission" | "slot";
type Kind = "semantic" | "episodic" | "procedural";
type StatusFilter = "active" | "proposed" | "superseded";

const NAMESPACES: { key: Namespace; label: string; hint: string }[] = [
  { key: "agent", label: "Agent", hint: "长期偏好、经验、公开人格配置——Agent 私有域" },
  { key: "room", label: "房间", hint: "项目知识、房间共识与异议（共享域）" },
  { key: "workspace", label: "工作区", hint: "工作区级知识（锚 default——单工作区事实）" },
  { key: "mission", label: "任务", hint: "阶段经验、假设、证据整理——随事务保存" },
  { key: "slot", label: "职责", hint: "职责实例的工作规范与环境说明" },
];
const KINDS: Kind[] = ["semantic", "episodic", "procedural"];
const KIND_LABEL: Record<Kind, string> = { semantic: "知识", episodic: "经历", procedural: "方法" };
const NS_SCOPE_PLACEHOLDER: Record<Namespace, string> = {
  agent: "agent_id（如 ag_…）",
  room: "room_id（如 rm_…）",
  workspace: "default（固定）",
  mission: "mission_id",
  slot: "slot_instance_id",
};
const CAUSE_LABEL: Record<string, string> = {
  revise: "修订", reject: "否决", withdraw: "撤回", supersede: "替代", accept: "接受",
};

function nowRFC3339(): string {
  return new Date().toISOString().replace(/\.\d{3}Z$/, "Z");
}

const inputCls = "w-full rounded-lg border border-line bg-surface-2 px-2.5 py-1.5 text-sm outline-none focus:border-accent";
const btnCls = "rounded-lg bg-accent px-3 py-1.5 text-sm text-white transition-opacity hover:opacity-90 disabled:opacity-50";
const btnGhostCls = "rounded-lg bg-surface-2 px-2.5 py-1 text-xs hover:bg-surface-3 disabled:opacity-50";

export function MemoryPage() {
  const [ns, setNs] = useState<Namespace>("agent");
  const [scopeFilter, setScopeFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("active");
  const [includeExpired, setIncludeExpired] = useState(false);
  const [items, setItems] = useState<MemoryItemView[] | null>(null);
  const [cursor, setCursor] = useState<string | null>(null);
  const [listErr, setListErr] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<string | null>(null);
  const seq = useRef(0);
  // 写入表单
  const [formOpen, setFormOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [form, setForm] = useState(() => ({
    namespace: "agent" as Namespace,
    scope_id: "",
    kind: "semantic" as Kind,
    title: "",
    content: "",
    sources: "",
    observed_at: nowRFC3339(),
    valid_from: "",
    valid_to: "",
    sensitivity: "normal" as "normal" | "sensitive",
    status: "active" as "active" | "proposed",
  }));
  // 0.7.1 策展编辑（修订/替代——目标条目 + 模式）
  const [editing, setEditing] = useState<{ mode: "revise" | "supersede"; item: MemoryItemView } | null>(null);
  const [editForm, setEditForm] = useState(() => ({ title: "", content: "", valid_from: "", valid_to: "", note: "", sensitivity: "normal" as "normal" | "sensitive" }));
  // 一审 #3：在途续页限制；二审：写入成功刷新信号。
  const [loadingMore, setLoadingMore] = useState(false);
  const [refreshTick, setRefreshTick] = useState(0);
  const [actionBusy, setActionBusy] = useState<string | null>(null);

  // 列表加载（浏览条件/状态口径/时效开关/写入成功重载；代次作废在途响应）。
  useEffect(() => {
    const cur = ++seq.current;
    setItems(null);
    setListErr(null);
    setCursor(null);
    api
      .listMemoryItems({
        namespace: ns,
        ...(scopeFilter.trim() ? { scope_id: scopeFilter.trim() } : {}),
        ...(statusFilter !== "active" ? { status: statusFilter } : {}),
        ...(includeExpired ? { include_expired: true } : {}),
        limit: 50,
      })
      .then((v) => {
        if (seq.current !== cur) return;
        setItems(v.items);
        setCursor(v.next_cursor ?? null);
      })
      .catch((e: unknown) => {
        if (seq.current !== cur) return;
        setListErr(e instanceof Error ? e.message : String(e));
      });
  }, [ns, scopeFilter, statusFilter, includeExpired, refreshTick]);

  // 一审 #3：代次检查 + 在途限制——切域后迟到响应不追加，同游标不重复发。
  const loadMore = () => {
    if (!cursor || loadingMore) return;
    const cur = seq.current;
    setLoadingMore(true);
    api
      .listMemoryItems({
        namespace: ns,
        ...(scopeFilter.trim() ? { scope_id: scopeFilter.trim() } : {}),
        ...(statusFilter !== "active" ? { status: statusFilter } : {}),
        ...(includeExpired ? { include_expired: true } : {}),
        limit: 50,
        before: cursor,
      })
      .then((v) => {
        if (seq.current !== cur) return; // 已切换浏览条件——丢弃迟到页
        setItems((prev) => [...(prev ?? []), ...v.items]);
        setCursor(v.next_cursor ?? null);
      })
      .catch((e: unknown) => {
        if (seq.current !== cur) return;
        setListErr(e instanceof Error ? e.message : String(e));
      })
      .finally(() => setLoadingMore(false));
  };

  // workspace 锚恒 default（表单内锁定）。
  useEffect(() => {
    setForm((f) => (f.namespace === "workspace" ? { ...f, scope_id: "default" } : f.scope_id === "default" ? { ...f, scope_id: "" } : f));
  }, [form.namespace]);

  const splitLines = (s: string) => s.split("\n").map((l) => l.trim()).filter(Boolean);

  const submit = () => {
    if (busy) return;
    setMsg(null);
    void (async () => {
      setBusy(true);
      try {
        await api.createMemoryItem({
          namespace: form.namespace,
          scope_id: form.namespace === "workspace" ? "default" : form.scope_id.trim(),
          kind: form.kind,
          title: form.title.trim(),
          content: form.content,
          observed_at: form.observed_at,
          ...(splitLines(form.sources).length ? { source_refs: splitLines(form.sources) } : {}),
          ...(form.valid_from ? { valid_from: form.valid_from } : {}),
          ...(form.valid_to ? { valid_to: form.valid_to } : {}),
          ...(form.sensitivity === "sensitive" ? { sensitivity: "sensitive" } : {}),
          ...(form.status === "proposed" ? { status: "proposed" } : {}),
        });
        setFormOpen(false);
        setForm((f) => ({ ...f, title: "", content: "", sources: "", observed_at: nowRFC3339(), valid_from: "", valid_to: "" }));
        setMsg("记忆已写入");
        setRefreshTick((t) => t + 1);
      } catch (e) {
        setMsg(e instanceof Error ? e.message : String(e));
      } finally {
        setBusy(false);
      }
    })();
  };

  // 策展动作（接受/否决/撤回）——成功后刷新信号 + 消息。
  const act = (item: MemoryItemView, kind: "accept" | "reject") => {
    if (actionBusy) return;
    let note = "";
    if (kind === "reject") {
      const input = window.prompt("原因（可选，留入版本留痕）：");
      if (input === null) return; // 取消——不动作
      note = input;
    }
    setActionBusy(item.memory_id + kind);
    setMsg(null);
    void (async () => {
      try {
        const v = kind === "accept"
          ? await api.acceptMemoryItem(item.memory_id)
          : await api.rejectMemoryItem(item.memory_id, note || undefined);
        setMsg(kind === "accept" ? `已接受（${v.title}）` : `已终结（${v.title}）`);
        setRefreshTick((t) => t + 1);
      } catch (e) {
        setMsg(e instanceof Error ? e.message : String(e));
      } finally {
        setActionBusy(null);
      }
    })();
  };

  // 修订 / 替代提交。
  const submitEdit = () => {
    if (!editing || actionBusy) return;
    setMsg(null);
    setActionBusy(editing.item.memory_id + editing.mode);
    void (async () => {
      try {
        if (editing.mode === "revise") {
          await api.reviseMemoryItem(editing.item.memory_id, {
            expected_version: editing.item.version,
            title: editForm.title.trim(),
            content: editForm.content,
            ...(editForm.valid_from !== editing.item.valid_from ? { valid_from: editForm.valid_from } : {}),
            ...(editForm.valid_to !== editing.item.valid_to ? { valid_to: editForm.valid_to } : {}),
            ...(editForm.sensitivity !== editing.item.sensitivity ? { sensitivity: editForm.sensitivity } : {}),
            ...(editForm.note ? { note: editForm.note } : {}),
          });
          setMsg("已修订（版本 +1）");
        } else {
          await api.supersedeMemoryItem(editing.item.memory_id, {
            expected_version: editing.item.version,
            ...(editForm.note ? { note: editForm.note } : {}),
            replacement: {
              namespace: editing.item.namespace,
              scope_id: editing.item.scope_id,
              kind: editing.item.kind,
              title: editForm.title.trim(),
              content: editForm.content,
              observed_at: nowRFC3339(),
              // 一审 #5：replacement 恒携带敏感度（预填原值——不默认降级）。
              sensitivity: editForm.sensitivity,
              ...(editForm.valid_from ? { valid_from: editForm.valid_from } : {}),
              ...(editForm.valid_to ? { valid_to: editForm.valid_to } : {}),
              source_refs: editing.item.source_refs ?? [],
            },
          });
          setMsg("已替代（旧条目终结，新条目入链）");
        }
        setEditing(null);
        setRefreshTick((t) => t + 1);
      } catch (e) {
        setMsg(e instanceof Error ? e.message : String(e));
      } finally {
        setActionBusy(null);
      }
    })();
  };

  const openEdit = (mode: "revise" | "supersede", item: MemoryItemView) => {
    setEditing({ mode, item });
    setEditForm({
      title: item.title,
      content: item.content,
      valid_from: item.valid_from ?? "",
      valid_to: item.valid_to ?? "",
      note: "",
      // 一审 #5：替代/修订预填原敏感度——replacement 恒携带（不默认降级
      // normal）；表单内可显式改。
      sensitivity: item.sensitivity === "sensitive" ? "sensitive" : "normal",
    });
  };

  const nsMeta = NAMESPACES.find((n) => n.key === ns)!;

  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-4 px-4 py-6">
      <header className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-medium">记忆</h1>
          <p className="mt-0.5 text-xs text-faint">
            分域 MemoryItem（RFC-0018 §7 / ADR-0037；策展与时效 ADR-0038，0.7.1）：接受、修订、否决与替代——版本留痕、过期不默认参与。
          </p>
        </div>
        <button type="button" onClick={() => setFormOpen((o) => !o)} className={btnCls}>
          {formOpen ? "收起" : "写入记忆"}
        </button>
      </header>

      {msg && <p className="rounded-lg bg-surface-2 px-3 py-2 text-xs">{msg}</p>}

      {formOpen && (
        <section className="flex flex-col gap-2 rounded-xl border border-line bg-surface-1 p-3">
          <div className="grid grid-cols-2 gap-2">
            <label className="flex flex-col gap-1 text-xs text-faint">
              命名空间
              <select className={inputCls} value={form.namespace} onChange={(e) => setForm((f) => ({ ...f, namespace: e.target.value as Namespace }))}>
                {NAMESPACES.map((n) => (
                  <option key={n.key} value={n.key}>{n.label}</option>
                ))}
              </select>
            </label>
            <label className="flex flex-col gap-1 text-xs text-faint">
              归属锚（scope_id）
              <input
                className={inputCls}
                value={form.namespace === "workspace" ? "default" : form.scope_id}
                disabled={form.namespace === "workspace"}
                placeholder={NS_SCOPE_PLACEHOLDER[form.namespace]}
                onChange={(e) => setForm((f) => ({ ...f, scope_id: e.target.value }))}
              />
            </label>
          </div>
          <div className="grid grid-cols-3 gap-2">
            <label className="flex flex-col gap-1 text-xs text-faint">
              类型（与权限域正交）
              <select className={inputCls} value={form.kind} onChange={(e) => setForm((f) => ({ ...f, kind: e.target.value as Kind }))}>
                {KINDS.map((k) => (
                  <option key={k} value={k}>{KIND_LABEL[k]}（{k}）</option>
                ))}
              </select>
            </label>
            <label className="flex flex-col gap-1 text-xs text-faint">
              敏感度（Bundle 脱敏消费）
              <select className={inputCls} value={form.sensitivity} onChange={(e) => setForm((f) => ({ ...f, sensitivity: e.target.value as "normal" | "sensitive" }))}>
                <option value="normal">normal</option>
                <option value="sensitive">sensitive</option>
              </select>
            </label>
            <label className="flex flex-col gap-1 text-xs text-faint">
              写入口径
              <select className={inputCls} value={form.status} onChange={(e) => setForm((f) => ({ ...f, status: e.target.value as "active" | "proposed" }))}>
                <option value="active">直接生效（active）</option>
                <option value="proposed">策展候选（proposed）</option>
              </select>
            </label>
          </div>
          <label className="flex flex-col gap-1 text-xs text-faint">
            标题（≤200 字）
            <input className={inputCls} value={form.title} onChange={(e) => setForm((f) => ({ ...f, title: e.target.value }))} />
          </label>
          <label className="flex flex-col gap-1 text-xs text-faint">
            正文（≤20000 字）
            <textarea className={`${inputCls} min-h-24`} value={form.content} onChange={(e) => setForm((f) => ({ ...f, content: e.target.value }))} />
          </label>
          <label className="flex flex-col gap-1 text-xs text-faint">
            来源引用（每行一条，≤16 条——事件/文档版本/显式操作）
            <textarea className={`${inputCls} min-h-12`} value={form.sources} onChange={(e) => setForm((f) => ({ ...f, sources: e.target.value }))} />
          </label>
          <div className="grid grid-cols-3 gap-2">
            <label className="flex flex-col gap-1 text-xs text-faint">
              何时知道（RFC3339）
              <input className={inputCls} value={form.observed_at} onChange={(e) => setForm((f) => ({ ...f, observed_at: e.target.value }))} />
            </label>
            <label className="flex flex-col gap-1 text-xs text-faint">
              有效期起（可选）
              <input className={inputCls} value={form.valid_from} onChange={(e) => setForm((f) => ({ ...f, valid_from: e.target.value }))} />
            </label>
            <label className="flex flex-col gap-1 text-xs text-faint">
              有效期止（可选）
              <input className={inputCls} value={form.valid_to} onChange={(e) => setForm((f) => ({ ...f, valid_to: e.target.value }))} />
            </label>
          </div>
          <button
            type="button"
            disabled={busy || !form.title.trim() || !form.content.trim() || !form.observed_at.trim() || (form.namespace !== "workspace" && !form.scope_id.trim())}
            onClick={submit}
            className={`${btnCls} self-start`}
          >
            {busy ? "写入中…" : "写入"}
          </button>
        </section>
      )}

      {editing && (
        <section className="flex flex-col gap-2 rounded-xl border border-line bg-surface-1 p-3">
          <p className="text-xs text-faint">
            {editing.mode === "revise" ? `修订：${editing.item.title}（v${editing.item.version} → v${editing.item.version + 1}，留痕入版本链）` : `替代：${editing.item.title}（旧条目终结，新条目 v1 入 supersedes 链）`}
          </p>
          <label className="flex flex-col gap-1 text-xs text-faint">
            标题
            <input className={inputCls} value={editForm.title} onChange={(e) => setEditForm((f) => ({ ...f, title: e.target.value }))} />
          </label>
          <label className="flex flex-col gap-1 text-xs text-faint">
            正文
            <textarea className={`${inputCls} min-h-24`} value={editForm.content} onChange={(e) => setEditForm((f) => ({ ...f, content: e.target.value }))} />
          </label>
          <div className="grid grid-cols-4 gap-2">
            <label className="flex flex-col gap-1 text-xs text-faint">
              有效期起（修订可改/空=清除）
              <input className={inputCls} value={editForm.valid_from} onChange={(e) => setEditForm((f) => ({ ...f, valid_from: e.target.value }))} />
            </label>
            <label className="flex flex-col gap-1 text-xs text-faint">
              有效期止
              <input className={inputCls} value={editForm.valid_to} onChange={(e) => setEditForm((f) => ({ ...f, valid_to: e.target.value }))} />
            </label>
            <label className="flex flex-col gap-1 text-xs text-faint">
              敏感度（预填原值——一审 #5 不默认降级）
              <select className={inputCls} value={editForm.sensitivity} onChange={(e) => setEditForm((f) => ({ ...f, sensitivity: e.target.value as "normal" | "sensitive" }))}>
                <option value="normal">normal</option>
                <option value="sensitive">sensitive</option>
              </select>
            </label>
            <label className="flex flex-col gap-1 text-xs text-faint">
              说明（留入版本留痕）
              <input className={inputCls} value={editForm.note} onChange={(e) => setEditForm((f) => ({ ...f, note: e.target.value }))} />
            </label>
          </div>
          <div className="flex gap-2">
            <button type="button" disabled={!editForm.title.trim() || !editForm.content.trim() || actionBusy !== null} onClick={submitEdit} className={btnCls}>
              {editing.mode === "revise" ? "提交修订" : "提交替代"}
            </button>
            <button type="button" onClick={() => setEditing(null)} className={btnGhostCls + " self-center"}>
              取消
            </button>
          </div>
        </section>
      )}

      <nav className="flex flex-wrap gap-1.5" aria-label="命名空间">
        {NAMESPACES.map((n) => (
          <button
            key={n.key}
            type="button"
            onClick={() => setNs(n.key)}
            className={`rounded-lg px-2.5 py-1 text-sm transition-colors ${ns === n.key ? "bg-surface-3" : "bg-surface-1 hover:bg-surface-2"}`}
          >
            {n.label}
          </button>
        ))}
      </nav>
      <p className="text-xs text-faint">{nsMeta.hint}</p>
      <div className="flex flex-wrap items-center gap-2">
        <input
          className={`${inputCls} flex-1`}
          value={scopeFilter}
          placeholder={`按归属锚过滤（${NS_SCOPE_PLACEHOLDER[ns]}）——留空列全部`}
          onChange={(e) => setScopeFilter(e.target.value)}
        />
        <select className={`${inputCls} w-40`} value={statusFilter} onChange={(e) => setStatusFilter(e.target.value as StatusFilter)}>
          <option value="active">现行（active）</option>
          <option value="proposed">候选（proposed）</option>
          <option value="superseded">历史（superseded）</option>
        </select>
        <label className="flex items-center gap-1.5 text-xs text-faint">
          <input type="checkbox" checked={includeExpired} onChange={(e) => setIncludeExpired(e.target.checked)} />
          含已过期
        </label>
      </div>

      {listErr && <p className="rounded-lg bg-surface-2 px-3 py-2 text-xs text-red-400">{listErr}</p>}
      {items === null && !listErr && <p className="text-xs text-faint">加载中…</p>}
      {items !== null && items.length === 0 && (
        <p className="rounded-lg bg-surface-1 px-3 py-6 text-center text-xs text-faint">
          {statusFilter === "active" && "该命名空间暂无现行记忆——写入显式添加，或切「候选」看待接受条目。"}
          {statusFilter === "proposed" && "暂无待策展候选。"}
          {statusFilter === "superseded" && "暂无已终结条目（否决/撤回/被替代的历史可在此解释）。"}
        </p>
      )}
      <ul className="flex flex-col gap-2">
        {items?.map((it) => (
          <li key={it.memory_id} className="rounded-xl border border-line bg-surface-1 p-3">
            <button type="button" className="flex w-full items-start justify-between gap-2 text-left" onClick={() => setExpanded((cur) => (cur === it.memory_id ? null : it.memory_id))}>
              <span className="flex flex-col gap-0.5">
                <span className="text-sm font-medium">{it.title}</span>
                <span className="text-xs text-faint">
                  {it.scope_id} · {KIND_LABEL[it.kind]}（{it.kind}） · 知于 {it.observed_at}
                  {(it.valid_from || it.valid_to) && ` · 有效 ${it.valid_from || "…"} → ${it.valid_to || "…"}`}
                </span>
              </span>
              <span className="flex shrink-0 items-center gap-1.5">
                {it.sensitivity === "sensitive" && <span className="rounded bg-amber-500/15 px-1.5 py-0.5 text-[10px] text-amber-500">敏感</span>}
                {(it.supersedes ?? []).length > 0 && <span className="rounded bg-surface-3 px-1.5 py-0.5 text-[10px] text-faint">替代 {(it.supersedes ?? []).length} 条</span>}
                <span className={`rounded px-1.5 py-0.5 text-[10px] ${it.status === "active" ? "bg-emerald-500/10 text-emerald-500" : it.status === "proposed" ? "bg-sky-500/10 text-sky-500" : "bg-surface-3 text-faint"}`}>{it.status}</span>
                <span className="text-[10px] text-faint">{(it.source_refs ?? []).length} 来源 · v{it.version}</span>
              </span>
            </button>
            {it.status !== "superseded" && (
              <div className="mt-2 flex flex-wrap gap-1.5">
                {it.status === "proposed" && (
                  <button type="button" className={btnGhostCls + " !text-emerald-500"} disabled={actionBusy !== null} onClick={() => act(it, "accept")}>接受</button>
                )}
                <button type="button" className={btnGhostCls} disabled={actionBusy !== null} onClick={() => act(it, "reject")}>
                  {it.status === "proposed" ? "否决" : "撤回"}
                </button>
                <button type="button" className={btnGhostCls} disabled={actionBusy !== null} onClick={() => openEdit("revise", it)}>修订</button>
                <button type="button" className={btnGhostCls} disabled={actionBusy !== null} onClick={() => openEdit("supersede", it)}>替代</button>
              </div>
            )}
            {expanded === it.memory_id && (
              <div className="mt-2 flex flex-col gap-1.5 border-t border-line pt-2">
                <p className="whitespace-pre-wrap text-sm">{it.content}</p>
                {(it.source_refs ?? []).length > 0 && (
                  <ul className="flex flex-col gap-0.5 text-xs text-faint">
                    {(it.source_refs ?? []).map((ref, i) => (
                      <li key={i}>· {ref}</li>
                    ))}
                  </ul>
                )}
                {(it.supersedes ?? []).length > 0 && (
                  <p className="text-[10px] text-faint">替代链：{(it.supersedes ?? []).join("、")}</p>
                )}
                {(it.edit_history ?? []).length > 0 && (
                  <div className="flex flex-col gap-0.5">
                    <p className="text-[10px] text-faint">版本留痕（最新在后）：</p>
                    {(it.edit_history ?? []).map((e, i) => (
                      <p key={i} className="text-[10px] text-faint">
                        v{e.version} {CAUSE_LABEL[e.cause] ?? e.cause}{e.note ? `——${e.note}` : ""} · {e.occurred_at}
                      </p>
                    ))}
                  </div>
                )}
                <p className="text-[10px] text-faint">
                  {it.memory_id} · v{it.version} · 置信 {it.confidence} · 建于 {it.created_at}
                </p>
              </div>
            )}
          </li>
        ))}
      </ul>
      {cursor && (
        <button type="button" disabled={loadingMore} onClick={loadMore} className="self-center rounded-lg bg-surface-2 px-3 py-1.5 text-sm hover:bg-surface-3 disabled:opacity-50">
          {loadingMore ? "加载中…" : "加载更多"}
        </button>
      )}
    </div>
  );
}
