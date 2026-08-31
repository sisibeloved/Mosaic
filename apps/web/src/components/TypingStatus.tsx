// 静默期进行中状态（计划 v1.11）：round.opened → 房间级"评估中"横幅；
// floor.granted → 座位级"正在生成"；draft.update 瞬态帧携带文本。
import type { TypingState } from "../api/room";

const PHASE_TEXT: Record<TypingState["phase"], string> = {
  evaluating: "评估中",
  generating: "正在生成",
  drafting: "草稿中",
};

export function TypingStatus({
  roundOpen,
  typing,
}: {
  roundOpen: boolean;
  typing: Record<string, TypingState>;
}) {
  const pids = Object.keys(typing);
  if (!roundOpen && pids.length === 0) return null;
  return (
    <div className="typing-bar" aria-live="polite">
      {roundOpen && pids.length === 0 && (
        <span className="typing-pill room">评估中——各座位并行评估发言意向…</span>
      )}
      {pids.map((pid) => (
        <span key={pid} className="typing-pill">
          <span className="typing-name">{shortName(pid)}</span>
          <span className="typing-dots">{PHASE_TEXT[typing[pid].phase]}…</span>
          {typing[pid].text && <span className="typing-text">{typing[pid].text}</span>}
        </span>
      ))}
    </div>
  );
}

function shortName(pid: string): string {
  // par_codex_codex_native__home_… → codex…；par_echo → echo
  const stripped = pid.replace(/^par_/, "");
  return stripped.length > 24 ? `${stripped.slice(0, 24)}…` : stripped;
}
