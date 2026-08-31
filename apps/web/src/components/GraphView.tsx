// Graph 视图（计划 M2 / RFC-0004）：显式关系边（forked_from/responds_to/
// merged_into + relations 类型化边）与线程生命周期状态。推断边属结构投影（M3），
// 接入后以 inferred 标记区分——双视图的"显式 vs 推断"语义在此可查。
import { useCallback, useEffect, useState } from "react";
import { api, type Snapshot } from "../api/client";

interface GraphEdge {
  kind: string;
  from: string;
  to: string;
  inferred: boolean;
}

interface ThreadItem {
  thread_id: string;
  state: string;
  parent?: string;
  goal?: string;
  merged_into?: string;
  message_count: number;
}

export function GraphView({ roomID }: { roomID: string | null }) {
  const [snap, setSnap] = useState<Snapshot | null>(null);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    if (!roomID) {
      setSnap(null);
      return;
    }
    try {
      setSnap(await api.snapshot(roomID));
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [roomID]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const threads: ThreadItem[] = (snap?.threads as unknown as ThreadItem[]) ?? [];
  const edges: GraphEdge[] = (snap?.graph as unknown as GraphEdge[]) ?? [];

  if (!roomID) {
    return (
      <div className="settings">
        <h2>图谱</h2>
        <p className="hint">创建房间后，这里展示线程谱系（fork/merge）与消息间的显式关系边。</p>
      </div>
    );
  }
  return (
    <div className="settings">
      <h2>图谱</h2>
      <p className="hint">
        显式边（发言者声明 + fork/merge 结构）；推断边随结构投影（M3）接入后标记"推断"。
      </p>
      {error && <div className="error">{error}</div>}
      <section>
        <h3>线程（{threads.length}）</h3>
        <table className="exe-table">
          <thead>
            <tr>
              <th>线程</th>
              <th>状态</th>
              <th>谱系</th>
              <th>目标</th>
              <th>消息数</th>
            </tr>
          </thead>
          <tbody>
            {threads.map((th) => (
              <tr key={th.thread_id}>
                <td className="mono">{short(th.thread_id)}</td>
                <td>
                  <span className={`badge ${th.state === "active" ? "ok" : th.state === "merged" ? "warn" : ""}`}>
                    {th.state}
                  </span>
                </td>
                <td>{th.parent ? `← ${short(th.parent)}` : "根"}</td>
                <td>{th.goal || (th.merged_into ? `→ 合并入 ${short(th.merged_into)}` : "—")}</td>
                <td>{th.message_count}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>
      <section>
        <h3>关系边（{edges.length}）</h3>
        {edges.length === 0 ? (
          <p className="hint">暂无——发消息时声明 relations（支持/质疑/…）或从消息分叉线程。</p>
        ) : (
          <ul className="edge-list">
            {edges.map((e, i) => (
              <li key={i}>
                <span className="badge">{e.kind}</span>{" "}
                <span className="mono">{short(e.from)}</span> → <span className="mono">{short(e.to)}</span>
                {e.inferred && <span className="hint">（推断）</span>}
              </li>
            ))}
          </ul>
        )}
      </section>
      <p>
        <button className="ghost" onClick={() => void refresh()}>
          刷新
        </button>
      </p>
    </div>
  );
}

function short(id: string): string {
  return id.length > 20 ? `${id.slice(0, 20)}…` : id;
}
