// 活动 Tab（M4-2 协作状态与用户反馈）：不用开发者日志即可区分"不想说 / 还在做 /
// 失败了"。最近一轮各座结果自权威事件重建（intent.recorded 的沉默与意愿 +
// message.posted 的发言——刷新/断线不丢）；座位失败是瞬态帧（断线不重建，标注时点）。
// 语义纪律（M4-2 条款）：静默 ≠ 共识；失败/超时 ≠ 同意。
import type { SeatFailure } from "../../api/room";
import type { ParticipantView } from "../../api/client";
import { displayNameOf, relativeTime, truncate } from "../../lib/ui";
import { Avatar } from "./Avatar";

type ScorecardEntry = {
  participant_id: string;
  action: string;
  public_rationale?: string | null;
  round_id?: string;
  occurred_at: string;
};

const FAILURE_TEXT: Record<SeatFailure["status"], string> = {
  eval_failed: "评估失败",
  generate_failed: "生成失败",
};

/** 最近一轮：取 occurred_at 最新的记分项所属 round（rnd_* ULID 时序可比，但
 * occurred_at 是更稳的判据）；每座一行——非 silent 意愿 = 已发言（撤销/失败
 * 由失败区如实呈现），silent = 主动沉默 + 公开理由。 */
function latestRoundBySeat(scorecard: ScorecardEntry[]) {
  let latestRound = "";
  let latestAt = "";
  for (const s of scorecard) {
    if (s.occurred_at >= latestAt) {
      latestRound = s.round_id ?? "";
      latestAt = s.occurred_at;
    }
  }
  const seats: { pid: string; kind: "spoke" | "silent"; rationale: string; at: string }[] = [];
  const seen = new Set<string>();
  for (const s of scorecard) {
    if ((s.round_id ?? "") !== latestRound || seen.has(s.participant_id)) continue;
    seen.add(s.participant_id);
    if (s.action === "silent") {
      seats.push({ pid: s.participant_id, kind: "silent", rationale: s.public_rationale ?? "", at: s.occurred_at });
    } else {
      seats.push({ pid: s.participant_id, kind: "spoke", rationale: "", at: s.occurred_at });
    }
  }
  return { latestRound, seats };
}

export function ActivityTab({
  scorecard,
  participants,
  failures,
  agentPids,
}: {
  scorecard: ScorecardEntry[];
  participants: ParticipantView[];
  failures: SeatFailure[];
  /** 在席 agent 座位（未出现在最近一轮记分里的座位=未参与/未评估）。 */
  agentPids: string[];
}) {
  const { seats } = latestRoundBySeat(scorecard);
  const failedPids = new Set(failures.map((f) => f.participantID));
  const covered = new Set(seats.map((s) => s.pid));

  return (
    <div className="flex flex-col gap-3 text-xs">
      <section>
        <h4 className="mb-1.5 font-medium text-text">最近一轮</h4>
        {seats.length === 0 ? (
          <p className="text-faint">还没有反应波记录。</p>
        ) : (
          <ul className="flex flex-col gap-1">
            {seats.map((s) => (
              <li key={s.pid} className="flex items-baseline gap-2">
                <Avatar participantID={s.pid} displayName={displayNameOf(participants, s.pid)} size={16} />
                <span className="font-medium text-text">{displayNameOf(participants, s.pid)}</span>
                {s.kind === "spoke" ? (
                  <span className="text-ok">已发言</span>
                ) : (
                  <>
                    <span className="text-dim">主动沉默</span>
                    {s.rationale && (
                      <span className="min-w-0 truncate text-faint" title={s.rationale}>
                        {truncate(s.rationale, 80)}
                      </span>
                    )}
                  </>
                )}
                <span className="ml-auto shrink-0 text-faint">{relativeTime(s.at)}</span>
              </li>
            ))}
            {agentPids
              .filter((pid) => !covered.has(pid) && !failedPids.has(pid))
              .map((pid) => (
                <li key={`np-${pid}`} className="flex items-baseline gap-2">
                  <Avatar participantID={pid} displayName={displayNameOf(participants, pid)} size={16} />
                  <span className="font-medium text-text">{displayNameOf(participants, pid)}</span>
                  <span className="text-faint">本轮未参与（无评估记录）</span>
                </li>
              ))}
          </ul>
        )}
      </section>

      <section>
        <h4 className="mb-1.5 font-medium text-text">本轮失败（瞬态）</h4>
        {failures.length === 0 ? (
          <p className="text-faint">无失败记录。</p>
        ) : (
          <ul className="flex flex-col gap-1">
            {failures.map((f, i) => (
              <li key={`f-${i}`} className="flex items-baseline gap-2">
                <Avatar participantID={f.participantID} displayName={displayNameOf(participants, f.participantID)} size={16} />
                <span className="font-medium text-text">{displayNameOf(participants, f.participantID)}</span>
                <span className="text-danger">{FAILURE_TEXT[f.status]}</span>
                <span className="min-w-0 truncate text-faint" title={f.detail}>
                  {truncate(f.detail, 120)}
                </span>
                <span className="ml-auto shrink-0 text-faint">{relativeTime(f.at)}</span>
              </li>
            ))}
          </ul>
        )}
        <p className="mt-1 text-[11px] leading-4 text-faint">
          失败与超时不是同意，静默不是共识；失败记录为瞬态（断线/刷新不重建，以事件流的
          发言与沉默为准）。配额耗尽/网络不可达时对应 Agent 本轮自动跳过，恢复后下一轮自动参与。
        </p>
      </section>
    </div>
  );
}
