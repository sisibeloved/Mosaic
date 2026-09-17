// 文档订阅 hook（RFC-0014 §2.6 doc:{id} 频道；useRoom 先例平移的最小版）：
// - 初载走快照（GET /v1/docs/{id}），EventSource 自 version 水位续传；
// - doc.* 事件驱动防抖重取快照（只读面/编辑器共用：简单正确优先于本地折叠；
//   编辑器的工作台合并策略在 DocEditorPage 自持）；
// - resync_required → 弃流、快照重建、重新订阅；断流 CLOSED → 退避重订；
// - 曾加载后 refetch 404 = 他端删除（级联清库）——deleted 标志供编辑器导航离开。
import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError, type DocState } from "./client";
import type { Connection } from "./room";

const DOC_EVENTS = [
  "doc.created",
  "doc.revision_committed",
  "doc.renamed",
  "doc.archived",
  "doc.restored",
  "doc.deleted",
] as const;

/** doc 版本 → SSE opaque 游标（protocol.EncodeCursor 同构：base64url("v1:<n>")，
 *  URL 安全字母表 + 填充——btoa 为标准字母表，逐字符替换对齐）。 */
export function docCursor(version: number): string {
  return btoa(`v1:${version}`).replaceAll("+", "-").replaceAll("/", "_");
}

export interface DocHandle {
  docID: string | null;
  doc: DocState | null;
  /** 首载 404（文档不存在或已删除）。 */
  notFound: boolean;
  /** 曾成功加载后 404 / doc.deleted = 他端删除（编辑器据此导航离开）。 */
  deleted: boolean;
  connection: Connection;
  error: string | null;
  refresh(): Promise<void>;
}

export function useDoc(docID: string | null): DocHandle {
  const [doc, setDoc] = useState<DocState | null>(null);
  const [notFound, setNotFound] = useState(false);
  const [deleted, setDeleted] = useState(false);
  const [connection, setConnection] = useState<Connection>("idle");
  const [error, setError] = useState<string | null>(null);
  const esRef = useRef<EventSource | null>(null);
  const docRef = useRef<string | null>(null);
  const loadedRef = useRef(false);
  const refetchTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const refetchBusyRef = useRef(false);
  const reloadRef = useRef<(id: string) => Promise<void>>(async () => {});

  const closeStream = useCallback(() => {
    esRef.current?.close();
    esRef.current = null;
  }, []);

  /** 快照重取（事件驱动；单飞——编辑器提交与 SSE 事件交叠时共享一次请求）。 */
  const refetch = useCallback(async () => {
    const id = docRef.current;
    if (!id || refetchBusyRef.current) return;
    refetchBusyRef.current = true;
    try {
      const d = await api.getDoc(id);
      if (docRef.current !== id) return;
      loadedRef.current = true;
      setDoc(d);
      setDeleted(false);
    } catch (e) {
      if (e instanceof ApiError && e.status === 404 && loadedRef.current) {
        setDeleted(true);
        closeStream();
      }
    } finally {
      refetchBusyRef.current = false;
    }
  }, [closeStream]);

  const scheduleRefetch = useCallback(() => {
    if (refetchTimerRef.current) clearTimeout(refetchTimerRef.current);
    refetchTimerRef.current = setTimeout(() => {
      refetchTimerRef.current = null;
      void refetch();
    }, 150);
  }, [refetch]);

  /** 快照重建 + 自水位订阅（初载与 resync_required 共用路径）。 */
  const loadAndSubscribe = useCallback(
    async (id: string) => {
      closeStream();
      setConnection("connecting");
      setError(null);
      setNotFound(false);
      let d: DocState;
      try {
        d = await api.getDoc(id);
      } catch (e) {
        setConnection("idle");
        setDoc(null);
        if (e instanceof ApiError && e.status === 404) {
          setNotFound(true);
          setError("文档不存在或已删除");
        } else {
          setError(`文档加载失败：${e instanceof Error ? e.message : String(e)}`);
        }
        return;
      }
      if (docRef.current !== id) return; // 等待期间已切换文档
      loadedRef.current = true;
      setDoc(d);
      setDeleted(false);
      const es = new EventSource(
        `/v1/docs/${encodeURIComponent(id)}/events?cursor=${encodeURIComponent(docCursor(d.version))}`,
      );
      esRef.current = es;
      es.onopen = () => setConnection("live");
      es.onerror = () => {
        if (es.readyState === EventSource.CLOSED) {
          // 永久断流：快照重建 + 退避重订（同 useRoom 纪律）
          setConnection("resync");
          setTimeout(() => {
            if (esRef.current === es) void reloadRef.current(id);
          }, 3000);
        } else {
          setConnection("reconnecting"); // 浏览器将带 Last-Event-ID 自动重连
        }
      };
      for (const t of DOC_EVENTS) es.addEventListener(t, scheduleRefetch);
      es.addEventListener("resync_required", () => {
        setConnection("resync");
        void loadAndSubscribe(id);
      });
    },
    [closeStream, scheduleRefetch],
  );

  useEffect(() => {
    reloadRef.current = loadAndSubscribe;
  }, [loadAndSubscribe]);

  // 文档切换：复位并重新装载；卸载/切换时断流。
  useEffect(() => {
    docRef.current = docID;
    loadedRef.current = false;
    setDoc(null);
    setDeleted(false);
    setNotFound(false);
    if (!docID) {
      closeStream();
      setConnection("idle");
      return;
    }
    void loadAndSubscribe(docID);
    return closeStream;
  }, [docID, loadAndSubscribe, closeStream]);

  return {
    docID: doc ? doc.doc_id : null,
    doc,
    notFound,
    deleted,
    connection,
    error,
    refresh: refetch,
  };
}
