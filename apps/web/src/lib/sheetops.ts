// 表格前端工作台模型（RFC-0017 M1，DocEditorPage 工作台纪律的表格版）：
// - 本地模型：LocalSheet 稀疏格 map（"r:c" 键）+ 版本水位；编辑动作即产 op
//   （不做 diff——每个用户动作语义精确，累积待提交批）；
// - applySheetOpsLocal：服务端机械应用的镜像（409 重定基时在新态上重放未
//   提交 ops；本地乐观显示同函数——公式派生值始终以服务端 range 回读为准）；
// - 输入类型推断：= 前缀公式、数值/TRUE/FALSE 字面、' 前缀强制文本。
import type { SheetCellView, SheetCellWrite, SheetOp, SheetRangeView } from "../api/client";

/** 清格写入（wire 值 "null" 是字符串——openapi-typescript 会把它折叠成 TS
 *  null，生成类型与运行时形态不符，边界处统一经本构造器收口）。 */
export function clearCell(r: number, c: number): SheetCellWrite {
  return { r, c, type: "null" as unknown as SheetCellWrite["type"] };
}

/** 列号 → 列标（1→A … 200→GR）。 */
export function colName(c: number): string {
  if (c < 1 || c > 200) return "";
  let n = c;
  let out = "";
  while (n > 0) {
    n--;
    out = String.fromCharCode(65 + (n % 26)) + out;
    n = Math.floor(n / 26);
  }
  return out;
}

export function cellKey(r: number, c: number): string {
  return `${r}:${c}`;
}

export function a1(r: number, c: number): string {
  return `${colName(c)}${r}`;
}

/** 本地单元格（编辑工作台视角：值/公式/样式独立可空）。 */
export interface LocalCell {
  type: "text" | "number" | "bool" | "formula";
  text?: string;
  number?: number;
  bool?: boolean;
  formula?: string;
  bold?: boolean;
  italic?: boolean;
  /** 服务端派生（公式格；随 range 回读刷新，本地编辑后置空=待服务端权威）。 */
  derived?: { kind: "value" | "error" | "unsupported"; display: string };
}

export interface LocalSheet {
  sheetID: string;
  name: string;
  rows: number;
  cols: number;
  cells: Map<string, LocalCell>;
  colWidths: Map<number, number>;
  rowHeights: Map<number, number>;
}

/** range 回读 → 本地表。 */
export function sheetFromRange(view: SheetRangeView, meta: { sheetID: string; name: string; rows: number; cols: number }): LocalSheet {
  const cells = new Map<string, LocalCell>();
  for (const c of view.cells ?? []) {
    cells.set(cellKey(c.r, c.c), cellViewToLocal(c));
  }
  return { sheetID: meta.sheetID, name: meta.name, rows: meta.rows, cols: meta.cols, cells, colWidths: new Map(), rowHeights: new Map() };
}

