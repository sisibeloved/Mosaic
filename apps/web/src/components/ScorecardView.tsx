// 记分卡面板（计划 M2 / R-08 / OQ-17）：intent 全量投影——band、未选理由、
// 保送入口（人类保送 = 直接授予 Floor，不绕过预算/资格）。
import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type Snapshot } from "../api/client";

export function ScorecardView({ roomID }: { roomID: string | null }) {
  const [snap, setSnap] = useState<Snapshot | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

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

  const endorse = async (intentID: string) => {
    if (!roomID || !snap) return;
    setBusy(intentID);
    try {
      await api.endorseIntent(roomID, snap.room_version, intentID);
      await refresh();
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        const fresh = await api.snapshot(roomID);
        await api.endorseIntent(roomID, fresh.room_version, intentID);
        await refresh();
        return;
      }
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  };

  if (!roomID) {
    return (
      <div className="settings">
        <h2>记分卡</h2>
        <p className="hint">创建房间后，这里展示每轮全部发言意向：分数区间（band）、是否获选、未选理由——可对未获选意向一键保送。</p>
      </div>
    );
  }
  const items = [...(snap?.scorecard ?? [])].reverse(); // 最新在前
  return (
    <div className="settings">
      <h2>记分卡</h2>
      <p className="hint">
        公开 band（反 Goodhart：不公开精确分）；保送 = owner 直接授予 Floor（OQ-17，不绕过预算/资格）。
      </p>
      {error && <div className="error">{error}</div>}
      {!snap ? (
        <p className="hint">加载中…</p>
      ) : items.length === 0 ? (
        <p className="hint">尚无意向记录——发起一轮讨论后此处可查。</p>
      ) : (
        <table className="exe-table">
          <thead>
            <tr>
              <th>参与者</th>
              <th>意向</th>
              <th>Band</th>
              <th>获选</th>
              <th>未选理由</th>
              <th>公开理由</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {items.map((it) => (
              <tr key={it.intent_id}>
                <td className="mono">{it.participant_id}</td>
                <td>
                  {it.action === "silent" ? "弃权" : (it.type || it.action)}
                </td>
                <td>
                  <span className={`badge ${it.selected ? "ok" : it.score_band === "unranked" ? "warn" : ""}`}>
                    {it.score_band}
                  </span>
                </td>
                <td>{it.selected ? "✓" : it.endorsed ? "保送" : "—"}</td>
                <td>{it.unselected_reason || "—"}</td>
                <td className="mono" title={it.public_rationale}>
                  {it.public_rationale ? truncateR(it.public_rationale) : "—"}
                </td>
                <td>
                  {!it.selected && !it.endorsed && it.action !== "silent" && (
                    <button disabled={busy === it.intent_id} onClick={() => void endorse(it.intent_id)}>
                      {busy === it.intent_id ? "保送中…" : "保送"}
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <p>
        <button className="ghost" onClick={() => void refresh()}>
          刷新
        </button>
      </p>
    </div>
  );
}

function truncateR(s: string): string {
  return s.length > 40 ? `${s.slice(0, 40)}…` : s;
}
