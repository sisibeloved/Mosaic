// 设置页（计划 v1.11）：harness 可执行项管理 + 策略配置（B1：三模式参数束）+ 预算水位。
import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type Executable, type Schemas } from "../api/client";

declare const MOSAIC_DEV: boolean;

interface DebugBudget {
  rounds?: number;
  utterances?: number;
  tokens?: number;
  level?: number;
  remaining_tokens?: number;
  limits?: { max_rounds?: number; max_utterances?: number; max_tokens?: number };
}

/** 快照策略区视图（字段可选——见 OpenAPI Snapshot.policy）。 */
interface SnapshotPolicy {
  policy_version?: string;
  mode?: string;
  max_speakers?: number;
  lambda?: number;
  intent_window?: string;
  response_cap?: number;
  reveal_strategy?: string;
}

/** 三模式产品面（review/decision 为收束模式，随 M3 面板开放）。 */
const MODES: { id: string; label: string; desc: string }[] = [
  { id: "open_floor", label: "Open Floor", desc: "开放讨论（默认 3 人/轮，20s 窗口）" },
  { id: "roundtable", label: "Roundtable", desc: "圆桌（全员各 1，30s 窗口）" },
  { id: "deep_dive", label: "Deep Dive", desc: "深潜（2 人/轮，15s 窗口，900 cap）" },
];

/** 模式默认参数束（与房间侧 policyDefaults 同源；提交前服务端再校验）。
 * reveal 三策略自 B2 起全部可执行。 */
function modeDefaults(mode: string): Schemas["PolicyParams"] {
  const base: Schemas["PolicyParams"] = {
    mode: "open_floor",
    max_speakers: 3,
    lambda: 0.3,
    weights: { relevance: 0.3, novelty: 0.2, diversity: 0.15, urgency: 0.1, direct_address: 0.15, floor_share: 0.05, repetition: 0.05 },
    intent_window: "20s",
    response_cap: 500,
    reveal_strategy: "simultaneous",
    rebuttals: 0,
  };
  if (mode === "roundtable") return { ...base, mode, max_speakers: 8, intent_window: "30s", response_cap: 600, reveal_strategy: "independent_then_cross", rebuttals: 1 };
  if (mode === "deep_dive") return { ...base, mode, max_speakers: 2, intent_window: "15s", response_cap: 900, reveal_strategy: "sequential" };
  return base;
}