function cellViewToLocal(c: SheetCellView): LocalCell {
  const cell: LocalCell = { type: c.type as LocalCell["type"] };
  if (c.type === "formula") {
    cell.formula = c.formula;
    if (c.derived) {
      cell.derived = { kind: c.derived.kind, display: derivedDisplay(c.derived) };
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

export function derivedDisplay(d: NonNullable<SheetCellView["derived"]>): string {
  if (d.kind === "error") return d.err ?? "#ERR";
  if (d.kind === "unsupported") return "不可计算";
  const v = d.value;
  if (!v) return "";
  if (typeof v.number === "number") return formatNumber(v.number);
  if (typeof v.bool === "boolean") return v.bool ? "TRUE" : "FALSE";
  return v.text ?? "";
}

export function formatNumber(n: number): string {
  return String(n);
}

/** 输入文本 → 写入 cell（= 公式 / ' 文本逃逸 / 数值与布尔字面推断）。 */
export function parseCellInput(raw: string): { type: "text" | "number" | "bool" | "formula" | "null"; text?: string; number?: number; bool?: boolean; formula?: string } {
  if (raw === "") return { type: "null" };
  if (raw.startsWith("=")) {
    const f = raw.slice(1).trim();
    return f === "" ? { type: "null" } : { type: "formula", formula: f };
  }
  if (raw.startsWith("'")) return { type: "text", text: raw.slice(1) };
  const up = raw.trim().toUpperCase();
  if (up === "TRUE") return { type: "bool", bool: true };
  if (up === "FALSE") return { type: "bool", bool: false };
  if (/^-?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/.test(raw.trim())) {
    const n = Number(raw.trim());
    if (Number.isFinite(n)) return { type: "number", number: n };
  }
  return { type: "text", text: raw };
}

/** 本地格 → 显示文本（公式格显派生；本地已编辑未回读时显原式文本）。 */
export function cellDisplay(cell: LocalCell | undefined): string {
  if (!cell) return "";
  if (cell.type === "formula") {
    if (cell.derived) return cell.derived.display;
    return `=${cell.formula ?? ""}`;
  }
  if (cell.type === "number") return typeof cell.number === "number" ? formatNumber(cell.number) : "";
  if (cell.type === "bool") return cell.bool ? "TRUE" : "FALSE";
  return cell.text ?? "";
}

/** 公式栏内容（原始文本形态）。 */
export function cellRaw(cell: LocalCell | undefined): string {
  if (!cell) return "";
  if (cell.type === "formula") return `=${cell.formula ?? ""}`;
  return cellDisplay(cell);
}

/** 本地应用一批 ops（服务端机械应用的镜像；返回新 map——409 重放与乐观渲染共用）。 */
export function applySheetOpsLocal(sheet: LocalSheet, ops: SheetOp[]): LocalSheet {
  const cells = new Map(sheet.cells);
  const colWidths = new Map(sheet.colWidths);
  const rowHeights = new Map(sheet.rowHeights);
  let rows = sheet.rows;
  let cols = sheet.cols;
  const shiftCoord = (key: string, axis: "r" | "c", at: number, count: number, insert: boolean): [string, LocalCell] | null => {
    const [rs, cs] = key.split(":");
    const pos = axis === "r" ? Number(rs) : Number(cs);
    let np = pos;
    if (insert) {
      if (pos < at) return null; // 不动
      np = pos + count;
      if (np > (axis === "r" ? 5000 : 200)) return null; // 推出上限（服务端已拒，防御）
    } else {
      if (pos >= at && pos < at + count) return null; // 被删
      if (pos >= at + count) np = pos - count;
    }
    const nr = axis === "r" ? np : Number(rs);
    const nc = axis === "c" ? np : Number(cs);
    return [cellKey(nr, nc), cells.get(key) as LocalCell];
  };
  for (const op of ops) {
    switch (op.op) {
      case "set_cells": {
        for (const c of op.cells ?? []) {
          const key = cellKey(c.r, c.c);
          const prev = cells.get(key);
          if ((c.type as string) === "null") {
            if (prev) {
              const next: LocalCell = { ...prev, type: "text", text: undefined, number: undefined, bool: undefined, formula: undefined, derived: undefined };
              if (isEmptyLocal(next)) cells.delete(key);
              else cells.set(key, next);
            }
            continue;
          }
          const next: LocalCell =
            c.type === "formula"
              ? { type: "formula", formula: c.formula, bold: prev?.bold, italic: prev?.italic }
              : c.type === "number"
                ? { type: "number", number: c.number, bold: prev?.bold, italic: prev?.italic }
                : c.type === "bool"
                  ? { type: "bool", bool: c.bool, bold: prev?.bold, italic: prev?.italic }
                  : { type: "text", text: c.text, bold: prev?.bold, italic: prev?.italic };
          if (c.style) {
            next.bold = c.style.bold ?? prev?.bold;
            next.italic = c.style.italic ?? prev?.italic;
          }
          cells.set(key, next);
          if (c.r > rows) rows = Math.min(c.r, 5000);
          if (c.c > cols) cols = Math.min(c.c, 200);
        }
        break;
      }
      case "clear_range": {
        for (let r = op.range!.r1; r <= op.range!.r2; r++) {
          for (let c = op.range!.c1; c <= op.range!.c2; c++) {
            const key = cellKey(r, c);
            const prev = cells.get(key);
            if (!prev) continue;
            if (op.keep_style) {
              const next: LocalCell = { type: "text", bold: prev.bold, italic: prev.italic };
              cells.set(key, next);
            } else {
              cells.delete(key);
            }
          }
        }
        break;
      }
      case "set_style": {
        for (let r = op.range!.r1; r <= op.range!.r2; r++) {
          for (let c = op.range!.c1; c <= op.range!.c2; c++) {
            const key = cellKey(r, c);
            const prev = cells.get(key) ?? ({ type: "text" } as LocalCell);
            cells.set(key, {
              ...prev,
              bold: op.style?.bold ?? prev.bold,
              italic: op.style?.italic ?? prev.italic,
            });
          }
        }
        break;
      }
      case "set_col_width":
        if (op.col && op.width) colWidths.set(op.col, op.width);
        break;
      case "set_row_height":
        if (op.row && op.height) rowHeights.set(op.row, op.height);
        break;
      case "insert_rows":
      case "delete_rows":
      case "insert_columns":
      case "delete_columns": {
        const axis: "r" | "c" = op.op.endsWith("rows") ? "r" : "c";
        const insert = op.op.startsWith("insert");
        const moved: [string, LocalCell][] = [];
        for (const key of [...cells.keys()]) {
          const res = shiftCoord(key, axis, op.at ?? 1, op.count ?? 1, insert);
          if (res) moved.push(res);
          cells.delete(key);
        }
        for (const [key, cell] of moved) cells.set(key, cell);
        const shiftMap = (m: Map<number, number>) => {
          const out = new Map<number, number>();
          for (const [k, v] of m) {
            const pos = k;
            if (insert) {
              if (pos >= (op.at ?? 1)) out.set(pos + (op.count ?? 1), v);
              else out.set(pos, v);
            } else {
              if (pos >= (op.at ?? 1) && pos < (op.at ?? 1) + (op.count ?? 1)) continue;
              if (pos >= (op.at ?? 1) + (op.count ?? 1)) out.set(pos - (op.count ?? 1), v);
              else out.set(pos, v);
            }
          }
          return out;
        };
        if (axis === "r") {
          const shifted = shiftMap(rowHeights);
          rowHeights.clear();
          for (const [k, v] of shifted) rowHeights.set(k, v);
          rows = Math.max(1, insert ? rows + (op.count ?? 1) : rows - (op.count ?? 1));
        } else {
          const shifted = shiftMap(colWidths);
          colWidths.clear();
          for (const [k, v] of shifted) colWidths.set(k, v);
          cols = Math.max(1, insert ? cols + (op.count ?? 1) : cols - (op.count ?? 1));
        }
        break;
      }
      // add/rename/delete sheet 在页级处理（影响表清单非单表内容）
    }
  }
  return { ...sheet, cells, colWidths, rowHeights, rows, cols };
}

function isEmptyLocal(cell: LocalCell): boolean {
  return cell.type === "text" && cell.text === undefined && !cell.bold && !cell.italic;
}
