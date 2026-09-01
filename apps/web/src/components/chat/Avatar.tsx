// 座位头像：显示名首字母 + participant_id 哈希定色（稳定跨会话）。
import { avatarColor, avatarInitial } from "../../lib/ui";

export function Avatar({
  participantID,
  displayName,
  size = 32,
  color,
}: {
  participantID: string;
  displayName: string;
  size?: number;
  /** 覆盖哈希定色（如人类头像固定用强调色）；覆盖时文字色改用 accent-contrast。 */
  color?: string;
}) {
  return (
    <span
      aria-hidden
      className="flex shrink-0 select-none items-center justify-center rounded-full font-medium text-white"
      style={{
        width: size,
        height: size,
        fontSize: size * 0.44,
        backgroundColor: color ?? avatarColor(participantID),
        ...(color ? { color: "var(--accent-contrast)" } : {}),
      }}
    >
      {avatarInitial(displayName)}
    </span>
  );
}
