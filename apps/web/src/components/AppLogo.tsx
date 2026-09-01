// 应用内品牌标识：复用 public/favicon.svg（M-mosaic 母版，自带深色圆角底，
// 双主题下均为自包含图标块）。单一资产——改母版经 export_icon.py 重导即全局生效。
export function AppLogo({
  size = 20,
  className,
}: {
  size?: number;
  className?: string;
}) {
  return (
    <img
      src="/favicon.svg"
      width={size}
      height={size}
      alt="Mosaic"
      className={className}
      draggable={false}
    />
  );
}
