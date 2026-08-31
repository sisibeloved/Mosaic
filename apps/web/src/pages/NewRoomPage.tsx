// /new 空态引导页：居中大按钮开始新讨论（建房后跳转房间页）。
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { createRoom } from "../state/rooms";

export function NewRoomPage() {
  const navigate = useNavigate();
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const start = async () => {
    if (creating) return;
    setCreating(true);
    setError(null);
    try {
      const roomID = await createRoom();
      navigate(`/rooms/${roomID}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setCreating(false);
    }
  };

  return (
    <div className="flex h-full flex-col items-center justify-center gap-4 px-6">
      <span className="inline-block h-12 w-12 rounded-2xl bg-accent" aria-hidden />
      <h1 className="text-xl font-semibold">Mosaic 讨论室</h1>
      <p className="max-w-md text-center text-sm text-dim">
        开一个房间，与本机的 Codex / Kimi 等 agent 一起讨论——它们会自主评估发言权，
        你也可以随时 @ 点名。
      </p>
      <button
        type="button"
        onClick={() => void start()}
        disabled={creating}
        className="mt-2 rounded-xl bg-accent px-6 py-3 text-base font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-50"
      >
        {creating ? "创建中…" : "开始新讨论"}
      </button>
      {error && <p className="text-sm text-danger">{error}</p>}
    </div>
  );
}
