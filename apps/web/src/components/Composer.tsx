import { useState } from "react";

export function Composer({
  disabled,
  addressTarget,
  onClearAddress,
  onSend,
}: {
  disabled: boolean;
  addressTarget: string | null;
  onClearAddress: () => void;
  onSend: (body: string, addressedTo: string[]) => void;
}) {
  const [body, setBody] = useState("");
  const submit = () => {
    const text = body.trim();
    if (!text || disabled) return;
    onSend(text, addressTarget ? [addressTarget] : []);
    setBody("");
    onClearAddress();
  };
  return (
    <form
      className="composer"
      onSubmit={(e) => {
        e.preventDefault();
        submit();
      }}
    >
      <div className="composer-main">
        {addressTarget && (
          <button type="button" className="ghost address-chip" onClick={onClearAddress} title="取消点名">
            → {addressTarget} ✕
          </button>
        )}
        <input
          value={body}
          placeholder={disabled ? "先创建房间" : addressTarget ? `点名回应 ${addressTarget}…` : "输入消息，回车发送（点消息作者可点名）"}
          disabled={disabled}
          autoComplete="off"
          onChange={(e) => setBody(e.target.value)}
        />
      </div>
      <button type="submit" disabled={disabled || !body.trim()}>
        发送
      </button>
    </form>
  );
}
