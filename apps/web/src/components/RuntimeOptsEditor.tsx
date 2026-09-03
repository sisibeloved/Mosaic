// Agent 实例运行参数编辑器（v1.48：模型覆盖与思考强度）：设置页实例行展开。
// 模型候选：kimi 为 CLI 实查（provider list --json，动态）；codex/mcode 无官方
// 列表命令（空候选+自由输入，placeholder 示例）。强度档位：仅 codex（五档）。
// 保存 = PUT 全量替换（空 = 清除覆盖回 CLI 默认）；座位 ≤10s resync 生效。
import { useEffect, useState } from "react";
import { api, type Executable, type RuntimeOptions } from "../api/client";

export function RuntimeOptsEditor({ exe }: { exe: Executable }) {
  const [open, setOpen] = useState(false);
  const [opts, setOpts] = useState<RuntimeOptions | null>(null);
  const [model, setModel] = useState(exe.model ?? "");
  const [effort, setEffort] = useState(exe.reasoning_effort ?? "");
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // 展开时拉候选（kimi 实查秒级；其余静态空面——直接呈现）
  useEffect(() => {
    if (!open || opts !== null) return;
    api
      .executableModels(exe.id)
      .then(setOpts)
      .catch(() => setOpts({ models: [], dynamic: false, effort_levels: [] }));
  }, [open, exe.id, opts]);

  const modelPlaceholder =
    exe.adapter === "codex" ? "如 gpt-5.6-sol（留空 = CLI 默认）"
    : exe.adapter === "minimax" ? "如 minimax/MiniMax-M2（留空 = CLI 默认）"
    : "留空 = CLI 默认";

  const save = async () => {
    setBusy(true);
    setError(null);
    setSaved(false);
    try {
      await api.updateExecutable(exe.id, model.trim(), effort);
      setSaved(true);
      setTimeout(() => setSaved(false), 2500);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const configured = (exe.model ?? "") !== "" || (exe.reasoning_effort ?? "") !== "";

  return (
    <div className="w-full">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-1 text-[11px] text-dim transition-colors hover:text-text"
      >
        <span className={`inline-block transition-transform ${open ? "rotate-90" : ""}`}>▸</span>
        运行参数
        {configured && (
          <span className="rounded bg-accent-soft px-1.5 text-[10px] leading-4 text-accent">已覆盖</span>
        )}
      </button>
      {open && (
        <div className="mt-2 flex flex-col gap-2 rounded-lg bg-surface-2 p-2.5">
          <label className="flex flex-col gap-1 text-[11px] text-faint">
            模型{opts?.dynamic ? "（CLI 实查候选，可直接输入）" : "（自由输入，留空 = CLI 默认）"}
            <input
              value={model}
              onChange={(e) => setModel(e.target.value)}
              placeholder={modelPlaceholder}
              list={`models-${exe.id}`}
              maxLength={120}
              className="rounded-lg border border-border bg-surface px-2 py-1.5 font-mono text-xs text-text outline-none focus:border-accent"
            />
            <datalist id={`models-${exe.id}`}>
              {(opts?.models ?? []).map((m) => (
                <option key={m.id} value={m.id}>
                  {m.display_name ?? m.id}
                </option>
              ))}
            </datalist>
            {opts?.dynamic && (opts.models?.length ?? 0) > 0 && (
              <span className="text-faint">
                候选：{opts.models.map((m) => m.display_name ? `${m.display_name}（${m.id}）` : m.id).join(" · ")}
              </span>
            )}
          </label>
          {(opts?.effort_levels?.length ?? 0) > 0 ? (
            <label className="flex flex-col gap-1 text-[11px] text-faint">
              思考强度（留空 = CLI 默认）
              <select
                value={effort}
                onChange={(e) => setEffort(e.target.value)}
                className="rounded-lg border border-border bg-surface px-2 py-1.5 text-xs text-text outline-none focus:border-accent"
              >
                <option value="">CLI 默认</option>
                {opts?.effort_levels.map((lv) => (
                  <option key={lv} value={lv}>
                    {lv}
                  </option>
                ))}
              </select>
            </label>
          ) : (
            <p className="text-[11px] text-faint">
              {exe.adapter === "kimi" ? "思考内建于 Kimi 模型能力（无用户档位）" : "该 CLI 暂无思考强度档位"}
            </p>
          )}
          <div className="flex items-center gap-2">
            <button
              type="button"
              disabled={busy}
              onClick={() => void save()}
              className="rounded-lg bg-surface-3 px-2.5 py-1 text-[11px] text-text transition-opacity hover:opacity-85 disabled:opacity-40"
            >
              {busy ? "保存中…" : "保存"}
            </button>
            {saved && <span className="text-[11px] text-ok">已保存（≤10 秒生效于后续发言）</span>}
            {error && <span className="text-[11px] text-danger">{error}</span>}
          </div>
        </div>
      )}
    </div>
  );
}
