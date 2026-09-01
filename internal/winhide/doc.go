// Package winhide：Windows 宿主侧子进程控制台窗口抑制。
//
// GUI 子系统进程（桌面壳 mosaic.exe）里 exec 控制台程序——harness 探测、
// wsl.exe、codex/kimi CLI——每个子进程都会自建控制台窗口，启动时表现为
// 连续闪黑框。CREATE_NO_WINDOW 让子进程不分配控制台（stdout/stderr 管道
// 照常工作）；控制台宿主（mosaic-server）下子进程本就继承其控制台，该标志
// 同样无害。非 Windows 平台为 no-op。
package winhide
