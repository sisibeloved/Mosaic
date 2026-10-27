// Agent CLI 版本检测与更新（0.4.1——负责人指令"本机使用的 CLI 通常不是最新的"，
// 原 0.7.0 生态项前移）：实例行内联的检查更新/更新面。
// 通道（后端实证口径）：claude 官方自更新（claude update）；codex/minimax npm
// 分发（npm view 查最新 / npm i -g 更新，登录 shell 形态）；kimi/zcode manual
// （检测面 can_check=false，note 携手动指引——如实呈现不虚构按钮）。
// 更新为同步执行（npm install 分钟级）——按钮进入"更新中…"态，完成后回填版本。
import { useState } from "react";
import { api, type Executable, type HarnessUpdateCheck } from "../api/client";

export function UpdateEditor({ exe, onUpdated }: { exe: Executable; onUpdated: () => void }) {
  const [check, setCheck] = useState<HarnessUpdateCheck | null>(null);
  const [checking, setChecking] = useState(false);
  const [updating, setUpdating] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);

  const runCheck = async () => {
    setChecking(true);
    setError(null);
    setDone(null);
    try {
      setCheck(await api.executableUpdateCheck(exe.id));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setChecking(false);
    }
  };

  const runUpdate = async () => {
    setUpdating(true);
    setError(null);
    setDone(null);
    try {
      const res = await api.executableUpdate(exe.id);
      setDone(`已更新到 ${res.new_version}`);
      onUpdated(); // 版本字样随注册表回填刷新
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setUpdating(false);
    }
  };

  const hasUpdate = check && check.latest && !check.up_to_date;
  // 已知最新（can_check 且比对为最新）→ 更新按钮禁用（0.4.1 验收反馈①：mcode
  // 已最新不可点）。无检测面的家族（kimi 官方安装器自更新）不知最新态——按钮
  // 保持可用，title 说明幂等升级语义。
  const knownCurrent = !!check && check.can_check && check.up_to_date;

  return (
    <div className="flex flex-wrap items-center gap-2 text-xs">
      <button
        type="button"
        disabled={checking || updating}
        onClick={() => void runCheck()}
        className="rounded-lg bg-surface-3 px-2 py-0.5 text-[11px] text-text transition-opacity hover:opacity-85 disabled:opacity-40"
      >
        {checking ? "检测中…" : "检查更新"}
      </button>
      {check && !checking && (
        <>
          {check.comparable && check.up_to_date ? (
            <span className="text-[11px] text-faint">已是最新（{check.latest}）</span>
          ) : hasUpdate ? (
            <span className="rounded-md bg-[color-mix(in_srgb,var(--warn)_14%,transparent)] px-1.5 py-px text-[11px] text-warn">
              新版本 {check.latest}
            </span>
          ) : null}
          {check.can_update && (
            <button
              type="button"
              disabled={updating || knownCurrent}
              title={
                knownCurrent
                  ? "已是最新版本"
                  : check.can_check
                    ? "检测失败或未知——重装最新版（幂等）"
                    : "该 CLI 无独立检测面——更新命令幂等执行，已是最新则无动作"
              }
              onClick={() => void runUpdate()}
              className="rounded-lg bg-accent-soft px-2 py-0.5 text-[11px] text-accent transition-opacity hover:opacity-85 disabled:opacity-40"
            >
              {updating ? "更新中…（可能需要一两分钟）" : "更新到最新"}
            </button>
          )}
          {check.note && !hasUpdate && <span className="text-[11px] text-faint">{check.note}</span>}
        </>
      )}
      {done && <span className="text-[11px] text-ok">{done}</span>}
      {error && <span className="text-[11px] text-danger">{truncateStr(error, 90)}</span>}
    </div>
  );
}

function truncateStr(s: string, n: number) {
  const runes = Array.from(s);
  return runes.length <= n ? s : runes.slice(0, n).join("") + "…";
}
