// 设置页（计划 v1.11）：harness 可执行项管理 + 预算/策略展示。
// 策略编辑面随 B 轨三模式配置面开放（本页先行只读水位）。
import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type Executable } from "../api/client";

declare const MOSAIC_DEV: boolean;

interface DebugBudget {
  rounds?: number;
  utterances?: number;
  tokens?: number;
  level?: number;
  remaining_tokens?: number;
  limits?: { max_rounds?: number; max_utterances?: number; max_tokens?: number };
}

export function SettingsView({ roomID }: { roomID: string | null }) {
  const [executables, setExecutables] = useState<Executable[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [budget, setBudget] = useState<DebugBudget | null>(null);

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
        <h3>策略与预算</h3>
        <p className="hint">
          当前策略：Open Floor（M1 单模式；三模式与 Policy 参数编辑随 B 轨配置面在此开放）。
        </p>
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
