// 路由（SPA 回退由服务端 handleUI 保证，BrowserRouter 直接可用）：
//   /                → 首页 = 引导建房页（ChatGPT `/` 语义；不再自动踹进最近房间——
//                      房间是二级内容，从侧栏列表点入）
//   /new             → 同上（侧栏"新房间"按钮的语义化别名）
//   /rooms/:roomId   → 房间聊天页（局部层）
//   /settings        → 设置页（全局层：实例/开发者，不含任何房间态）
//   /me              → 个人中心（全局层）
import { createBrowserRouter, Navigate } from "react-router-dom";
import { AppShell } from "./AppShell";
import { MePage } from "../pages/MePage";
import { NewRoomPage } from "../pages/NewRoomPage";
import { RoomPage } from "../pages/RoomPage";
import { SettingsPage } from "../pages/SettingsPage";

export const router = createBrowserRouter([
  {
    element: <AppShell />,
    children: [
      { path: "/", element: <NewRoomPage /> },
      { path: "/new", element: <NewRoomPage /> },
      { path: "/rooms/:roomId", element: <RoomPage /> },
      { path: "/settings", element: <SettingsPage /> },
      { path: "/me", element: <MePage /> },
      { path: "*", element: <Navigate to="/" replace /> },
    ],
  },
]);
