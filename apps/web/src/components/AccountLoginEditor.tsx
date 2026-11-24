// zcode 账号登录（独立登录档——OAuth 账号 provider 的设置页入口）：Mosaic 独立驱动的
// zcode 内核另立一份登录态（~/.mosaic/zcode-standalone，与 ZCode 桌面版凭据互不相干）；
// 已登录后个人清单外的模型经账号通道调用。流程同更新面（UpdateEditor）：POST 受理
// 即返（202 异步任务），授权链接与结果经 GET .../account-login/stream SSE 推送——链接
// 到达即请桌面壳调系统浏览器（服务端形态无壳：按钮/复制链接兜底），终态后回填状态。
import { useCallback, useEffect, useRef, useState } from "react";
import {
  api,
  type Executable,
  type HarnessAccountLoginEvent,
  type HarnessAccountLoginFamily,
  type HarnessAccountLoginStatus,
} from "../api/client";

const FAMILIES: { id: HarnessAccountLoginFamily; label: string }[] = [
  { id: "bigmodel", label: "BigModel" },
  { id: "zai", label: "Z.AI" },
];

// 内核 login 子命令的授权窗口：5 分钟超时（超时为 failed 终态事件）。
const AUTH_WINDOW_HINT = "请在 5 分钟内完成浏览器授权";
// 订阅遇 idle（服务端尚无任务）的重试上限。
const IDLE_RETRY_MAX = 5;

