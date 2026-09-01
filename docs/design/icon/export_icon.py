#!/usr/bin/env python3
"""正式图标位图导出(M-mosaic 方案)。

矢量母版是 docs/design/icon/mosaic.svg;本脚本与其共用同一组几何常量
(512 视口: margin 120 / pitch 56.8 / tile 44.8 / rx 12.5 / 底圆角 112),
仅用于批量渲染位图。改动设计时先改 SVG,再同步这里的常量。

用法: docs/design/icon/.venv/bin/python docs/design/icon/export_icon.py
输出:
  docs/design/icon/mosaic-512.png        预览母版
  apps/web/public/favicon-32x32.png      SVG favicon 的位图回退
  apps/web/public/apple-touch-icon.png   180px
  apps/desktop/build/appicon.png         Wails 约定 1024px(wails build 打包用)
  apps/desktop/build/windows/icon.ico    16..256 多尺寸(Windows 打包用)
"""
import os

from PIL import Image, ImageDraw

ROOT = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(ROOT, "..", "..", ".."))

BG = "#0f1218"
COLS = {0: "#7aa2ff", 1: "#4ade80", 2: "#fbbf24", 3: "#fb7185", 4: "#a78bfa"}
CELLS = ([(r, 0) for r in range(5)] + [(r, 4) for r in range(5)]
         + [(1, 1), (2, 2), (1, 3)])

VIEW = 512.0
MARGIN, PITCH, TILE, RX, BG_RX = 120.0, 56.8, 44.8, 12.5, 112.0


def render(size):
    """4 倍超采样绘制后 LANCZOS 降采样,保证任意目标尺寸的边缘质量。"""
    k = size * 4 / VIEW
    side = int(size * 4)
    img = Image.new("RGBA", (side, side), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    d.rounded_rectangle([0, 0, side - 1, side - 1], radius=BG_RX * k, fill=BG)
    for r, c in CELLS:
        x = (MARGIN + c * PITCH) * k
        y = (MARGIN + r * PITCH) * k
        t = TILE * k
        d.rounded_rectangle([x, y, x + t, y + t], radius=RX * k, fill=COLS[c])
    return img.resize((size, size), Image.LANCZOS)


def main():
    for d in ("apps/desktop/build/windows", "apps/web/public"):
        os.makedirs(os.path.join(REPO, d), exist_ok=True)
    master = render(1024)
    master.save(os.path.join(REPO, "apps/desktop/build/appicon.png"))
    master.save(
        os.path.join(REPO, "apps/desktop/build/windows/icon.ico"),
        sizes=[(16, 16), (24, 24), (32, 32), (48, 48), (64, 64),
               (128, 128), (256, 256)],
    )
    render(512).save(os.path.join(ROOT, "mosaic-512.png"))
    render(180).save(os.path.join(REPO, "apps/web/public/apple-touch-icon.png"))
    render(32).save(os.path.join(REPO, "apps/web/public/favicon-32x32.png"))
    print("exported -> apps/desktop/build/, apps/web/public/, docs/design/icon/")


if __name__ == "__main__":
    main()
