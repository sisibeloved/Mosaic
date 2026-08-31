// 开发者模式面板（M1 v1.8 机制延续）：MOSAIC_DEV 由服务端注入；直读 /v1/debug 只读端点。
import { useState } from "react";
import { api, lastTrace } from "../api/client";

declare const MOSAIC_DEV: boolean;

export function DevPanel({ roomID }: { roomID: string | null }) {
  const [out, setOut] = useState<string>("");
  if (!MOSAIC_DEV) return null;
  const load = async (fn: () => Promise<unknown>) => {
    try {
      setOut(JSON.stringify(await fn(), null, 2));
    } catch (e) {
      setOut(String(e));
    }
  };
  return (
    <div className="devpanel">
      <div className="devpanel-title">开发者模式</div>
      <div className="hint">trace: {lastTrace || "—"}</div>
      <div className="devpanel-actions">
        <button className="ghost" disabled={!roomID} onClick={() => void load(() => api.debugState(roomID!))}>
          状态
        </button>
        <button className="ghost" disabled={!roomID} onClick={() => void load(() => api.debugEvents(roomID!))}>
          事件
        </button>
      </div>
      {out && <pre className="devpanel-out">{out}</pre>}
    </div>
  );
}
