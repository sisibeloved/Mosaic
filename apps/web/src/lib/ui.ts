// 聊天 UI 共享小工具：相对时间、头像色/首字母、显示名解析。
import type { ParticipantView } from "../api/client";

/** 相对时间：刚刚 / N 分钟前 / N 小时前 / 昨天 HH:mm / 当天 HH:mm / 更早 MM-DD。 */
export function relativeTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return "";
  const diff = Date.now() - t;
  if (diff < 60_000) return "刚刚";
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`;
  const d = new Date(t);
  const now = new Date();
  const hhmm = `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
  const dayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  if (t >= dayStart) return hhmm;
  if (t >= dayStart - 86_400_000) return `昨天 ${hhmm}`;
  return `${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")} ${hhmm}`;
}

/** participant_id 哈希定色（同一座位跨房间/会话稳定）。 */
export function avatarColor(participantID: string): string {
  let h = 0;
  for (let i = 0; i < participantID.length; i++) {
    h = (h * 31 + participantID.charCodeAt(i)) % 360;
  }
  return `hsl(${h} 55% 48%)`;
}

/** 头像首字符：显示名首字母大写。 */
export function avatarInitial(displayName: string): string {
  const ch = [...displayName.trim()][0];
  return ch ? ch.toUpperCase() : "?";
}

/** participant_id → 显示名（快照参与者表查不到时退化为短 id）。 */
export function displayNameOf(participants: ParticipantView[], participantID: string): string {
  const hit = participants.find((p) => p.participant_id === participantID);
  if (hit) return hit.display_name;
  return shortId(participantID);
}

export function participantOf(
  participants: ParticipantView[],
  participantID: string,
): ParticipantView | undefined {
  return participants.find((p) => p.participant_id === participantID);
}

/** par_codex_codex_native__home_… → codex_codex_native…（最多 24 字符）。 */
export function shortId(id: string): string {
  const stripped = id.replace(/^par_/, "");
  return stripped.length > 24 ? `${stripped.slice(0, 24)}…` : stripped;
}

export function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n)}…` : s;
}
