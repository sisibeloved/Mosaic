import { useState } from "react";

export function Composer({
  disabled,
  onSend,
}: {
  disabled: boolean;
  onSend: (body: string) => void;
}) {
  const [body, setBody] = useState("");
  const submit = () => {
    const text = body.trim();
    if (!text || disabled) return;
    onSend(text);
    setBody("");
  };
  return (
    <form
      className="composer"
      onSubmit={(e) => {
        e.preventDefault();
        submit();
      }}
    >
      <input
        value={body}
        placeholder={disabled ? "先创建房间" : "输入消息，回车发送"}
        disabled={disabled}
        autoComplete="off"
        onChange={(e) => setBody(e.target.value)}
      />
      <button type="submit" disabled={disabled || !body.trim()}>
        发送
      </button>
    </form>
  );
}
