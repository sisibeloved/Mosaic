// Agent CLI 版本检测与更新（0.4.1——负责人指令"本机使用的 CLI 通常不是最新的"，
// 原 0.7.0 生态项前移）：实例行内联的检查更新/更新面。
// 通道（后端实证口径）：claude 官方自更新（claude update）；kimi 官方自更新
// （kimi upgrade --yes）；codex/minimax npm 分发（npm view 查最新 / npm i -g
// 更新，登录 shell 形态）；zcode releases（产品更新源优先、源码 tag 回退）+
// manual 更新（自建打包）。
// 更新过程可视化（验收反馈轮）：POST 受理即返（202 异步任务），子进程输出经
// GET .../update/stream SSE 逐行转发——npm 下载进度/自更新输出实时呈现在日志
// 控制台（自动滚底），终态（done/failed）后回填版本。
import { useEffect, useRef, useState } from "react";
import { api, type Executable, type HarnessUpdateCheck } from "../api/client";

const LOG_LINE_CAP = 400; // 控制台保留尾部行（服务端环形 1000，前端再收口）

export function UpdateEditor({ exe, onUpdated }: { exe: Executable; onUpdated: () => void }) {
  const [check, setCheck] = useState<HarnessUpdateCheck | null>(null);
  const [checking, setChecking] = useState(false);
  const [updating, setUpdating] = useState(false);
  const [logLines, setLogLines] = useState<string[]>([]);
  const [logOpen, setLogOpen] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const logRef = useRef<HTMLDivElement | null>(null);
  const esRef = useRef<EventSource | null>(null);

  useEffect(() => {
    return () => esRef.current?.close(); // 行卸载（切换实例/页面）即断订阅
  }, []);

  useEffect(() => {
    // 自动滚底：新行到达时贴底（用户上翻查阅历史不被强拉——贴底判定）
    const el = logRef.current;
    if (el && logOpen) {
      const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
      if (atBottom || logLines.length < 3) el.scrollTop = el.scrollHeight;
    }
  }, [logLines, logOpen]);

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

  // subscribeStream 订阅过程日志 SSE（done/failed/idle 终态关流；idle=服务端
  // 无任务——连接先于任务注册的瞬态，延时重试一次）。
  const subscribeStream = () => {
    esRef.current?.close();
    const es = new EventSource(
      `/v1/harness/executables/${encodeURIComponent(exe.id)}/update/stream`,
    );
    esRef.current = es;
    es.onmessage = (msg) => {
      let ev: { kind: string; line?: string; new_version?: string; error?: string };
      try {
        ev = JSON.parse(msg.data);
      } catch {
        return;
      }
      if (ev.kind === "line" && ev.line) {
        setLogLines((prev) => {
          const next = [...prev, ev.line as string];
          return next.length > LOG_LINE_CAP ? next.slice(-LOG_LINE_CAP) : next;
        });
      } else if (ev.kind === "done") {
        setDone(`已更新到 ${ev.new_version ?? ""}`);
        setUpdating(false);
        es.close();
        onUpdated(); // 版本字样随注册表回填刷新
      } else if (ev.kind === "failed") {
        setError(ev.error ?? "更新失败");
        setUpdating(false);
        es.close();
      } else if (ev.kind === "idle") {
        es.close();
        const self = es;
        setTimeout(() => {
          if (esRef.current === self) subscribeStream();
        }, 600);
      }
    };
  };

  const runUpdate = async () => {
    setUpdating(true);
    setError(null);
    setDone(null);
    setLogLines([]);
    setLogOpen(true);
    try {
      await api.executableUpdate(exe.id); // 202 受理（started/running）
      subscribeStream();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setUpdating(false);
    }
  };

  const hasUpdate = check && check.latest && !check.up_to_date;
  // 已知最新（can_check 且比对为最新）→ 更新按钮禁用（验收反馈①）。无检测面的
  // 家族（kimi）不知最新态——按钮保持可用，title 说明幂等升级语义。
  const knownCurrent = !!check && check.can_check && check.up_to_date;

  return (
    <div className="flex flex-col gap-1.5 text-xs">
      <div className="flex flex-wrap items-center gap-2">
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
                {updating ? "更新中…" : "更新到最新"}
              </button>
            )}
            {check.note && !hasUpdate && (
              <span className="text-[11px] text-faint">{truncateStr(check.note, 110)}</span>
            )}
          </>
        )}
        {done && <span className="text-[11px] text-ok">{done}</span>}
        {error && <span className="text-[11px] text-danger">{truncateStr(error, 90)}</span>}
        {logLines.length > 0 && (
          <button
            type="button"
            onClick={() => setLogOpen((v) => !v)}
            className="ml-auto rounded bg-surface-3 px-1.5 py-px text-[10px] text-dim"
          >
            {logOpen ? "收起日志" : `展开日志（${logLines.length} 行）`}
          </button>
        )}
      </div>
      {logLines.length > 0 && logOpen && (
        <div
          ref={logRef}
          className="max-h-44 overflow-auto rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-[10.5px] leading-[1.55] text-dim"
        >
          {logLines.map((line, i) => (
            <div key={i} className={line.startsWith("$ ") || line.startsWith("> ") ? "text-accent" : undefined}>
              {line || "\u00a0"}
            </div>
          ))}
          {updating && <div className="animate-pulse text-faint">▍</div>}
        </div>
      )}
    </div>
  );
}

function truncateStr(s: string, n: number) {
  const runes = Array.from(s);
  return runes.length <= n ? s : runes.slice(0, n).join("") + "…";
}
