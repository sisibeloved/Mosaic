// 表格编辑器（RFC-0017 M1，DocEditorPage 工作台纪律的表格版，自绘网格）：
//  工作台模型：sheets（本地 LocalSheet）+ versionRef 水位——编辑动作即产 op
//    （语义精确，不做 diff），防抖 1.2s 聚合 commit_sheet_revision；
//  409 重定基：ApiError.docConflict 携带当前态 → 重拉 range → 在新态上重放
//    未提交 ops → 重试一次；再撞 → conflict 横幅（本地编辑保留）；
//  直播：useDoc 订 doc 频道（sheet 修订驱动快照重取）；无未提交变更静默刷新
//    range，有则只亮"有新版本"；
//  公式权威：派生值永远来自服务端 range 回读（提交成功后刷新当前表），
//    本地编辑中的公式格显示原式（斜体灰）直到回读定稿；
//  虚拟滚动：行 ×列 双向视口窗口（行高 26 / 列宽 96 固定，自定义宽待后续）。
import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { SheetTransferDialog } from "../components/SheetTransferDialog";
import { api, ApiError, type SheetCellView, type SheetCellWrite, type SheetOp } from "../api/client";
import { useDoc } from "../api/doc";
import {
  applySheetOpsLocal, cellDisplay, cellKey, cellRaw, clearCell, colName, parseCellInput,
  type LocalCell, type LocalSheet,
} from "../lib/sheetops";
import { relativeTime } from "../lib/ui";

const COMMIT_DEBOUNCE_MS = 1200;
const OPS_BATCH = 64;
const ROW_H = 26;
const COL_W = 96;
const HEAD_W = 48;
const OVERSCAN = 8;

type SaveState = "idle" | "dirty" | "saving" | "saved" | "conflict" | "error";

