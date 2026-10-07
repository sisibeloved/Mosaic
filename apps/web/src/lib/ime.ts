import { useRef } from "react";

// WebKit（WKWebView / Safari）的 IME 坑：按 Enter 确认候选词时派发的 keydown
// isComposing=false（Chrome/Edge 在组合中携带 true 的路径正常）——单查事件内标志
// 会把"确认候选的 Enter"误判成真实按键（中文输入法下刚选完词就触发发送/提交）。
//
// 三重防线（真机实测迭代：同拍 setTimeout 清除假设不成立——compositionend 与
// 确认 Enter 可能跨任务派发，定时器会在两事件之间清掉标志，v1 被绕过）：
//  1. keyCode 229 —— IME 派发键的的经典标记（组合期所有 keydown 携带）；
//  2. 组合态跟踪 —— compositionstart/end 间一律视为 IME 消费（覆盖确认 Enter
//     先于 compositionend 到达的序变体）；
//  3. 刚结束时间窗 —— compositionend 后 50ms 内仍视为 IME 消费（WebKit 跨任务
//     派发的间隔在毫秒级；人对"确认候选后再发送"的双击 Enter 间隔远大于 50ms，
//     第二击照常发送）。
export function useIMEGuard() {
  const active = useRef(false);
  const endedAt = useRef(0);
  return {
    /** keydown 时该键是否仍属 IME 组合/确认（true = 放行给输入法，不处理）。 */
    consuming(e: { nativeEvent: KeyboardEvent }) {
      return (
        e.nativeEvent.keyCode === 229 ||
        e.nativeEvent.isComposing ||
        active.current ||
        performance.now() - endedAt.current < 50
      );
    },
    /** 输入元素的 composition 事件面。 */
    props: {
      onCompositionStart: () => {
        active.current = true;
      },
      onCompositionEnd: () => {
        active.current = false;
        endedAt.current = performance.now();
      },
    },
  };
}
