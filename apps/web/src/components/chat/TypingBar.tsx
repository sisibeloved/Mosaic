// "正在输入"区（计划 v1.11 静默期反馈 + draft.update 草稿流预览）：
// 座位级条目——头像小圆点 + 阶段文案 + 草稿预览（渐显、斜体、截断 140 字）；无活动时隐藏。
import type { TypingState } from "../../api/room";
import type { ParticipantView } from "../../api/client";
import { displayNameOf, truncate } from "../../lib/ui";
import { Avatar } from "./Avatar";

const PHASE_TEXT: Record<TypingState["phase"], string> = {
  queued: "排队中",
  evaluating: "正在评估",
  generating: "正在生成",
  validating: "正在校验",
  drafting: "正在起草",
};

export function TypingBar({
  typing,
  participants,
}: {
  typing: Record<string, TypingState>;
  participants: ParticipantView[];
}) {
  const pids = Object.keys(typing);
  if (pids.length === 0) return null;
  return (
    <div aria-live="polite" className="border-t border-border px-4 py-1.5">
      <div className="mx-auto flex max-w-3xl flex-col gap-1">
        {pids.map((pid) => {
          const t = typing[pid];
          const name = displayNameOf(participants, pid);
          return (
            <div key={pid} className="animate-fade-in flex items-baseline gap-2 text-xs">
              <span className="flex shrink-0 items-center gap-1.5 text-dim">
                <Avatar participantID={pid} displayName={name} size={16} />
                <span className="font-medium text-text">{name}</span>
                <span className="typing-dots">{PHASE_TEXT[t.phase]}</span>
              </span>
              {t.text && (
                <span className="draft-text min-w-0 truncate italic text-faint">
                  {truncate(t.text, 140)}
                </span>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}
