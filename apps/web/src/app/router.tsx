// 路由（SPA 回退由服务端 handleUI 保证，BrowserRouter 直接可用）：
//   /                → 工作台（v1.72 裁定：房间是长期资产——最近会话 + 新建次级
//                      入口；零房间退化为建房引导）
//   /new             → 建房页（侧栏 + 与工作台"开始新讨论"的语义化目标）
//   /rooms/:roomId   → 房间聊天页（局部层）
//   /contacts        → 联系人（M4-4 固定私聊入口——全局层，按稳定身份回到长期会话）
//   /settings        → 设置页（全局层：左子导航 Agent/自动化/数据/外观/通用；
//                      v1.72 个人中心并入，不含任何房间态）
import { createBrowserRouter, Navigate } from "react-router-dom";
import { AppShell } from "./AppShell";
import { ContactsPage } from "../pages/ContactsPage";
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
      { path: "/contacts", element: <ContactsPage /> },
      { path: "/settings", element: <SettingsPage /> },
      { path: "*", element: <Navigate to="/" replace /> },
    ],
  },
]);
