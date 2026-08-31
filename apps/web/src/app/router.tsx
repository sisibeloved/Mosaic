// 路由（SPA 回退由服务端 handleUI 保证，BrowserRouter 直接可用）：
//   /                → 最近房间（localStorage）→ 列表首间 → /new
//   /new             → 空态引导页
//   /rooms/:roomId   → 房间聊天页
//   /settings        → 设置页
import { useEffect } from "react";
import { createBrowserRouter, Navigate, useNavigate } from "react-router-dom";
import { AppShell } from "./AppShell";
import { NewRoomPage } from "../pages/NewRoomPage";
import { RoomPage } from "../pages/RoomPage";
import { SettingsPage } from "../pages/SettingsPage";
import { getLastRoomId, refreshRooms, useRooms } from "../state/rooms";

function HomeRedirect() {
  const { rooms } = useRooms();
  const navigate = useNavigate();

  useEffect(() => {
    void refreshRooms();
  }, []);

  useEffect(() => {
    if (rooms === null) return;
    const last = getLastRoomId();
    const target =
      (last && rooms.some((r) => r.room_id === last) ? last : null) ?? rooms[0]?.room_id ?? null;
    navigate(target ? `/rooms/${target}` : "/new", { replace: true });
  }, [rooms, navigate]);

  return (
    <div className="flex h-full items-center justify-center text-sm text-dim">加载房间列表…</div>
  );
}

export const router = createBrowserRouter([
  {
    element: <AppShell />,
    children: [
      { path: "/", element: <HomeRedirect /> },
      { path: "/new", element: <NewRoomPage /> },
      { path: "/rooms/:roomId", element: <RoomPage /> },
      { path: "/settings", element: <SettingsPage /> },
      { path: "*", element: <Navigate to="/" replace /> },
    ],
  },
]);