export function SheetEditorPage({ docId }: { docId: string }) {
  const { doc, notFound, deleted, error: loadError } = useDoc(docId);
  const [sheets, setSheets] = useState<LocalSheet[] | null>(null);
  const [activeIdx, setActiveIdx] = useState(0);
  const [sel, setSel] = useState<{ r: number; c: number }>({ r: 1, c: 1 });
  const [editing, setEditing] = useState<{ r: number; c: number; draft: string } | null>(null);
  const [saveState, setSaveState] = useState<SaveState>("idle");
  const [saveError, setSaveError] = useState<string | null>(null);
  const [hasUpdate, setHasUpdate] = useState(false);
  const [transferOpen, setTransferOpen] = useState(false);
  const [renaming, setRenaming] = useState<{ idx: number; draft: string } | null>(null);

  const versionRef = useRef<number>(0);
  const sheetsRef = useRef<LocalSheet[] | null>(null);
  const pendingOpsRef = useRef<SheetOp[]>([]);
  const dirtyRef = useRef(false);
  const committingRef = useRef(false);
  const dirtyAgainRef = useRef(false);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const loadedVersionRef = useRef(0);
  const gridRef = useRef<HTMLDivElement | null>(null);
  const editInputRef = useRef<HTMLInputElement | null>(null);

  const active = sheets?.[activeIdx] ?? null;

  const setWorking = useCallback((next: LocalSheet[]) => {
    sheetsRef.current = next;
    setSheets(next);
  }, []);

  /** 全表 range 拉取（初载与静默刷新共用）。 */
  const loadSheets = useCallback(
    async (docVersion: number) => {
      const d = await api.getDoc(docId);
      const metas = d.sheets ?? [];
      const loaded: LocalSheet[] = [];
      for (const m of metas) {
        const view = await api.getDocSheetRange(docId, m.sheet_id);
        const cells = new Map<string, LocalCell>();
        for (const c of view.cells ?? []) {
          cells.set(cellKey(c.r, c.c), viewCellToLocal(c));
        }
        loaded.push({
          sheetID: m.sheet_id, name: m.name, rows: m.rows, cols: m.cols,
          cells, colWidths: new Map(), rowHeights: new Map(),
        });
      }
      loadedVersionRef.current = docVersion;
      versionRef.current = d.version;
      pendingOpsRef.current = [];
      dirtyRef.current = false;
      setWorking(loaded);
    },
    [docId, setWorking],
  );

  // 初载 / 服务器态合并（无未提交变更 → 静默刷新；有 → 亮"有新版本"）
  useEffect(() => {
    if (!doc || doc.format !== "spreadsheet") return;
    if (doc.version <= loadedVersionRef.current) return;
    if (dirtyRef.current || committingRef.current) {
      setHasUpdate(true);
      return;
    }
    void loadSheets(doc.version).catch(() => undefined);
  }, [doc, loadSheets]);

  /** 记录一个编辑动作：本地乐观应用 + op 入待提交批 + 防抖。 */
  const pushOps = useCallback(
    (ops: SheetOp[]) => {
      if (ops.length === 0) return;
      const cur = sheetsRef.current;
      if (!cur) return;
      const next = cur.map((s, i) => (i === activeIdx ? applySheetOpsLocal(s, ops) : s));
      pendingOpsRef.current = [...pendingOpsRef.current, ...ops];
      dirtyRef.current = true;
      setHasUpdate(false);
      setSaveState("dirty");
      setWorking(next);
      if (debounceRef.current) clearTimeout(debounceRef.current);
      debounceRef.current = setTimeout(() => {
        debounceRef.current = null;
        void commitNow();
      }, COMMIT_DEBOUNCE_MS);
    },
    // commitNow 经 ref 间接调用避免依赖环
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [activeIdx, setWorking],
  );

  const commitNowRef = useRef<() => Promise<void>>(async () => {});
  const commitNow = useCallback(() => commitNowRef.current(), []);

  /** 提交：批 ≤64 顺序；409 重定基重试一次（DocEditorPage 同纪律）。 */
  const doCommit = useCallback(async () => {
    if (committingRef.current) {
      dirtyAgainRef.current = true;
      return;
    }
    if (!dirtyRef.current || pendingOpsRef.current.length === 0) return;
    committingRef.current = true;
    setSaveState("saving");
    setSaveError(null);
    let retried = false;
    try {
      for (;;) {
        const ops = pendingOpsRef.current.slice(0, OPS_BATCH);
        if (ops.length === 0) break;
        try {
          await api.commitSheetRevision(docId, versionRef.current, ops);
          versionRef.current += 1;
          pendingOpsRef.current = pendingOpsRef.current.slice(ops.length);
        } catch (e) {
          if (e instanceof ApiError && e.docConflict && !retried) {
            retried = true;
            // 重定基：暂存未提交 ops → 拉服务端态 → 在新态上本地重放 → 续批重试
            const pending = [...pendingOpsRef.current];
            await loadSheets(e.docConflict.currentVersion ?? versionRef.current + 1);
            const cur = sheetsRef.current;
            if (cur && pending.length > 0) {
              const next = cur.map((s, i) => (i === activeIdx ? applySheetOpsLocal(s, pending) : s));
              pendingOpsRef.current = pending;
              dirtyRef.current = true;
              setWorking(next);
            }
            continue;
          }
          throw e;
        }
      }
      dirtyRef.current = false;
      setSaveState("saved");
      // 提交落地 → 刷新当前表 range（服务端派生值权威定稿）
      const d = await api.getDoc(docId).catch(() => null);
      if (d && !dirtyRef.current) {
        await loadSheets(d.version).catch(() => undefined);
      }
      if (dirtyAgainRef.current) {
        dirtyAgainRef.current = false;
        void commitNow();
      }
    } catch (e) {
      setSaveState(e instanceof ApiError && e.docConflict ? "conflict" : "error");
      setSaveError(e instanceof Error ? e.message : String(e));
    } finally {
      committingRef.current = false;
    }
  }, [docId, activeIdx, loadSheets, setWorking]);

  useEffect(() => {
    commitNowRef.current = doCommit;
  }, [doCommit]);

  useEffect(() => {
    return () => {
      if (debounceRef.current) clearTimeout(debounceRef.current);
    };
  }, []);

  // ---- 编辑动作 ----

  const commitEdit = useCallback(
    (move: "down" | "right" | null = "down") => {
      if (!editing || !active) return;
      const parsed = parseCellInput(editing.draft);
      const { r, c } = editing;
      setEditing(null);
      const cur = active.cells.get(cellKey(r, c));
      const unchanged = cur?.type === parsed.type && cellRaw(cur) === editing.draft;
      if (!unchanged && !(parsed.type === "null" && !cur)) {
        const write = {
          r, c, type: parsed.type,
          ...(parsed.type === "text" ? { text: parsed.text ?? "" } : {}),
          ...(parsed.type === "number" ? { number: parsed.number } : {}),
          ...(parsed.type === "bool" ? { bool: parsed.bool } : {}),
          ...(parsed.type === "formula" ? { formula: parsed.formula } : {}),
        } as unknown as SheetCellWrite;
        pushOps([{ op: "set_cells", sheet_id: active.sheetID, cells: [write] }]);
      }
      if (move === "down") setSel({ r: Math.min(r + 1, active.rows), c });
      if (move === "right") setSel({ r, c: Math.min(c + 1, active.cols) });
    },
    [editing, active, pushOps],
  );

  const startEdit = useCallback(
    (initial?: string) => {
      if (!active || doc?.status === "archived") return;
      setEditing({ r: sel.r, c: sel.c, draft: initial ?? cellRaw(active.cells.get(cellKey(sel.r, sel.c))) });
    },
    [active, sel, doc],
  );

  const onKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (!active) return;
      const { r, c } = sel;
      if (editing) return; // 编辑态按键由输入框自持
      const move = (dr: number, dc: number) => {
        e.preventDefault();
        setSel({ r: Math.min(Math.max(r + dr, 1), active.rows), c: Math.min(Math.max(c + dc, 1), active.cols) });
      };
      switch (e.key) {
        case "ArrowUp": return move(-1, 0);
        case "ArrowDown": return move(1, 0);
        case "ArrowLeft": return move(0, -1);
        case "ArrowRight": return move(0, 1);
        case "Tab": e.preventDefault(); return setSel({ r, c: Math.min(c + 1, active.cols) });
        case "Enter": return startEdit();
        case "F2": e.preventDefault(); return startEdit();
        case "Delete":
        case "Backspace": {
          e.preventDefault();
          pushOps([{ op: "set_cells", sheet_id: active.sheetID, cells: [clearCell(r, c)] }]);
          return;
        }
        default:
          if (e.key.length === 1 && !e.ctrlKey && !e.metaKey) {
            e.preventDefault();
            startEdit(e.key);
          }
      }
    },
    [active, sel, editing, startEdit, pushOps],
  );

  // 选中格滚动入视（粗略：仅行向）
  useEffect(() => {
    const grid = gridRef.current;
    if (!grid) return;
    const top = (sel.r - 1) * ROW_H;
    const bottom = top + ROW_H;
    if (top < grid.scrollTop) grid.scrollTop = top;
    else if (bottom > grid.scrollTop + grid.clientHeight) grid.scrollTop = bottom - grid.clientHeight;
    const left = (sel.c - 1) * COL_W;
    const right = left + COL_W;
    if (left < grid.scrollLeft) grid.scrollLeft = left;
    else if (right > grid.scrollLeft + grid.clientWidth) grid.scrollLeft = right - grid.clientWidth;
  }, [sel]);

  // ---- 视口窗口 ----
  const [view, setView] = useState({ rowStart: 1, rowEnd: 40, colStart: 1, colEnd: 12 });
  const onScroll = useCallback(() => {
    const grid = gridRef.current;
    if (!grid || !active) return;
    const rowStart = Math.max(1, Math.floor(grid.scrollTop / ROW_H) - OVERSCAN + 1);
    const rowEnd = Math.min(active.rows, rowStart + Math.ceil(grid.clientHeight / ROW_H) + OVERSCAN * 2);
    const colStart = Math.max(1, Math.floor(grid.scrollLeft / COL_W) - OVERSCAN + 1);
    const colEnd = Math.min(active.cols, colStart + Math.ceil(grid.clientWidth / COL_W) + OVERSCAN * 2);
    setView({ rowStart, rowEnd, colStart, colEnd });
  }, [active]);

  useEffect(() => {
    onScroll();
  }, [onScroll, sheets]);

  // ---- 结构与工作表动作 ----

  const structuralOp = useCallback(
    (op: SheetOp) => {
      pushOps([op]);
      if (op.op === "insert_rows") setSel((s) => ({ ...s, r: s.r + 1 }));
      if (op.op === "insert_columns") setSel((s) => ({ ...s, c: s.c + 1 }));
    },
    [pushOps],
  );

  const addSheet = useCallback(() => {
    const name = `Sheet${(sheets?.length ?? 0) + 1}`;
    pushOps([{ op: "add_sheet", name }]);
    setActiveIdx(sheets?.length ?? 0);
  }, [sheets, pushOps]);

  const deleteSheet = useCallback(
    (idx: number) => {
      const target = sheets?.[idx];
      if (!target || (sheets?.length ?? 0) <= 1) return;
      if (!window.confirm(`删除工作表「${target.name}」？此操作将随保存提交。`)) return;
      pushOps([{ op: "delete_sheet", sheet_id: target.sheetID }]);
      setActiveIdx(0);
    },
    [sheets, pushOps],
  );

  const toggleStyle = useCallback(
    (kind: "bold" | "italic") => {
      if (!active) return;
      const cur = active.cells.get(cellKey(sel.r, sel.c));
      const next = !(kind === "bold" ? cur?.bold : cur?.italic);
      pushOps([{ op: "set_style", sheet_id: active.sheetID, range: { r1: sel.r, c1: sel.c, r2: sel.r, c2: sel.c }, style: { [kind]: next } }]);
    },
    [active, sel, pushOps],
  );

  // ---- 渲染 ----

  const busy = saveState === "saving";
  const saveBadge =
    saveState === "dirty" ? "保存中…" :
    saveState === "saving" ? "保存中…" :
    saveState === "conflict" ? "版本冲突" :
    saveState === "error" ? "保存失败" :
    saveState === "saved" ? "已保存" : "";

  if (notFound) {
    return (
      <div className="p-8 text-sm text-faint">
        文档不存在或已删除。<Link to="/docs" className="text-accent">返回文档列表</Link>
      </div>
    );
  }
  if (loadError || !doc || !sheets) {
    return <div className="p-8 text-sm text-faint">{loadError ?? "加载中…"}</div>;
  }
  if (deleted) {
    return (
      <div className="p-8 text-sm text-faint">
        文档已在别处删除。<Link to="/docs" className="text-accent">返回文档列表</Link>
      </div>
    );
  }

  const totalRows = active?.rows ?? 40;
  const totalCols = active?.cols ?? 8;
  const selCell = active?.cells.get(cellKey(sel.r, sel.c));

  return (
    <div className="flex h-full flex-col">
      {/* 顶栏 */}
      <div className="flex items-center gap-3 border-b border-border px-4 py-2">
        <Link to="/docs" className="rounded-lg px-2 py-1 text-xs text-dim hover:bg-surface-2 hover:text-text">← 文档</Link>
        <span className="text-sm font-medium">{doc.title}</span>
        <span className="text-[10px] text-faint">表格 · v{doc.version}</span>
        {saveBadge && <span className={`text-[10px] ${saveState === "conflict" || saveState === "error" ? "text-danger" : "text-faint"}`}>{saveBadge}</span>}
        {hasUpdate && <span className="rounded bg-accent-soft px-1.5 py-0.5 text-[10px] text-accent">有新版本</span>}
        <div className="flex-1" />
        <button
          type="button"
          onClick={() => setTransferOpen(true)}
          className="rounded-lg border border-border px-2 py-1 text-xs text-dim hover:bg-surface-2 hover:text-text"
        >
          导出到云
        </button>
        <a
          href={api.exportDocUrl(docId, active?.sheetID)}
          className="rounded-lg border border-border px-2 py-1 text-xs text-dim hover:bg-surface-2 hover:text-text"
          download
        >
          导出 CSV
        </a>
      </div>
      {transferOpen && (
        <SheetTransferDialog mode="export" docID={docId} onClose={() => setTransferOpen(false)} />
      )}

      {(saveState === "conflict" || saveError) && (
        <div className="flex items-center gap-3 border-b border-border bg-surface-2 px-4 py-1.5 text-xs text-dim">
          <span>{saveState === "conflict" ? "远端有更新提交，本地编辑保留中——重试保存以合并。" : `保存失败：${saveError}`}</span>
          <button
            type="button"
            className="rounded-lg border border-accent bg-accent-soft px-2 py-0.5 text-xs text-accent"
            onClick={() => void commitNow()}
            disabled={busy}
          >
            重试保存
          </button>
        </div>
      )}
      {doc.status === "archived" && (
        <div className="border-b border-border bg-surface-2 px-4 py-1.5 text-xs text-faint">已归档（只读）。</div>
      )}

      {/* 工具条 */}
      <div className="flex items-center gap-1 border-b border-border px-4 py-1.5 text-xs">
        <ToolBtn label="B" bold active={selCell?.bold} disabled={!active} onClick={() => toggleStyle("bold")} title="加粗" />
        <ToolBtn label="I" italic active={selCell?.italic} disabled={!active} onClick={() => toggleStyle("italic")} title="斜体" />
        <span className="mx-1 h-4 w-px bg-border" />
        <ToolBtn label="↑ 行" disabled={!active} onClick={() => structuralOp({ op: "insert_rows", sheet_id: active!.sheetID, at: sel.r, count: 1 })} title="在上方插入行" />
        <ToolBtn label="↓ 删行" disabled={!active} onClick={() => structuralOp({ op: "delete_rows", sheet_id: active!.sheetID, at: sel.r, count: 1 })} title="删除当前行" />
        <ToolBtn label="← 列" disabled={!active} onClick={() => structuralOp({ op: "insert_columns", sheet_id: active!.sheetID, at: sel.c, count: 1 })} title="在左侧插入列" />
        <ToolBtn label="→ 删列" disabled={!active} onClick={() => structuralOp({ op: "delete_columns", sheet_id: active!.sheetID, at: sel.c, count: 1 })} title="删除当前列" />
        <div className="flex-1" />
        <span className="text-[10px] text-faint">
          {a1Label(sel)} · {active ? `${active.rows} 行 × ${active.cols} 列` : ""}
          {doc.updated_at ? ` · 更新于 ${relativeTime(doc.updated_at)}` : ""}
        </span>
      </div>

      {/* 公式栏 */}
      <div className="flex items-center gap-2 border-b border-border px-4 py-1.5">
        <span className="w-14 rounded bg-surface-3 px-2 py-1 text-center font-mono text-xs text-dim">{a1Label(sel)}</span>
        <span className="text-xs text-faint">fx</span>
        <input
          className="flex-1 rounded-lg border border-border bg-surface-2 px-2 py-1 font-mono text-xs text-text focus:border-accent focus:outline-none"
          value={editing ? editing.draft : cellRaw(selCell)}
          placeholder={doc.status === "archived" ? "已归档（只读）" : "输入值或 =公式（SUM/AVERAGE/MIN/MAX/COUNT/COUNTA/IF/ROUND）"}
          disabled={doc.status === "archived"}
          onChange={(e) => setEditing({ r: sel.r, c: sel.c, draft: e.target.value })}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              commitEdit("down");
              (e.target as HTMLInputElement).blur();
            }
            if (e.key === "Escape") {
              setEditing(null);
              (e.target as HTMLInputElement).blur();
            }
          }}
        />
      </div>

      {/* 网格（双向窗口化） */}
      <div
        ref={gridRef}
        className="relative flex-1 overflow-auto bg-surface font-mono text-xs outline-none"
        tabIndex={0}
        onKeyDown={onKeyDown}
        onScroll={onScroll}
      >
        {active && (
          <div className="relative" style={{ width: HEAD_W + totalCols * COL_W, height: 28 + totalRows * ROW_H }}>
            {/* 列头 */}
            <div className="sticky top-0 z-10 flex bg-surface-3" style={{ marginLeft: HEAD_W }}>
              {range(view.colStart, view.colEnd).map((c) => (
                <div
                  key={c}
                  className={`flex shrink-0 items-center justify-center border-b border-r border-border text-[10px] ${c === sel.c ? "bg-accent-soft text-accent" : "text-dim"}`}
                  style={{ width: COL_W, height: 28 }}
                >
                  {colName(c)}
                </div>
              ))}
            </div>
            {/* 行头 */}
            <div className="sticky left-0 z-10 float-left bg-surface-3" style={{ width: HEAD_W, marginTop: 28 }}>
              {range(view.rowStart, view.rowEnd).map((r) => (
                <div
                  key={r}
                  className={`flex shrink-0 items-center justify-center border-b border-r border-border text-[10px] ${r === sel.r ? "bg-accent-soft text-accent" : "text-dim"}`}
                  style={{ height: ROW_H }}
                >
                  {r}
                </div>
              ))}
            </div>
            {/* 单元格 */}
            <div className="absolute" style={{ left: HEAD_W, top: 28 }}>
              {range(view.rowStart, view.rowEnd).map((r) => (
                <div key={r} className="flex" style={{ height: ROW_H }}>
                  {range(view.colStart, view.colEnd).map((c) => {
                    const cell = active.cells.get(cellKey(r, c));
                    const isSel = r === sel.r && c === sel.c;
                    const isEditing = editing && editing.r === r && editing.c === c;
                    const display = cellDisplay(cell);
                    const pending = cell?.type === "formula" && !cell.derived;
                    return (
                      <div
                        key={c}
                        className={`flex shrink-0 items-center overflow-hidden border-b border-r border-border px-1.5 ${isSel ? "outline outline-2 -outline-offset-2 outline-accent" : ""} ${cell?.bold ? "font-bold" : ""} ${cell?.italic ? "italic" : ""} ${cell?.derived?.kind === "error" ? "text-danger" : "text-text"}`}
                        style={{ width: COL_W, height: ROW_H }}
                        onClick={() => {
                          if (editing) commitEdit(null);
                          setSel({ r, c });
                          gridRef.current?.focus();
                        }}
                        onDoubleClick={() => {
                          setSel({ r, c });
                          startEdit();
                        }}
                      >
                        {isEditing ? (
                          <input
                            ref={editInputRef}
                            className="h-full w-full bg-surface-2 px-0.5 text-xs outline-none"
                            value={editing!.draft}
                            autoFocus
                            onChange={(e) => setEditing({ r, c, draft: e.target.value })}
                            onBlur={() => commitEdit(null)}
                            onKeyDown={(e) => {
                              if (e.key === "Enter") {
                                e.preventDefault();
                                commitEdit("down");
                                gridRef.current?.focus();
                              }
                              if (e.key === "Tab") {
                                e.preventDefault();
                                commitEdit("right");
                                gridRef.current?.focus();
                              }
                              if (e.key === "Escape") {
                                e.preventDefault();
                                setEditing(null);
                                gridRef.current?.focus();
                              }
                            }}
                          />
                        ) : (
                          <span className={`truncate ${pending ? "italic text-dim" : ""}`} title={display}>{display}</span>
                        )}
                      </div>
                    );
                  })}
                </div>
              ))}
            </div>
          </div>
        )}
      </div>

      {/* 工作表页签 */}
      <div className="flex items-center gap-1 border-t border-border bg-surface px-3 py-1.5">
        {sheets.map((s, idx) => (
          <div key={s.sheetID} className="group relative">
            {renaming?.idx === idx ? (
              <input
                className="w-20 rounded border border-accent bg-surface-2 px-1.5 py-0.5 text-xs outline-none"
                value={renaming.draft}
                autoFocus
                onChange={(e) => setRenaming({ idx, draft: e.target.value })}
                onBlur={() => {
                  const name = renaming.draft.trim();
                  if (name && name !== s.name) pushOps([{ op: "rename_sheet", sheet_id: s.sheetID, name }]);
                  setRenaming(null);
                }}
                onKeyDown={(e) => {
                  if (e.key === "Enter") (e.target as HTMLInputElement).blur();
                  if (e.key === "Escape") setRenaming(null);
                }}
              />
            ) : (
              <button
                type="button"
                className={`rounded-lg px-2.5 py-1 text-xs ${idx === activeIdx ? "bg-accent-soft text-accent" : "text-dim hover:bg-surface-2 hover:text-text"}`}
                onClick={() => {
                  setActiveIdx(idx);
                  setSel({ r: 1, c: 1 });
                }}
                onDoubleClick={() => setRenaming({ idx, draft: s.name })}
                title="双击重命名"
              >
                {s.name}
                {sheets.length > 1 && (
                  <span
                    className="ml-1 hidden text-[10px] text-faint group-hover:inline hover:text-danger"
                    onClick={(e) => {
                      e.stopPropagation();
                      deleteSheet(idx);
                    }}
                  >
                    ✕
                  </span>
                )}
              </button>
            )}
          </div>
        ))}
        <button
          type="button"
          className="rounded-lg px-2 py-1 text-xs text-faint hover:bg-surface-2 hover:text-text"
          onClick={addSheet}
          disabled={doc.status === "archived"}
          title="新增工作表"
        >
          ＋
        </button>
      </div>
    </div>
  );
}

