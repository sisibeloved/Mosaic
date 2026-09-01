// App 壳（参考 ChatGPT App）：左侧边栏（Logo / + 新房间 / 房间列表 / 底部设置与主题），
// 主区为当前房间或设置页（<Outlet/>），无多余 chrome。
import { useEffect } from "react";
import { NavLink, Outlet, useNavigate } from "react-router-dom";
import { relativeTime } from "../lib/ui";
import { refreshRooms, useRooms } from "../state/rooms";
import { toggleTheme, useTheme } from "../state/theme";

export function AppShell() {
  const { rooms, error } = useRooms();
  const navigate = useNavigate();
  const [theme, setTheme] = useTheme();

  useEffect(() => {
    void refreshRooms();
  }, []);

  return (
    <div className="flex h-full">
      <aside className="flex w-64 shrink-0 flex-col border-r border-border bg-surface">
        <div className="flex items-center gap-2 px-4 pb-2 pt-4">
          <span className="inline-block h-5 w-5 rounded-md bg-accent" aria-hidden />
          <span className="text-base font-semibold tracking-wide">Mosaic</span>
        </div>
        <div className="px-3 pb-2">
          <button
            type="button"
            onClick={() => navigate("/new")}
            className="flex w-full items-center justify-center gap-1.5 rounded-lg bg-accent px-3 py-2 text-sm font-medium text-accent-contrast transition-opacity hover:opacity-90"
          >
            <IconPlus />
            新房间
          </button>
        </div>
        <nav className="flex-1 overflow-y-auto px-2 py-1" aria-label="房间列表">
          {rooms === null ? (
            <p className="px-2 py-3 text-xs text-faint">加载中…</p>
          ) : rooms.length === 0 ? (
            <p className="px-2 py-3 text-xs text-faint">还没有房间——点上方按钮开始第一场讨论。</p>
          ) : (
            rooms.map((r) => (
              <NavLink
                key={r.room_id}
                to={`/rooms/${r.room_id}`}
                className={({ isActive }) =>
                  `group mb-0.5 block rounded-lg px-3 py-2 transition-colors ${
                    isActive ? "bg-surface-3" : "hover:bg-surface-2"
                  }`
                }
              >
                <span className="block truncate text-sm text-text">{r.display_name}</span>
                <span className="block text-xs text-faint">
                  {r.paused && <span className="mr-1 text-warn">已暂停 ·</span>}
                  {relativeTime(r.last_event_at)} · {r.message_count} 条
                </span>
              </NavLink>
            ))
          )}
          {error && <p className="px-2 py-2 text-xs text-danger">{error}</p>}
        </nav>
        <div className="flex items-center justify-between border-t border-border px-3 py-2">
          <NavLink
            to="/settings"
            className={({ isActive }) =>
              `flex items-center gap-1.5 rounded-lg px-2 py-1.5 text-sm transition-colors ${
                isActive ? "bg-surface-3 text-text" : "text-dim hover:bg-surface-2 hover:text-text"
              }`
            }
          >
            <IconGear />
            设置
          </NavLink>
          <button
            type="button"
            onClick={() => setTheme(toggleTheme(theme))}
            title={theme === "dark" ? "切换亮色" : "切换暗色"}
            aria-label="切换主题"
            className="rounded-lg p-2 text-dim transition-colors hover:bg-surface-2 hover:text-text"
          >
            {theme === "dark" ? <IconSun /> : <IconMoon />}
          </button>
        </div>
      </aside>
      <main className="min-w-0 flex-1">
        <Outlet />
      </main>
    </div>
  );
}

function IconPlus() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round">
      <path d="M12 5v14M5 12h14" />
    </svg>
  );
}

function IconGear() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
    </svg>
  );
}

function IconSun() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41" />
    </svg>
  );
}

function IconMoon() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
      <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z" />
    </svg>
  );
}