export function SettingsView({ roomID }: { roomID: string | null }) {
  const [executables, setExecutables] = useState<Executable[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [budget, setBudget] = useState<DebugBudget | null>(null);
  const [policy, setPolicy] = useState<SnapshotPolicy | null>(null);
  const [policyVersion, setPolicyVersion] = useState<string>("");
  const [roomVersion, setRoomVersion] = useState(0);
  const [draftMode, setDraftMode] = useState<string>("open_floor");
  const [policyMsg, setPolicyMsg] = useState<string | null>(null);

  const refreshPolicy = useCallback(async () => {
    if (!roomID) {
      setPolicy(null);
      return;
    }
    try {
      const snap = await api.snapshot(roomID);
      setPolicy(snap.policy ?? null);
      setPolicyVersion(snap.policy?.policy_version ?? "");
      setRoomVersion(snap.room_version);
      setDraftMode(snap.policy?.mode ?? "open_floor");
    } catch {
      setPolicy(null);
    }
  }, [roomID]);

  useEffect(() => {
    void refreshPolicy();
  }, [refreshPolicy]);

  const applyPolicy = async () => {
    if (!roomID) return;
    setBusy("policy");
    setPolicyMsg(null);
    try {
      // 以所选模式的默认束提交（参数编辑面随记分卡面板细化；权重/λ 走默认）
      await api.setPolicy(roomID, roomVersion, modeDefaults(draftMode));
      await refreshPolicy();
      setPolicyMsg("已生效（下一轮起）");
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        // 版本已前进（轮次推进）——校准后重试一次
        const snap = await api.snapshot(roomID);
        await api.setPolicy(roomID, snap.room_version, modeDefaults(draftMode));
        await refreshPolicy();
        setPolicyMsg("已生效（下一轮起；版本校准后重试）");
        return;
      }
      setPolicyMsg(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  };


  const refresh = useCallback(async () => {
    try {
      const { executables } = await api.executables();
      setExecutables(executables);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const refreshBudget = useCallback(async () => {
    if (!MOSAIC_DEV || !roomID) return;
    try {
      const st = (await api.debugState(roomID)) as { budget?: DebugBudget };
      setBudget(st.budget ?? null);
    } catch {
      setBudget(null);
    }
  }, [roomID]);

  useEffect(() => {
    void refreshBudget();
    const timer = setInterval(() => void refreshBudget(), 5000);
    return () => clearInterval(timer);
  }, [refreshBudget]);

  const toggle = async (exe: Executable) => {
    setBusy(exe.id);
    try {
      await api.setEnabled(exe.id, !exe.enabled);
      await refresh();
    } catch (e) {
      setError(
        e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e),
      );
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="settings">
      <h2>设置</h2>
      <section>
        <h3>Agent 可执行项</h3>
        <p className="hint">
          自动扫描本机已安装的 CLI（Codex / Kimi 等）；未登录的项不可启用。启用后 ≤10 秒自动入座，无需重启。
        </p>
        {error && <div className="error">{error}</div>}
        {executables === null ? (
          <p className="hint">加载中…</p>
        ) : executables.length === 0 ? (
          <p className="hint">未发现任何可执行项。</p>
        ) : (
          <table className="exe-table">
            <thead>
              <tr>
                <th>适配器</th>
                <th>运行面</th>
                <th>版本</th>
                <th>登录态</th>
                <th>路径</th>
                <th>状态</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {executables.map((exe) => (
                <tr key={exe.id}>
                  <td>{exe.adapter}</td>
                  <td>{exe.runtime}{exe.distro ? `（${exe.distro}）` : ""}</td>
                  <td>{exe.version || "—"}</td>
                  <td>
                    <span className={`badge ${exe.login_state === "logged_in" ? "ok" : "warn"}`}>
                      {exe.login_state}
                    </span>
                  </td>
                  <td className="mono" title={exe.path}>
                    {truncate(exe.path)}
                  </td>
                  <td>{exe.enabled ? "已启用" : "未启用"}</td>
                  <td>
                    <button
                      disabled={busy === exe.id || (exe.login_state !== "logged_in" && !exe.enabled)}
                      onClick={() => void toggle(exe)}
                    >
                      {exe.enabled ? "禁用" : "启用"}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <p>
          <button className="ghost" onClick={() => void refresh()}>
            重新扫描注册表
          </button>
        </p>
      </section>
      <section>
        <h3>策略</h3>
        {!roomID ? (
          <p className="hint">创建房间后在此配置讨论模式（变更在下一轮生效）。</p>
        ) : policy ? (
          <>
            <p className="hint">
              当前：{policy.mode}（{policyVersion}）· 单轮 ≤{policy.max_speakers} 人 · 窗口{" "}
              {policy.intent_window} · cap {policy.response_cap} · reveal {policy.reveal_strategy}
            </p>
            <div className="row policy-modes">
              {MODES.map((m) => (
                <button
                  key={m.id}
                  className={draftMode === m.id ? "active" : "ghost"}
                  disabled={busy === "policy"}
                  onClick={() => setDraftMode(m.id)}
                  title={m.desc}
                >
                  {m.label}
                </button>
              ))}
            </div>
            <p className="hint">{MODES.find((m) => m.id === draftMode)?.desc}</p>
            <p>
              <button disabled={busy === "policy"} onClick={() => void applyPolicy()}>
                {busy === "policy" ? "提交中…" : "应用模式（下一轮起生效）"}
              </button>
              {policyMsg && <span className="hint"> {policyMsg}</span>}
            </p>
            <p className="hint">
              权重/λ/续聊等细参数编辑随记分卡面板开放；reveal 策略随模式默认（Roundtable 含 1 轮 cross 交锋）。
            </p>
          </>
        ) : (
          <p className="hint">加载策略中…</p>
        )}
      </section>
      <section>
        <h3>预算</h3>
        {budget ? (
          <table className="exe-table">
            <tbody>
              <tr>
                <th>轮次</th>
                <td>
                  {budget.rounds ?? "—"} / {budget.limits?.max_rounds ?? "—"}
                </td>
              </tr>
              <tr>
                <th>发言</th>
                <td>
                  {budget.utterances ?? "—"} / {budget.limits?.max_utterances ?? "—"}
                </td>
              </tr>
              <tr>
                <th>Token</th>
                <td>
                  {budget.tokens ?? "—"} / {budget.limits?.max_tokens ?? "—"}
                  {budget.remaining_tokens !== undefined &&
                    `（剩余 ${budget.remaining_tokens.toLocaleString()}）`}
                </td>
              </tr>
              <tr>
                <th>梯度</th>
                <td>{["正常", "70% 降发言", "90% 降座", "100% 硬停"][budget.level ?? 0]}</td>
              </tr>
            </tbody>
          </table>
        ) : (
          <p className="hint">
            {MOSAIC_DEV ? "创建房间后此处显示实时预算水位。" : "开发者模式（-dev）下显示实时预算水位。"}
          </p>
        )}
      </section>
    </div>
  );
}

function truncate(s: string): string {
  return s.length > 48 ? `${s.slice(0, 48)}…` : s;
}
