// 路由（SPA 回退由服务端 handleUI 保证，BrowserRouter 直接可用）：
//   /                → 工作台（v1.72 裁定：房间是长期资产——最近会话 + 新建次级
//                      入口；零房间退化为建房引导）
//   /new             → 建房页（侧栏 + 与工作台"开始新讨论"的语义化目标）
//   /rooms/:roomId   → 房间聊天页（局部层）
//   /settings        → 设置页（全局层：左子导航 Agent/自动化/数据/外观/通用；
//                      v1.72 个人中心并入，不含任何房间态）
// v1.74：/contacts 退役——联系人收敛进侧栏私聊组（在席 Agent 即私聊入口）。
import { createBrowserRouter, Navigate } from "react-router-dom";
import { AppShell } from "./AppShell";
import { HomePage } from "../pages/HomePage";
import { NewRoomPage } from "../pages/NewRoomPage";
import { RoomPage } from "../pages/RoomPage";
import { SettingsPage } from "../pages/SettingsPage";

export const router = createBrowserRouter([
  {
    element: <AppShell />,
    children: [
      { path: "/", element: <HomePage /> },
      { path: "/new", element: <NewRoomPage /> },
      { path: "/rooms/:roomId", element: <RoomPage /> },
      { path: "/settings", element: <SettingsPage /> },
      { path: "*", element: <Navigate to="/" replace /> },
    ],
  },
]);
