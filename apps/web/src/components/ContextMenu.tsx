// 右键上下文菜单（v1.73）：按对象分型的菜单项由调用方组装，本组件只负责
// 弹出/定位/关闭。设计约束：
//   - 只在注册了 onContextMenu 的目标上劫持——输入框/正文选区等场景保留浏览器
//     原生菜单（复制/粘贴可达性不受损）；
//   - 边界翻转（右/下溢出时收进视口）；ESC / 点击外部 / 滚动 / 失焦关闭；
//   - 指针操作专属（右键无键盘等价物）：键盘用户的等价动作仍走常驻按钮
//     （气泡 hover 动作条、房间页 ⋯ 菜单），本组件不承担键盘导航。
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

export interface ContextMenuItem {
  kind?: "item" | "separator";
  label: string;
  /** 右侧辅助文字（如快捷提示）。 */
  hint?: string;
  danger?: boolean;
  disabled?: boolean;
  onSelect?: () => void;
}

interface MenuState {
  x: number;
  y: number;
  items: ContextMenuItem[];
}

/** 用法：const menu = useContextMenu(); 目标 onContextMenu={(e) => menu.open(e, [...])}；渲染 {menu.element}。 */
export function useContextMenu() {
  const [state, setState] = useState<MenuState | null>(null);
  const ref = useRef<HTMLDivElement | null>(null);

  const open = useCallback((e: React.MouseEvent, items: ContextMenuItem[]) => {
    e.preventDefault();
    e.stopPropagation();
    setState({ x: e.clientX, y: e.clientY, items });
  }, []);

  const close = useCallback(() => setState(null), []);

  // 边界翻转：菜单渲染后按实测尺寸收进视口（右/下溢出改从左/上起）。
  useLayoutEffect(() => {
    if (!state || !ref.current) return;
    const el = ref.current;
    const rect = el.getBoundingClientRect();
    let x = state.x;
    let y = state.y;
    if (x + rect.width > window.innerWidth - 8) x = Math.max(8, window.innerWidth - rect.width - 8);
    if (y + rect.height > window.innerHeight - 8) y = Math.max(8, window.innerHeight - rect.height - 8);
    if (x !== state.x || y !== state.y) setState((s) => (s ? { ...s, x, y } : s));
  }, [state]);

  useEffect(() => {
    if (!state) return;
    const onDown = (ev: MouseEvent) => {
      if (ref.current && !ref.current.contains(ev.target as Node)) close();
    };
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key === "Escape") close();
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    window.addEventListener("resize", close);
    window.addEventListener("blur", close);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
      window.removeEventListener("resize", close);
      window.removeEventListener("blur", close);
    };
  }, [state, close]);

  const element = state ? (
    <div
      ref={ref}
      role="menu"
      onClick={(e) => e.stopPropagation()}
      className="animate-fade-in fixed z-50 min-w-44 overflow-hidden rounded-xl border border-border bg-surface py-1 shadow-2xl"
      style={{ left: state.x, top: state.y }}
    >
      {state.items.map((it, i) =>
        it.kind === "separator" ? (
          <div key={i} role="separator" className="my-1 h-px bg-border" />
        ) : (
          <button
            key={i}
            type="button"
            role="menuitem"
            disabled={it.disabled}
            onClick={() => {
              close();
              it.onSelect?.();
            }}
            className={`flex w-full items-center gap-3 px-3 py-1.5 text-left text-xs transition-colors disabled:opacity-40 ${
              it.danger
                ? "text-danger hover:bg-[color-mix(in_srgb,var(--danger)_12%,transparent)]"
                : "text-dim hover:bg-surface-2 hover:text-text"
            }`}
          >
            <span className="min-w-0 flex-1 truncate">{it.label}</span>
            {it.hint && <span className="shrink-0 text-[10px] text-faint">{it.hint}</span>}
          </button>
        ),
      )}
    </div>
  ) : null;

  return { open, close, element };
}
