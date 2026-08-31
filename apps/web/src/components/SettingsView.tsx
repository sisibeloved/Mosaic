// 设置页（计划 v1.11）：harness 可执行项管理（发现/登录态/启停）。
import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type Executable } from "../api/client";

export function SettingsView() {
  const [executables, setExecutables] = useState<Executable[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

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
    </div>
  );
}

function truncate(s: string): string {
  return s.length > 48 ? `${s.slice(0, 48)}…` : s;
}
