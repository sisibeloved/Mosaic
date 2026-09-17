// 文档面 store（零依赖：useSyncExternalStore + 模块级状态，state/rooms.ts 先例）：
//  1. 文档列表（主页数据源；created_by 筛选态自持，刷新单飞去重，不轮询）；
//  2. 文档元信息缓存（消息卡片/附着行/引用 chip 的标题·版本·更新者数据源）——
//     首订阅拉快照；live 订阅挂 doc:{id} SSE（事件驱动重取，计数归零断流）。
import { useCallback, useSyncExternalStore } from "react";
import { api, ApiError, type DocState, type DocSummary } from "../api/client";
import { docCursor } from "../api/doc";

// ---- 文档列表 ----

export interface DocsState {
  docs: DocSummary[] | null; // null = 尚未加载
  error: string | null;
  /** 创建者筛选（participant_id；null = 全部）。 */
  createdBy: string | null;
}

let listState: DocsState = { docs: null, error: null, createdBy: null };
const listListeners = new Set<() => void>();
let listInflight: Promise<void> | null = null;

function emitList(next: DocsState): void {
  listState = next;
  listListeners.forEach((l) => l());
}

function subscribeList(listener: () => void): () => void {
  listListeners.add(listener);
  return () => listListeners.delete(listener);
}

function getList(): DocsState {
  return listState;
}

export function useDocs(): DocsState {
  return useSyncExternalStore(subscribeList, getList);
}

/** 拉取文档列表（单飞去重——并发调用共享一次请求）。 */
export async function refreshDocs(): Promise<void> {
  if (listInflight) return listInflight;
  listInflight = (async () => {
    try {
      const { docs } = await api.listDocs(listState.createdBy ? { createdBy: listState.createdBy } : {});
      emitList({ ...listState, docs, error: null });
    } catch (e) {
      emitList({ ...listState, error: e instanceof Error ? e.message : String(e) });
    } finally {
      listInflight = null;
    }
  })();
  return listInflight;
}

/** 创建者筛选切换（§2.8：我/各 agent）→ 重取列表。 */
export async function setDocsCreatorFilter(createdBy: string | null): Promise<void> {
  if (listState.createdBy === createdBy) return;
  emitList({ ...listState, createdBy, docs: null });
  await refreshDocs();
}

// ---- 文档元信息缓存 ----

export interface DocMeta {
  doc: DocState | null; // null = 加载中
  missing: boolean; // 404 = 不存在或已删除（卡片降级）
}

interface MetaEntry {
  meta: DocMeta;
  listeners: Set<() => void>;
  liveCount: number;
  es: EventSource | null;
  fetching: boolean;
}

const metas = new Map<string, MetaEntry>();
const EMPTY_META: DocMeta = { doc: null, missing: false };

const META_EVENTS = [
  "doc.created",
  "doc.revision_committed",
  "doc.renamed",
  "doc.archived",
  "doc.restored",
  "doc.deleted",
] as const;

function entryOf(docID: string): MetaEntry {
  let e = metas.get(docID);
  if (!e) {
    e = { meta: EMPTY_META, listeners: new Set(), liveCount: 0, es: null, fetching: false };
    metas.set(docID, e);
  }
  return e;
}

function emitMeta(docID: string, meta: DocMeta): void {
  const e = entryOf(docID);
  e.meta = meta;
  e.listeners.forEach((l) => l());
}

async function fetchMeta(docID: string): Promise<void> {
  const e = entryOf(docID);
  if (e.fetching) return;
  e.fetching = true;
  try {
    const doc = await api.getDoc(docID);
    emitMeta(docID, { doc, missing: false });
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) {
      emitMeta(docID, { doc: null, missing: true });
    }
    // 其他错误保持原状——下个事件/订阅再试
  } finally {
    e.fetching = false;
  }
}

function closeLive(docID: string): void {
  const e = entryOf(docID);
  e.es?.close();
  e.es = null;
}

function openLive(docID: string): void {
  const e = entryOf(docID);
  if (e.es) return;
  const cursor = docCursor(e.meta.doc?.version ?? 0);
  const es = new EventSource(`/v1/docs/${encodeURIComponent(docID)}/events?cursor=${encodeURIComponent(cursor)}`);
  e.es = es;
  // 事件驱动重取（卡片/附着行只读面：简单正确优先于本地折叠）
  for (const t of META_EVENTS) es.addEventListener(t, () => void fetchMeta(docID));
  es.addEventListener("resync_required", () => {
    closeLive(docID);
    void fetchMeta(docID).then(() => {
      if (entryOf(docID).liveCount > 0) openLive(docID);
    });
  });
  es.onerror = () => {
    if (es.readyState !== EventSource.CLOSED) return; // 浏览器自动重连
    closeLive(docID);
    setTimeout(() => {
      if (entryOf(docID).liveCount <= 0) return;
      void fetchMeta(docID).then(() => {
        if (entryOf(docID).liveCount > 0) openLive(docID);
      });
    }, 3000);
  };
}

/** 订阅文档元信息（live=true 挂 SSE 实时刷新；live 计数归零断流）。 */
export function subscribeDocMeta(docID: string, live: boolean, onChange: () => void): () => void {
  const e = entryOf(docID);
  e.listeners.add(onChange);
  if (!e.meta.doc && !e.meta.missing) void fetchMeta(docID);
  if (live) {
    e.liveCount++;
    openLive(docID);
  }
  return () => {
    e.listeners.delete(onChange);
    if (live) {
      e.liveCount = Math.max(0, e.liveCount - 1);
      if (e.liveCount === 0) closeLive(docID);
    }
  };
}

/** 文档元信息 hook（卡片 live=true；附着行/芯片 live=false 快照即可）。 */
export function useDocMeta(docID: string, live = false): DocMeta {
  const subscribe = useCallback((onChange: () => void) => subscribeDocMeta(docID, live, onChange), [docID, live]);
  const getSnapshot = useCallback(() => entryOf(docID).meta, [docID]);
  return useSyncExternalStore(subscribe, getSnapshot);
}

/** 主动刷新某文档元信息（编辑器提交后卡片同步等）。 */
export async function refreshDocMeta(docID: string): Promise<void> {
  await fetchMeta(docID);
}
