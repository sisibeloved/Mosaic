// 开发者模式开关（dogfood 反馈：设置内开关，而非仅服务端注入常量）。
// 服务端 -dev 仍决定调试端点是否装配（能力面）；这里管 UI 展示面（DevPanel、
// trace 等），localStorage 持久化。首启以注入值做种子，此后完全由用户掌控。
import { useSyncExternalStore } from "react";

declare const MOSAIC_DEV: boolean;

const STORAGE_KEY = "mosaic.devMode";
const listeners = new Set<() => void>();
let current: boolean | null = null;

function read(): boolean {
  if (current !== null) return current;
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    current = raw === null ? MOSAIC_DEV : raw === "1";
  } catch {
    current = MOSAIC_DEV;
  }
  return current;
}

export function setDevMode(on: boolean): void {
  current = on;
  try {
    localStorage.setItem(STORAGE_KEY, on ? "1" : "0");
  } catch {
    // 存储不可用时仅本次会话生效
  }
  listeners.forEach((l) => l());
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function useDevMode(): [boolean, (on: boolean) => void] {
  return [useSyncExternalStore(subscribe, read), setDevMode];
}
