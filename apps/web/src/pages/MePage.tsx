// 个人中心（全局层）：我的资料卡 / 外观主题 / 关于。
// 纯本地内容——后端无个人资料端点；显示名由服务端分配，自定义昵称随后续版本。
import { AppLogo } from "../components/AppLogo";
import { Avatar } from "../components/chat/Avatar";
import { useTheme } from "../state/theme";

export function MePage() {
  const [theme, setTheme] = useTheme();

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto flex max-w-2xl flex-col gap-8 px-6 py-8">
        <h1 className="text-lg font-semibold tracking-tight">个人中心</h1>

        <section>
          <h2 className="mb-3 text-sm font-medium">我的资料</h2>
          <div className="flex items-center gap-4 rounded-xl border border-border px-4 py-4">
            <Avatar participantID="human" displayName="我" size={48} color="var(--accent)" />
            <div className="min-w-0 flex-1">
              <p className="text-base font-medium">我</p>
              <p className="text-xs text-faint">房主 · 人类席位</p>
            </div>
          </div>
          <p className="mt-2 text-[11px] text-faint">
            显示名当前由服务端分配；自定义昵称与头像随后续版本开放。
          </p>
        </section>

        <section>
          <h2 className="mb-1 text-sm font-medium">外观</h2>
          <p className="mb-3 text-xs text-faint">主题即时生效，本地持久化。</p>
          <div className="flex gap-2">
            {(
              [
                { id: "dark", label: "暗色（默认）" },
                { id: "light", label: "亮色" },
              ] as const
            ).map((t) => (
              <button
                key={t.id}
                type="button"
                onClick={() => setTheme(t.id)}
                className={`rounded-xl border px-3.5 py-2 text-sm transition-colors ${
                  theme === t.id
                    ? "border-accent bg-accent-soft text-accent"
                    : "border-border bg-surface-2 text-text hover:border-faint"
                }`}
              >
                {t.label}
              </button>
            ))}
          </div>
        </section>

        <section>
          <h2 className="mb-3 text-sm font-medium">关于</h2>
          <div className="flex items-center gap-4 rounded-xl border border-border px-4 py-4">
            <AppLogo size={40} />
            <div className="min-w-0 flex-1">
              <p className="text-sm font-medium">Mosaic v0.1.0</p>
              <p className="text-xs text-faint">多智能体讨论室 · M2 开发线</p>
            </div>
            <a
              href="https://github.com/sisibeloved/Mosaic"
              target="_blank"
              rel="noreferrer"
              className="shrink-0 text-xs text-accent hover:underline"
            >
              GitHub 仓库
            </a>
          </div>
        </section>
      </div>
    </div>
  );
}
