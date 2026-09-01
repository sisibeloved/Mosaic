// 设置页：实例（harness 可执行项管理 + 手动登记）/ 策略（当前房间三模式预设，
// 沿用 set_policy 命令链）/ 外观（主题切换）/ 开发者（MOSAIC_DEV 时 DevPanel）。
import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, ApiError, type Executable, type PolicyParams } from "../api/client";
import { DevPanel } from "../components/DevPanel";
import { getLastRoomId } from "../state/rooms";
import { useTheme } from "../state/theme";
import { adapterLabel, channelLabel } from "../lib/copy";
import { truncate } from "../lib/ui";

declare const MOSAIC_DEV: boolean;

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

/** 三模式产品面（B1 参数束；review/decision 为收束模式，随 M3 面板开放）。 */
const MODES: { id: string; label: string; desc: string }[] = [
  { id: "open_floor", label: "Open Floor", desc: "开放讨论（默认 3 人/轮，20s 窗口）" },
  { id: "roundtable", label: "Roundtable", desc: "圆桌（全员各 1，30s 窗口）" },
  { id: "deep_dive", label: "Deep Dive", desc: "深潜（2 人/轮，15s 窗口，900 cap）" },
];

/** 模式默认参数束（与房间侧 policyDefaults 同源；提交前服务端再校验）。 */
function modeDefaults(mode: string): PolicyParams {
  const base: PolicyParams = {
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

export function SettingsPage() {
  const roomID = getLastRoomId();
  const [theme, setTheme] = useTheme();
  const [executables, setExecutables] = useState<Executable[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [policy, setPolicy] = useState<SnapshotPolicy | null>(null);
  const [policyMsg, setPolicyMsg] = useState<string | null>(null);
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

  const refreshPolicy = useCallback(async () => {
    if (!roomID) {
      setPolicy(null);
      return;
    }
    try {
      const snap = await api.snapshot(roomID);
      setPolicy(snap.policy ?? null);
    } catch {
      setPolicy(null);
    }
  }, [roomID]);

  useEffect(() => {
    void refreshPolicy();
  }, [refreshPolicy]);

  const applyMode = async (mode: string) => {
    if (!roomID) return;
    setBusy("policy");
    setPolicyMsg(null);
    try {
      const snap = await api.snapshot(roomID); // 命令前版本校准；409 兜底一次
      try {
        await api.setPolicy(roomID, snap.room_version, modeDefaults(mode));
      } catch (e) {
        if (e instanceof ApiError && e.status === 409) {
          const fresh = await api.snapshot(roomID);
          await api.setPolicy(roomID, fresh.room_version, modeDefaults(mode));
        } else {
          throw e;
        }
      }
      await refreshPolicy();
      setPolicyMsg("已生效（下一轮起）");
    } catch (e) {
      setPolicyMsg(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  };

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
                  <li key={exe.id} className="flex items-center gap-3 px-3 py-2.5">
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
          <h2 className="mb-1 text-sm font-medium">讨论策略</h2>
          {!roomID ? (
            <p className="text-xs text-faint">进入一个房间后在此配置讨论模式（变更在下一轮生效）。</p>
          ) : (
            <>
              <p className="mb-3 text-xs text-faint">
                作用于当前房间（
                <Link to={`/rooms/${roomID}`} className="text-accent hover:underline">
                  返回房间
                </Link>
                ）。当前：
                {policy
                  ? `${policy.mode}（${policy.policy_version}）· 单轮 ≤${policy.max_speakers} 人 · 窗口 ${policy.intent_window} · cap ${policy.response_cap} · reveal ${policy.reveal_strategy}`
                  : "读取中…"}
              </p>
              <div className="flex flex-wrap gap-2">
                {MODES.map((m) => {
                  const active = policy?.mode === m.id;
                  return (
                    <button
                      key={m.id}
                      type="button"
                      disabled={busy === "policy"}
                      title={m.desc}
                      onClick={() => void applyMode(m.id)}
                      className={`rounded-xl border px-3.5 py-2 text-left transition-colors disabled:opacity-40 ${
                        active
                          ? "border-accent bg-accent-soft"
                          : "border-border bg-surface-2 hover:border-faint"
                      }`}
                    >
                      <span className={`block text-sm ${active ? "text-accent" : "text-text"}`}>{m.label}</span>
                      <span className="block text-[11px] text-faint">{m.desc}</span>
                    </button>
                  );
                })}
              </div>
              {policyMsg && <p className="mt-2 text-xs text-dim">{policyMsg}</p>}
              <p className="mt-2 text-[11px] text-faint">
                权重/λ/续聊等细参数编辑随记分卡面板开放；reveal 策略随模式默认（Roundtable 含 1 轮 cross 交锋）。
              </p>
            </>
          )}
        </section>

        <section>
          <h2 className="mb-1 text-sm font-medium">外观</h2>
          <div className="flex gap-2">
            {(
              [
                { id: "dark", label: "暗色（默认）" },
                { id: "light", label: "亮色" },
              ] as const
            ).map((t) => (
              <button
                key={t.id}
                type="button"
                onClick={() => setTheme(t.id)}
                className={`rounded-xl border px-3.5 py-2 text-sm transition-colors ${
                  theme === t.id
                    ? "border-accent bg-accent-soft text-accent"
                    : "border-border bg-surface-2 text-text hover:border-faint"
                }`}
              >
                {t.label}
              </button>
            ))}
          </div>
        </section>

        {MOSAIC_DEV && (
          <section>
            <h2 className="mb-1 text-sm font-medium">开发者</h2>
            <DevPanel roomID={roomID} />
          </section>
        )}
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