export function AccountLoginEditor({
  exe,
  onLoggedIn,
}: {
  exe: Executable;
  onLoggedIn: () => void;
}) {
  const [status, setStatus] = useState<HarnessAccountLoginStatus | null>(null);
  const [pending, setPending] = useState<HarnessAccountLoginFamily | null>(null); // 进行中的登录家族
  const [url, setUrl] = useState<string | null>(null); // 授权链接（url 事件到达后）
  const [lastLine, setLastLine] = useState<string | null>(null); // 等待期间内核最近一行输出
  const [result, setResult] = useState<string | null>(null); // 终态呈现（成功/失败）
  const [copied, setCopied] = useState(false);
  const esRef = useRef<EventSource | null>(null);

  const refreshStatus = useCallback(async () => {
    try {
      setStatus(await api.executableAccountLogin(exe.id));
    } catch {
      setStatus(null); // 状态读不到 = 不呈现（不虚构登录态）
    }
  }, [exe.id]);

  useEffect(() => {
    void refreshStatus();
    return () => esRef.current?.close(); // 行卸载（切换实例/页面）即断订阅
  }, [refreshStatus]);

  // 桌面壳调系统浏览器（POST /v1/shell/open-external，仅壳装配存在）；失败静默——
  // 链接本身与"复制链接"始终呈现，服务端形态由用户自行打开。
  const openInBrowser = async (u: string) => {
    try {
      await api.shellOpenExternal(u);
    } catch {
      /* 无桌面壳 */
    }
  };

  // finish 登录任务终态收口：关订阅（否则 EventSource 对服务端关流自动重连并重放
  // 旧任务）、清等待态。
  const finish = () => {
    esRef.current?.close();
    esRef.current = null;
    setPending(null);
    setUrl(null);
    setLastLine(null);
  };

  // subscribeStream 订阅登录事件 SSE（done/failed/idle 终态关流；idle=服务端无任务
  // ——连接先于任务注册的瞬态，延时重试；重试有界——任务迟迟不出现则显式失败，不
  // 无限轮询）。
  const subscribeStream = (family: HarnessAccountLoginFamily, attempt = 0) => {
    esRef.current?.close();
    const es = new EventSource(
      `/v1/harness/executables/${encodeURIComponent(exe.id)}/account-login/stream`,
    );
    esRef.current = es;
    es.onmessage = (msg) => {
      let ev: HarnessAccountLoginEvent;
      try {
        ev = JSON.parse(msg.data) as HarnessAccountLoginEvent;
      } catch {
        return;
      }
      if (ev.kind === "url" && ev.url) {
        setUrl(ev.url);
        void openInBrowser(ev.url);
      } else if (ev.kind === "line" && ev.line) {
        setLastLine(ev.line);
      } else if (ev.kind === "done") {
        const model = ev.model ? ev.model.split("/").pop() : "";
        setResult(`已登录${ev.account ? `：${ev.account}` : ""}${model ? ` · ${model}` : ""}`);
        finish();
        void refreshStatus();
        onLoggedIn();
      } else if (ev.kind === "failed") {
        setResult(loginErrorText(ev.error));
        finish();
      } else if (ev.kind === "idle") {
        es.close();
        if (esRef.current === es) esRef.current = null;
        if (attempt >= IDLE_RETRY_MAX) {
          setResult("登录任务未就绪——请重新发起登录");
          finish();
          return;
        }
        setTimeout(() => {
          if (esRef.current === null) subscribeStream(family, attempt + 1);
        }, 600);
      }
    };
  };

  const runLogin = async (family: HarnessAccountLoginFamily) => {
    setResult(null);
    setUrl(null);
    setLastLine(null);
    setCopied(false);
    setPending(family);
    try {
      await api.executableAccountLoginStart(exe.id, family); // 202 受理（started/running）
      subscribeStream(family);
    } catch (e) {
      setResult(e instanceof Error ? e.message : String(e));
      setPending(null);
    }
  };

  const copyLink = async (u: string) => {
    try {
      await navigator.clipboard.writeText(u);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      /* 剪贴板不可用——链接本身可选中文本 */
    }
  };

  if (!status || !status.supported) return null;

  const modelShort = status.model ? status.model.split("/").pop() : "";
  const familyLabel = FAMILIES.find((f) => f.id === pending)?.label ?? "";
  const failed = pending === null && result !== null && !result.startsWith("已登录");
  return (
    <div className="flex flex-col gap-1.5 text-xs">
      <div className="flex flex-wrap items-center gap-2">
        <span
          className="text-[11px] text-faint"
          title="登录态独立于 ZCode 桌面版，保存在 ~/.mosaic/zcode-standalone；个人清单外的模型经此账号通道调用"
        >
          账号登录
        </span>
        {status.logged_in ? (
          <span
            className="rounded-md bg-[color-mix(in_srgb,var(--ok)_14%,transparent)] px-1.5 py-px text-[11px] text-ok"
            title={status.model || undefined}
          >
            已登录{status.account ? ` ${status.account}` : ""}
            {modelShort ? ` · ${modelShort}` : ""}
          </span>
        ) : (
          <span className="rounded-md bg-[color-mix(in_srgb,var(--warn)_14%,transparent)] px-1.5 py-px text-[11px] text-warn">
            未登录
          </span>
        )}
        {FAMILIES.map((f) => (
          <button
            key={f.id}
            type="button"
            disabled={pending !== null}
            onClick={() => void runLogin(f.id)}
            className="rounded-lg bg-surface-3 px-2 py-0.5 text-[11px] text-text transition-opacity hover:opacity-85 disabled:opacity-40"
          >
            {status.logged_in ? `重新登录 ${f.label}` : `登录 ${f.label} 账号`}
          </button>
        ))}
        {pending === null && result && (
          <span className={failed ? "text-[11px] text-danger" : "text-[11px] text-ok"}>
            {truncateStr(result, 160)}
          </span>
        )}
      </div>
      {pending && (
        <div className="rounded-lg border border-border bg-surface-2 px-2.5 py-2">
          {url ? (
            <>
              <div className="text-text">
                已发起 {familyLabel} 账号登录——{AUTH_WINDOW_HINT}。
              </div>
              <div className="mt-1.5 flex flex-wrap items-center gap-2">
                <button
                  type="button"
                  onClick={() => void openInBrowser(url)}
                  className="rounded-lg bg-accent-soft px-2 py-0.5 text-[11px] text-accent transition-opacity hover:opacity-85"
                >
                  在浏览器中打开授权页
                </button>
                <button
                  type="button"
                  onClick={() => void copyLink(url)}
                  className="rounded-lg bg-surface-3 px-2 py-0.5 text-[11px] text-text transition-opacity hover:opacity-85"
                >
                  {copied ? "已复制" : "复制链接"}
                </button>
                <span className="animate-pulse text-[11px] text-faint">等待浏览器授权…</span>
              </div>
              <div className="mt-1.5 break-all font-mono text-[10.5px] leading-[1.55] text-faint select-all">
                {url}
              </div>
            </>
          ) : (
            <div className="animate-pulse text-dim">正在发起登录…</div>
          )}
          {lastLine && <div className="mt-1 font-mono text-[10.5px] text-faint">{lastLine}</div>}
        </div>
      )}
    </div>
  );
}

// loginErrorText 失败原因呈现：已知的内核超时给中文说明（其余原样——内核原话是
// 排障依据，不翻译不吞）。
function loginErrorText(msg: string | undefined): string {
  if (!msg) return "登录失败";
  if (/authorization timed out/i.test(msg)) {
    return "授权超时：5 分钟内未在浏览器完成确认——请重新发起登录";
  }
  return msg;
}

function truncateStr(s: string, n: number) {
  const runes = Array.from(s);
  return runes.length <= n ? s : runes.slice(0, n).join("") + "…";
}
