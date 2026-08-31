// 主题状态：暗色默认 + 亮色备选。<html data-theme> 驱动 CSS 变量，localStorage 持久化。
// index.html 内联引导脚本负责首帧前设定，这里管交互期切换与订阅。
import { useSyncExternalStore } from "react";

export type Theme = "dark" | "light";

const STORAGE_KEY = "mosaic.theme";
const listeners = new Set<() => void>();

function current(): Theme {
  return document.documentElement.dataset.theme === "light" ? "light" : "dark";
}

export function setTheme(theme: Theme): void {
  document.documentElement.dataset.theme = theme;
  try {
    localStorage.setItem(STORAGE_KEY, theme);
  } catch {
    // 存储不可用时仅本次会话生效
  }
  listeners.forEach((l) => l());
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function useTheme(): [Theme, (t: Theme) => void] {
  return [useSyncExternalStore(subscribe, current), setTheme];
}

export function toggleTheme(t: Theme): Theme {
  return t === "dark" ? "light" : "dark";
}
