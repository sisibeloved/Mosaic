// 文档块编辑的本地运算（RFC-0014 §2.3/§2.5）：工作台与已提交态的差分 → 协议
// ops；409 冲突时在新态上重放未提交 ops（锚点丢失回退文末追加——文本不丢）。
// 新块 ID 客户端生成：服务端对空 block_id 才分配（normalizeOps），非空原样
// 保留——编辑器自造稳定 ID，新块跨提交可锚定、重放可对应。
import type { DocBlock, DocOp } from "../api/client";

/** 新块 ID（blk_w + 12hex 随机尾；与服务端 blk_<8hex> 形态同族不撞名）。 */
export function newBlockID(): string {
  const rnd = new Uint8Array(6);
  crypto.getRandomValues(rnd);
  return `blk_w${Array.from(rnd, (b) => b.toString(16).padStart(2, "0")).join("")}`;
}

/**
 * 差分：base（已提交态）→ next（工作台）的协议 op 批。
 * 序：先删（腾位 + 释放 ID），再文本替换，最后按文档序插入新块。
 * type 变更无法由 replace 表达 → delete + insert_after 同 ID 重建（ID 保留，
 * 同批先删后插不撞唯一性）。连续新块同锚时逆序 insert_after——服务端语义下
 * 同锚重复 insert_after 逆序堆叠（room 引擎 chainedInserts 同因）。
 */
export function diffBlocks(base: DocBlock[], next: DocBlock[]): DocOp[] {
  const ops: DocOp[] = [];
  const baseByID = new Map(base.map((b) => [b.block_id, b]));
  const nextByID = new Map(next.map((b) => [b.block_id, b]));
  const isNew = (id: string): boolean => {
    const b = baseByID.get(id);
    const n = nextByID.get(id);
    return !b || !n || b.type !== n.type;
  };
  // 1) 删除：base 有而 next 无，或 type 变更（旧形态先删）
  for (const b of base) {
    if (isNew(b.block_id)) ops.push({ op: "delete", block_id: b.block_id });
  }
  // 2) 文本替换：同 ID 同 type 而 text 变
  for (const b of base) {
    const n = nextByID.get(b.block_id);
    if (n && n.type === b.type && n.text !== b.text) {
      ops.push({ op: "replace", block_id: b.block_id, text: n.text });
    }
  }
  // 3) 插入：next 中的新块段，锚 = 段前最近的存活块（无则 null=文首）；
  //    段内逆序同锚 insert_after 保序
  let i = 0;
  while (i < next.length) {
    if (!isNew(next[i].block_id)) {
      i++;
      continue;
    }
    let j = i;
    while (j < next.length && isNew(next[j].block_id)) j++;
    let anchor: string | null = null;
    for (let k = i - 1; k >= 0; k--) {
      if (!isNew(next[k].block_id)) {
        anchor = next[k].block_id;
        break;
      }
    }
    for (let k = j - 1; k >= i; k--) {
      ops.push({ op: "insert_after", block_id: anchor, block: next[k] });
    }
    i = j;
  }
  return ops;
}

/**
 * 重放（409 重定基用）：把一批 ops 应用到本地块数组（与 doc.ApplyDocOps 同
 * 语义，外加降级——锚点已在他端消失时不丢用户文本：insert_after/append 落
 * 文末，replace 落为文末段落块）。返回新数组（输入不变）。
 */
export function applyOpsLocal(base: DocBlock[], ops: DocOp[]): DocBlock[] {
  const next = [...base];
  const indexOf = (id: string) => next.findIndex((b) => b.block_id === id);
  for (const op of ops) {
    switch (op.op) {
      case "delete": {
        const idx = indexOf(op.block_id ?? "");
        if (idx >= 0) next.splice(idx, 1);
        break;
      }
      case "replace": {
        const idx = indexOf(op.block_id ?? "");
        if (idx >= 0) {
          next[idx] = { ...next[idx], text: op.text ?? next[idx].text };
        } else if (op.block_id && op.text !== undefined) {
          next.push({ block_id: op.block_id, type: "paragraph", text: op.text });
        }
        break;
      }
      case "append":
      case "insert_after": {
        if (!op.block) break;
        if (indexOf(op.block.block_id) >= 0) break; // 已在（重放撞车）跳过
        let pos = next.length;
        if (op.op === "insert_after") {
          if (op.block_id === null) {
            pos = 0;
          } else {
            const idx = indexOf(op.block_id);
            pos = idx >= 0 ? idx + 1 : next.length; // 锚点丢失 → 文末
          }
        }
        next.splice(pos, 0, op.block);
        break;
      }
    }
  }
  return next;
}
