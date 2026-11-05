// 分域记忆页（0.7.0 / ADR-0037 / RFC-0018 §7.1/§7.2）：五命名空间 tab +
// scope 过滤 + 记忆卡列表（创建序降序 + 游标加载更多）+ 显式写入表单。
// 房间收束胶囊（RFC-0007）仍是房间事件流事实——本页是分域 MemoryItem 的
// 独立读写面，不经胶囊复制。
import { useEffect, useRef, useState } from "react";
import { api, Schemas } from "../api/client";

type MemoryItemView = Schemas["MemoryItemView"];
type Namespace = "agent" | "room" | "workspace" | "mission" | "slot";
type Kind = "semantic" | "episodic" | "procedural";

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

function nowRFC3339(): string {
  return new Date().toISOString().replace(/\.\d{3}Z$/, "Z");
}

export function MemoryPage() {
  const [ns, setNs] = useState<Namespace>("agent");
  const [scopeFilter, setScopeFilter] = useState("");
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
  }));

  // 列表加载（tab/scope 变化重载；代次作废在途响应）。
  useEffect(() => {
    const cur = ++seq.current;
    setItems(null);
    setListErr(null);
    setCursor(null);
    api
      .listMemoryItems({ namespace: ns, ...(scopeFilter.trim() ? { scope_id: scopeFilter.trim() } : {}), limit: 50 })
      .then((v) => {
        if (seq.current !== cur) return;
        setItems(v.items);
        setCursor(v.next_cursor ?? null);
      })
      .catch((e: unknown) => {
        if (seq.current !== cur) return;
        setListErr(e instanceof Error ? e.message : String(e));
      });
  }, [ns, scopeFilter]);

  const loadMore = () => {
    if (!cursor) return;
    api
      .listMemoryItems({
        namespace: ns,
        ...(scopeFilter.trim() ? { scope_id: scopeFilter.trim() } : {}),
        limit: 50,
        before: cursor,
      })
      .then((v) => {
        setItems((prev) => [...(prev ?? []), ...v.items]);
        setCursor(v.next_cursor ?? null);
      })
      .catch((e: unknown) => setListErr(e instanceof Error ? e.message : String(e)));
  };

  // workspace 锚恒 default（表单内锁定）。
  useEffect(() => {
    setForm((f) => (f.namespace === "workspace" ? { ...f, scope_id: "default" } : f.scope_id === "default" ? { ...f, scope_id: "" } : f));
  }, [form.namespace]);

  const submit = () => {
    if (busy) return;
    setMsg(null);
    const splitLines = (s: string) => s.split("\n").map((l) => l.trim()).filter(Boolean);
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
        });
        setFormOpen(false);
        setForm((f) => ({ ...f, title: "", content: "", sources: "", observed_at: nowRFC3339(), valid_from: "", valid_to: "" }));
        setMsg("记忆已写入");
        const cur = ++seq.current;
        api
          .listMemoryItems({ namespace: form.namespace, ...(form.scope_id.trim() ? { scope_id: form.scope_id.trim() } : {}), limit: 50 })
          .then((v) => {
            if (seq.current !== cur) return;
            setItems(v.items);
            setCursor(v.next_cursor ?? null);
          })
          .catch(() => {/* 列表刷新失败不打断写入成功反馈 */});
      } catch (e) {
        setMsg(e instanceof Error ? e.message : String(e));
      } finally {
        setBusy(false);
      }
    })();
  };

  const nsMeta = NAMESPACES.find((n) => n.key === ns)!;
  const inputCls = "w-full rounded-lg border border-line bg-surface-2 px-2.5 py-1.5 text-sm outline-none focus:border-accent";

  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-4 px-4 py-6">
      <header className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-medium">记忆</h1>
          <p className="mt-0.5 text-xs text-faint">
            分域 MemoryItem（RFC-0018 §7 / ADR-0037，0.7.0）：每条记忆归属恰一命名空间并锚定归属对象；显式写入面——策展与召回随 0.7.1/0.7.2。
          </p>
        </div>
        <button
          type="button"
          onClick={() => setFormOpen((o) => !o)}
          className="rounded-lg bg-accent px-3 py-1.5 text-sm text-white transition-opacity hover:opacity-90"
        >
          {formOpen ? "收起" : "写入记忆"}
        </button>
      </header>

      {msg && <p className="rounded-lg bg-surface-2 px-3 py-2 text-xs">{msg}</p>}

      {formOpen && (
        <section className="flex flex-col gap-2 rounded-xl border border-line bg-surface-1 p-3">
          <div className="grid grid-cols-2 gap-2">
            <label className="flex flex-col gap-1 text-xs text-faint">
              命名空间
              <select
                className={inputCls}
                value={form.namespace}
                onChange={(e) => setForm((f) => ({ ...f, namespace: e.target.value as Namespace }))}
              >
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
          <div className="grid grid-cols-2 gap-2">
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
              <select
                className={inputCls}
                value={form.sensitivity}
                onChange={(e) => setForm((f) => ({ ...f, sensitivity: e.target.value as "normal" | "sensitive" }))}
              >
                <option value="normal">normal</option>
                <option value="sensitive">sensitive</option>
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
            className="self-start rounded-lg bg-accent px-3 py-1.5 text-sm text-white transition-opacity hover:opacity-90 disabled:opacity-50"
          >
            {busy ? "写入中…" : "写入"}
          </button>
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
      <input
        className={inputCls}
        value={scopeFilter}
        placeholder={`按归属锚过滤（${NS_SCOPE_PLACEHOLDER[ns]}）——留空列全部`}
        onChange={(e) => setScopeFilter(e.target.value)}
      />

      {listErr && <p className="rounded-lg bg-surface-2 px-3 py-2 text-xs text-red-400">{listErr}</p>}
      {items === null && !listErr && <p className="text-xs text-faint">加载中…</p>}
      {items !== null && items.length === 0 && (
        <p className="rounded-lg bg-surface-1 px-3 py-6 text-center text-xs text-faint">
          该命名空间暂无记忆——右上「写入记忆」显式添加（模型抽取与策展随 0.7.1）。
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
                <span className="rounded bg-surface-3 px-1.5 py-0.5 text-[10px] text-faint">{it.status}</span>
                <span className="text-[10px] text-faint">{it.source_refs.length} 来源</span>
              </span>
            </button>
            {expanded === it.memory_id && (
              <div className="mt-2 flex flex-col gap-1.5 border-t border-line pt-2">
                <p className="whitespace-pre-wrap text-sm">{it.content}</p>
                {it.source_refs.length > 0 && (
                  <ul className="flex flex-col gap-0.5 text-xs text-faint">
                    {it.source_refs.map((ref, i) => (
                      <li key={i}>· {ref}</li>
                    ))}
                  </ul>
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
        <button type="button" onClick={loadMore} className="self-center rounded-lg bg-surface-2 px-3 py-1.5 text-sm hover:bg-surface-3">
          加载更多
        </button>
      )}
    </div>
  );
}
