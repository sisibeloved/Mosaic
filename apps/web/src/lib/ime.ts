import { useRef } from "react";

// WebKit（WKWebView / Safari）的 IME 坑：按 Enter 确认候选词时，compositionend
// 先于 keydown 派发，且该 keydown 的 isComposing 已是 false——单查事件内标志会把
// "确认候选的 Enter"误判成真实按键（中文输入法下刚选完词就触发发送/提交）。
// Chrome/Edge 在组合中的 keydown 携带 isComposing=true，两种内核都要拦。
//
// 用法：keyDown 首行 if (ime(e)) return; 输入元素展开 {...useIMEGuard().props}。
// 组合刚结束的一拍以 setTimeout(0) 清除——事件派发先于定时器，同一拍内紧随
// compositionend 到达的 keydown 必被拦下，之后的真实 Enter 不受影响。
export function useIMEGuard() {
  const active = useRef(false);
  const justEnded = useRef(false);
  return {
    /** keydown 时该键是否仍属 IME 组合/确认（true = 放行给输入法，不处理）。 */
    consuming(e: { nativeEvent: { isComposing: boolean } }) {
      return e.nativeEvent.isComposing || active.current || justEnded.current;
    },
    /** 输入元素的 composition 事件面。 */
    props: {
      onCompositionStart: () => {
        active.current = true;
      },
      onCompositionEnd: () => {
        active.current = false;
        justEnded.current = true;
        setTimeout(() => {
          justEnded.current = false;
        }, 0);
      },
    },
  };
}
