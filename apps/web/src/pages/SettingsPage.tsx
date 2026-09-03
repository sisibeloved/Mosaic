// 设置页（纯全局层，不读取任何"当前房间"状态）：Agent 实例（harness 可执行项管理 +
// 手动登记）/ 开发者（全局开关——开启后调试面板出现在各房间抽屉的"调试"Tab；
// 调试端点仍由服务端 -dev 决定是否装配）。
// 分层规矩：房间讨论策略在房间内调（抽屉"策略"Tab）；外观主题在个人中心。
import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type Executable } from "../api/client";
import { RuntimeOptsEditor } from "../components/RuntimeOptsEditor";
import { useDevMode } from "../state/dev";
import { adapterLabel, channelLabel } from "../lib/copy";
import { truncate } from "../lib/ui";

export function SettingsPage() {
  const [devMode, setDevMode] = useDevMode();
  const [executables, setExecutables] = useState<Executable[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [form, setForm] = useState({ adapter: "", runtime: "native", distro: "", path: "", version: "", channel: "" });
  const [formMsg, setFormMsg] = useState<string | null>(null);

  const refreshExecutables = useCallback(async () => {
    try {
      const { executables } = await api.executables();
      setExecutables(executables);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    void refreshExecutables();
  }, [refreshExecutables]);

  const toggle = async (exe: Executable) => {
    setBusy(exe.id);
    setError(null);
    try {
      await api.setEnabled(exe.id, !exe.enabled);
      await refreshExecutables();
    } catch (e) {
      setError(
        e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e),
      );
    } finally {
      setBusy(null);
    }
  };

  const register = async () => {
    const adapter = form.adapter.trim();
    const path = form.path.trim();
    if (!adapter || !path) {
      setFormMsg("适配器与路径必填");
      return;
    }
    setBusy("register");
    setFormMsg(null);
    try {
      await api.registerExecutable({
        adapter,
        runtime: form.runtime,
        path,
        ...(form.runtime === "wsl" && form.distro.trim() ? { distro: form.distro.trim() } : {}),
        ...(form.version.trim() ? { version: form.version.trim() } : {}),
        ...(form.channel.trim() ? { channel: form.channel.trim() } : {}),
      });
      setForm({ adapter: "", runtime: "native", distro: "", path: "", version: "", channel: "" });
      setFormMsg("已登记");
      await refreshExecutables();
    } catch (e) {
      setFormMsg(
        e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e),
      );
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto flex max-w-2xl flex-col gap-8 px-6 py-8">
        <h1 className="text-lg font-semibold tracking-tight">设置</h1>

        <section>
          <h2 className="mb-1 text-sm font-medium">Agent 实例</h2>
          <p className="mb-3 text-xs text-faint">
            自动扫描本机已安装的 CLI（Codex / Kimi 等）；未登录的项不可启用。启用后 ≤10 秒自动入座，无需重启。
            同一 Agent 的多个实例按家族优先级排序（Codex 桌面 App 优先、Kimi Code 优先，ADR-0012）——想用哪个就启用哪个。
          </p>
          {error && <p className="mb-2 text-xs text-danger">{error}</p>}
          {executables === null ? (
            <p className="text-xs text-faint">加载中…</p>
          ) : executables.length === 0 ? (
            <p className="text-xs text-faint">未发现任何可执行项。</p>
          ) : (
            <ul className="divide-y divide-border rounded-xl border border-border">
              {executables.map((exe) => {
                const loginBlocked = exe.login_state !== "logged_in" && !exe.enabled;
                return (
                  <li key={exe.id} className="flex flex-col gap-1.5 px-3 py-2.5">
                    <div className="flex items-center gap-3">
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-1.5 text-sm">
                          <span className="font-medium">{adapterLabel(exe.adapter)}</span>
                          <ExeBadge>{channelLabel(exe.channel)}</ExeBadge>
                          <ExeBadge>{exe.runtime}{exe.distro ? `（${exe.distro}）` : ""}</ExeBadge>
                          {exe.version && <span className="text-xs text-faint">{exe.version}</span>}
                          <ExeBadge tone={exe.login_state === "logged_in" ? "ok" : "warn"}>
                            {exe.login_state ?? "unknown"}
                          </ExeBadge>
                          {exe.source === "manual" && <ExeBadge>手动登记</ExeBadge>}
                        </div>
                        <p className="mt-0.5 truncate font-mono text-[11px] text-faint" title={exe.path}>
                          {truncate(exe.path, 64)}
                        </p>
                      </div>
                      <button
                        type="button"
                        disabled={busy === exe.id || loginBlocked}
                        title={loginBlocked ? "未登录——请先在对应 CLI 登录" : undefined}
                        onClick={() => void toggle(exe)}
                        className={`shrink-0 rounded-lg px-2.5 py-1 text-xs transition-opacity disabled:opacity-40 ${
                          exe.enabled
                            ? "bg-accent-soft text-accent hover:opacity-85"
                            : "bg-surface-3 text-text hover:opacity-85"
                        }`}
                      >
                        {exe.enabled ? "已启用" : loginBlocked ? "未登录" : "启用"}
                      </button>
                    </div>
                    {/* v1.48 运行参数：模型覆盖与思考强度（kimi 实查候选；codex 五档强度） */}
                    <RuntimeOptsEditor exe={exe} />
                  </li>
                );
              })}
            </ul>
          )}
          <div className="mt-3">
            <button
              type="button"
              onClick={() => void refreshExecutables()}
              className="rounded-lg bg-surface-3 px-2.5 py-1 text-xs text-text transition-opacity hover:opacity-85"
            >
              重新扫描
            </button>
          </div>
          <details className="mt-3 rounded-xl border border-border px-3 py-2">
            <summary className="cursor-pointer text-xs text-dim">手动登记可执行项</summary>
            <div className="mt-3 grid grid-cols-2 gap-2 text-xs">
              <label className="flex flex-col gap-1">
                <span className="text-faint">适配器（如 codex / kimi）</span>
                <input
                  value={form.adapter}
                  onChange={(e) => setForm({ ...form, adapter: e.target.value })}
                  className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                />
              </label>
              <label className="flex flex-col gap-1">
                <span className="text-faint">运行面</span>
                <select
                  value={form.runtime}
                  onChange={(e) => setForm({ ...form, runtime: e.target.value })}
                  className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                >
                  <option value="native">native</option>
                  <option value="wsl">wsl</option>
                </select>
              </label>
              {form.runtime === "wsl" && (
                <label className="flex flex-col gap-1">
                  <span className="text-faint">WSL 发行版</span>
                  <input
                    value={form.distro}
                    onChange={(e) => setForm({ ...form, distro: e.target.value })}
                    className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                  />
                </label>
              )}
              <label className="col-span-2 flex flex-col gap-1">
                <span className="text-faint">可执行文件绝对路径</span>
                <input
                  value={form.path}
                  onChange={(e) => setForm({ ...form, path: e.target.value })}
                  placeholder="/usr/local/bin/codex"
                  className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono outline-none focus:border-accent"
                />
              </label>
              <label className="flex flex-col gap-1">
                <span className="text-faint">版本（可选，留空自动探测）</span>
                <input
                  value={form.version}
                  onChange={(e) => setForm({ ...form, version: e.target.value })}
                  className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                />
              </label>
              <label className="flex flex-col gap-1">
                <span className="text-faint">渠道（可选，如 app:kimi-work；留空按 CLI）</span>
                <input
                  value={form.channel}
                  onChange={(e) => setForm({ ...form, channel: e.target.value })}
                  className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                />
              </label>
            </div>
            <div className="mt-3 flex items-center gap-2">
              <button
                type="button"
                disabled={busy === "register"}
                onClick={() => void register()}
                className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
              >
                {busy === "register" ? "登记中…" : "登记"}
              </button>
              {formMsg && <span className="text-xs text-dim">{formMsg}</span>}
            </div>
            <p className="mt-2 text-[11px] text-faint">登记时服务端会探测该路径（探测失败拒收）。</p>
          </details>
        </section>

        <section>
          <h2 className="mb-1 text-sm font-medium">开发者</h2>
          <div className="flex items-start justify-between gap-4">
            <p className="text-xs text-faint">
              全局开关：开启后每个房间抽屉出现"调试"Tab（trace、事件日志、预算水位——
              调试数据是房间局部信息，在房间上下文查看）。调试端点需服务端以 -dev
              启动才装配（桌面端默认启用）；开关只控制界面展示，本地持久化。
            </p>
            <button
              type="button"
              aria-pressed={devMode}
              onClick={() => setDevMode(!devMode)}
              className={`shrink-0 rounded-xl border px-3.5 py-2 text-sm transition-colors ${
                devMode
                  ? "border-accent bg-accent-soft text-accent"
                  : "border-border bg-surface-2 text-text hover:border-faint"
              }`}
            >
              {devMode ? "已开启" : "已关闭"}
            </button>
          </div>
        </section>
      </div>
    </div>
  );
}

function ExeBadge({
  children,
  tone = "dim",
}: {
  children: React.ReactNode;
  tone?: "ok" | "warn" | "dim";
}) {
  const cls =
    tone === "ok"
      ? "bg-accent-soft text-accent"
      : tone === "warn"
        ? "bg-[color-mix(in_srgb,var(--warn)_14%,transparent)] text-warn"
        : "bg-surface-3 text-dim";
  return <span className={`rounded px-1.5 py-px text-[10px] leading-4 ${cls}`}>{children}</span>;
}