function range(a: number, b: number): number[] {
  const out: number[] = [];
  for (let i = a; i <= b; i++) out.push(i);
  return out;
}

function a1Label(sel: { r: number; c: number }): string {
  return `${colName(sel.c)}${sel.r}`;
}

function ToolBtn({ label, bold, italic, active, disabled, onClick, title }: {
  label: string; bold?: boolean; italic?: boolean; active?: boolean; disabled?: boolean; onClick: () => void; title: string;
}) {
  return (
    <button
      type="button"
      title={title}
      disabled={disabled}
      onClick={onClick}
      className={`min-w-8 rounded-lg border px-2 py-0.5 text-xs transition-colors disabled:opacity-40 ${active ? "border-accent bg-accent-soft text-accent" : "border-transparent text-dim hover:bg-surface-2 hover:text-text"}`}
    >
      <span className={bold ? "font-bold" : italic ? "italic" : ""}>{label}</span>
    </button>
  );
}

function viewCellToLocal(c: SheetCellView): LocalCell {
  const cell: LocalCell = { type: c.type as LocalCell["type"] };
  if (c.type === "formula") {
    cell.formula = c.formula;
    if (c.derived) {
      cell.derived = { kind: c.derived.kind, display: derivedText(c.derived) };
    }
  } else {
    cell.text = c.text;
    if (typeof c.number === "number") cell.number = c.number;
    if (typeof c.bool === "boolean") cell.bool = c.bool;
  }
  if (c.style) {
    cell.bold = c.style.bold;
    cell.italic = c.style.italic;
  }
  return cell;
}

function derivedText(d: NonNullable<SheetCellView["derived"]>): string {
  if (d.kind === "error") return d.err ?? "#ERR";
  if (d.kind === "unsupported") return "不可计算";
  const v = d.value;
  if (!v) return "";
  if (typeof v.number === "number") return String(v.number);
  if (typeof v.bool === "boolean") return v.bool ? "TRUE" : "FALSE";
  return v.text ?? "";
}
