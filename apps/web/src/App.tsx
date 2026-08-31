import { useState } from "react";
import { useRoom } from "./api/room";
import { Composer } from "./components/Composer";
import { DevPanel } from "./components/DevPanel";
import { ScorecardView } from "./components/ScorecardView";
import { SettingsView } from "./components/SettingsView";
import { Timeline } from "./components/Timeline";
import { TypingStatus } from "./components/TypingStatus";

const CONNECTION_TEXT: Record<string, string> = {
  idle: "未连接",
  connecting: "连接中…",
  live: "实时",
  reconnecting: "断线重连中…",
  resync: "快照恢复中…",
};

export function App() {
  const [view, setView] = useState<"room" | "scorecard" | "settings">("room");
  const [addressTarget, setAddressTarget] = useState<string | null>(null);
  const room = useRoom();

  return (
    <div className="app">
      <aside className="side">
        <h1>Mosaic</h1>
        <nav className="nav">
          <button className={view === "room" ? "active" : ""} onClick={() => setView("room")}>
            讨论
          </button>
          <button className={view === "scorecard" ? "active" : ""} onClick={() => setView("scorecard")}>
            记分卡
          </button>
          <button className={view === "settings" ? "active" : ""} onClick={() => setView("settings")}>
            设置
          </button>
        </nav>
        {view === "room" && (
          <>
            <div className="room-info">
              {room.roomID ? (
                <>
                  <div className="mono" title={room.roomID}>
                    {room.roomID}
                  </div>
                  <div className="hint">
                    v{room.version} · {CONNECTION_TEXT[room.connection]}
                    {room.paused ? " · 已暂停" : ""}
                  </div>
                </>
              ) : (
                <div className="hint">尚未创建房间</div>
              )}
            </div>
            <button onClick={() => void room.createRoom()}>创建房间</button>
            <div className="row">
              <button className="ghost" disabled={!room.roomID} onClick={() => void room.pause()}>
                暂停
              </button>
              <button className="ghost" disabled={!room.roomID} onClick={() => void room.resume()}>
                恢复
              </button>
            </div>
            {room.error && <div className="error">{room.error}</div>}
            <DevPanel roomID={room.roomID} />
          </>
        )}
      </aside>
      <main className="main">
        {view === "settings" ? (
          <SettingsView roomID={room.roomID} />
        ) : view === "scorecard" ? (
          <ScorecardView roomID={room.roomID} />
        ) : (
          <>
            <TypingStatus roundOpen={room.roundOpen} typing={room.typing} />
            <Timeline entries={room.entries} onAddress={setAddressTarget} />
            <Composer
              disabled={!room.roomID}
              addressTarget={addressTarget}
              onClearAddress={() => setAddressTarget(null)}
              onSend={(body, addressedTo) => void room.send(body, addressedTo)}
            />
          </>
        )}
      </main>
    </div>
  );
}
