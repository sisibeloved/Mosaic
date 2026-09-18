// 消息正文 Markdown 渲染（M3-7 / v1.44 dogfood 登记：agent 消息普遍含
// **粗体**/列表/表格标记，纯文本展示满屏原始标记）。
//
// 安全管线（登记前提：XSS 白名单过滤、不引入原始 HTML 直通）——
// react-markdown 组件式渲染，三道白名单由构造保证：
//  1. 无 HTML 直通：MD 文本里的 <script>/<iframe> 等按字面转义显示（默认行为，
//     不启用 rehype-raw）；
//  2. URL scheme 白名单：defaultUrlTransform 只放行 http/https/mailto 与相对
//     路径（javascript: 等被剔除）；
//  3. 元素白名单：下方 components 覆盖决定渲染面——img 不渲染（外链请求与
//     布局破坏面直接关闭），链接强制 rel/target。
//
// remark-gfm：表格/删除线/任务列表（agent 输出的高频形态）；remark-breaks：
// 聊天单换行即断行（标准 MD 会折叠成空格——与聊天语义相悖）。
import { memo } from "react";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkBreaks from "remark-breaks";
import remarkGfm from "remark-gfm";

// mention 锚前缀（渲染层内部协议：@ 解析产物以 #m:<pid> 链接形态过 markdown，
// a 组件分支渲染为不可导航的高亮 chip——不进浏览器 hash、不开新页签）。
const MENTION_HREF = "#m:";

const components: Components = {
  // 链接：新开 + 不带referrer（WebView 内外链行为归壳层，渲染面只保证不泄漏referrer）
  a: ({ children, href }) => {
    if (typeof href === "string" && href.startsWith(MENTION_HREF)) {
      return <span className="font-medium text-accent">{children}</span>;
    }
    return (
      <a href={href} target="_blank" rel="noreferrer noopener" className="text-accent underline break-all">
        {children}
      </a>
    );
  },
  // 图片不渲染（防外链请求/追踪/布局破坏）——以链接形式保留线索
  img: ({ alt }) =>
    alt ? <span className="text-dim">[图片：{alt}]</span> : <span className="text-dim">[图片]</span>,
  // 行内代码
  code: ({ className, children }) => (
    <code className={`rounded bg-surface-3 px-1 py-px font-mono text-[0.92em] ${className ?? ""}`}>
      {children}
    </code>
  ),
  // 围栏代码块：横向滚动（长行不撑爆气泡）
  pre: ({ children }) => (
    <pre className="overflow-x-auto rounded-lg bg-surface-3 p-2.5 text-xs leading-relaxed">{children}</pre>
  ),
  // 表格：横向滚动容器（宽表可读）
  table: ({ children }) => (
    <div className="my-1 max-w-full overflow-x-auto">
      <table className="border-collapse text-xs">{children}</table>
    </div>
  ),
  th: ({ children }) => (
    <th className="border border-border bg-surface-3 px-2 py-1 text-left font-medium">{children}</th>
  ),
  td: ({ children }) => <td className="border border-border px-2 py-1 align-top">{children}</td>,
  // 消息内标题不撑层级——收敛为加粗段落级（h1~h4 统一）
  h1: ({ children }) => <p className="my-1 font-semibold text-text">{children}</p>,
  h2: ({ children }) => <p className="my-1 font-semibold text-text">{children}</p>,
  h3: ({ children }) => <p className="my-1 font-medium text-text">{children}</p>,
  h4: ({ children }) => <p className="my-1 font-medium text-text">{children}</p>,
  // 列表排版（气泡内紧凑）
  ul: ({ children }) => <ul className="my-1 list-disc pl-5">{children}</ul>,
  ol: ({ children }) => <ol className="my-1 list-decimal pl-5">{children}</ol>,
  li: ({ children }) => <li className="my-0.5 leading-relaxed">{children}</li>,
  blockquote: ({ children }) => (
    <blockquote className="my-1 border-l-2 border-border pl-2.5 text-dim">{children}</blockquote>
  ),
  hr: () => <hr className="my-2 border-border" />,
  p: ({ children }) => <p className="leading-relaxed">{children}</p>,
};

/** 正文内 @ 提及解析（v1.77 狗粮：@par_* 原样显示未解析）：
 *  - @participant_id（pid 精确匹配）→ [@显示名](#m:pid) 高亮；
 *  - @显示名（在席参与者名，两侧无字母数字下划线时整词匹配）→ 同上。
 *  替换在 markdown 解析前进行——@ 名内的 markdown 元字符由显示名转义兜住。 */
function resolveMentions(text: string, names: Map<string, string>): string {
  if (names.size === 0 || !text.includes("@")) return text;
  // pid 形态字符集封闭（par_ 前缀 + [A-Za-z0-9_-]），全局精确替换。
  text = text.replace(/@(par_[A-Za-z0-9_-]+)/g, (whole, pid: string) => {
    const name = names.get(pid);
    return name ? mentionLink(name, pid) : whole;
  });
  // 显示名形态：长名优先（防 "Kim" 抢先于 "Kimi"）；转义元字符；
  // 两侧边界为非 [\w]（中文显示名天然成立，英文名防部分词误命中）。
  const displayNames = [...new Set([...names.values()])]
    .filter((n) => n.trim().length > 0)
    .sort((a, b) => b.length - a.length);
  for (const name of displayNames) {
    const pid = pidOfName(names, name);
    if (!pid) continue;
    const esc = name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    text = text.replace(new RegExp(`(^|[^\\w])@(${esc})(?=$|[^\\w])`, "g"), (_w, pre: string, hit: string) =>
      `${pre}${mentionLink(hit, pid)}`,
    );
  }
  return text;
}

function mentionLink(name: string, pid: string): string {
  return `[@${name.replace(/([[\]])/g, "\\$1")}](${MENTION_HREF}${pid})`;
}

function pidOfName(names: Map<string, string>, name: string): string | null {
  for (const [pid, n] of names) {
    if (n === name) return pid;
  }
  return null;
}

/** 消息正文渲染（memo：SSE 高频刷新下避免同文本重复解析）。mentionNames：
 * pid → 显示名（正文 @ 提及解析；缺省不解析）。 */
export const MarkdownBody = memo(function MarkdownBody({
  text,
  mentionNames,
}: {
  text: string;
  mentionNames?: Map<string, string>;
}) {
  const src = mentionNames ? resolveMentions(text, mentionNames) : text;
  return (
    <div className="min-w-0 break-words text-sm">
      <ReactMarkdown remarkPlugins={[remarkGfm, remarkBreaks]} components={components}>
        {src}
      </ReactMarkdown>
    </div>
  );
});
