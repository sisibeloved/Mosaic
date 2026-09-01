#!/usr/bin/env python3
"""Mosaic 应用图标候选:4 个概念各出一张 512px PNG,外加一张对比总览。

用法: docs/design/icon/.venv/bin/python docs/design/icon/gen_icons.py
输出: docs/design/icon/icon-{a,b,c,d}-*.png + sheet.png
"""
import math
import os

from PIL import Image, ImageDraw, ImageFont

S = 4               # 超采样倍率,抗锯齿
SZ = 512 * S        # 绘制画布
OUT = os.path.dirname(os.path.abspath(__file__))

BG = (15, 18, 24, 255)       # 图标底 #0f1218,贴合 web 端深色基调
TILE_DARK = (35, 41, 54, 255)
INK = (232, 236, 244, 255)
RING = (57, 67, 90, 255)
SHEET_BG = (24, 28, 38, 255)

# 调色板与 web 端渠道配色一致: codex=蓝 / kimi=绿 / echo=琥珀,扩展席位玫瑰/紫/青
BLUE = (122, 162, 255, 255)
GREEN = (74, 222, 128, 255)
AMBER = (251, 191, 36, 255)
ROSE = (251, 113, 133, 255)
VIOLET = (167, 139, 250, 255)
TEAL = (45, 212, 191, 255)


def canvas():
    img = Image.new("RGBA", (SZ, SZ), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    d.rounded_rectangle([0, 0, SZ - 1, SZ - 1], radius=112 * S, fill=BG)
    return img, d


def ellipsis(d, cx, cy, dot, span, color):
    for i in (-1, 0, 1):
        x = cx + i * span
        d.ellipse([x - dot, cy - dot, x + dot, cy + dot], fill=color)


def save(img, name):
    img = img.resize((512, 512), Image.LANCZOS)
    img.save(os.path.join(OUT, name))
    return img


# --- A 拼块: 2x2 马赛克,三块彩色 = 各家 Agent,暗块省略号 = 讨论进行中 ---
def cand_a():
    img, d = canvas()
    m, gap = 96 * S, 28 * S
    t = (SZ - 2 * m - gap) // 2
    r = 34 * S
    cells = [(m, m, BLUE), (m + t + gap, m, GREEN), (m, m + t + gap, AMBER)]
    for x, y, c in cells:
        d.rounded_rectangle([x, y, x + t, y + t], radius=r, fill=c)
    x = y = m + t + gap
    d.rounded_rectangle([x, y, x + t, y + t], radius=r, fill=TILE_DARK)
    ellipsis(d, x + t // 2, y + t // 2, 13 * S, 36 * S, INK)
    return save(img, "icon-a-tiles.png")


# --- B 圆桌: 俯视圆桌,六席环绕,中央省略号 = 正在进行的多方讨论 ---
def cand_b():
    img, d = canvas()
    cx = cy = SZ // 2
    rr = 150 * S
    d.ellipse([cx - rr, cy - rr, cx + rr, cy + rr], outline=RING, width=10 * S)
    dot = 36 * S
    for i, c in enumerate([BLUE, GREEN, AMBER, ROSE, VIOLET, TEAL]):
        a = -math.pi / 2 + i * math.pi / 3
        x, y = cx + rr * math.cos(a), cy + rr * math.sin(a)
        d.ellipse([x - dot - 7 * S, y - dot - 7 * S,
                   x + dot + 7 * S, y + dot + 7 * S], fill=BG)
        d.ellipse([x - dot, y - dot, x + dot, y + dot], fill=c)
    ellipsis(d, cx, cy, 12 * S, 32 * S, INK)
    return save(img, "icon-b-roundtable.png")


# --- C 群聊: 三只对话气泡叠成拼块,底部气泡省略号点题 ---
def cand_c():
    img, d = canvas()
    ol = 10 * S  # 交叠处的底色描边,形成拼块缝隙感

    def bubble(x0, y0, w, h, r, tail, color):
        d.rounded_rectangle([x0, y0, x0 + w, y0 + h], radius=r,
                            fill=color, outline=BG, width=ol)
        d.polygon(tail, fill=color)

    bubble(75 * S, 105 * S, 160 * S, 125 * S, 38 * S,
           [(105 * S, 225 * S), (175 * S, 225 * S), (90 * S, 275 * S)], BLUE)
    bubble(220 * S, 125 * S, 160 * S, 125 * S, 38 * S,
           [(300 * S, 240 * S), (355 * S, 240 * S), (380 * S, 300 * S)], GREEN)
    bubble(155 * S, 270 * S, 200 * S, 125 * S, 40 * S,
           [(195 * S, 390 * S), (265 * S, 390 * S), (175 * S, 445 * S)], AMBER)
    ellipsis(d, 255 * S, 332 * S, 11 * S, 30 * S, BG)
    return save(img, "icon-c-bubbles.png")


# --- D 拼字 M: 5x5 马赛克拼出字母 M,按列取渠道色 ---
def cand_d():
    img, d = canvas()
    m, gap = 120 * S, 12 * S
    cell = (SZ - 2 * m - 4 * gap) // 5
    r = int(cell * 0.28)
    on = [(i, 0) for i in range(5)] + [(i, 4) for i in range(5)]
    on += [(1, 1), (2, 2), (1, 3)]
    col_color = {0: BLUE, 1: GREEN, 2: AMBER, 3: ROSE, 4: VIOLET}
    for row, col in on:
        x = m + col * (cell + gap)
        y = m + row * (cell + gap)
        d.rounded_rectangle([x, y, x + cell, y + cell],
                            radius=r, fill=col_color[col])
    return save(img, "icon-d-m.png")


def sheet(images):
    pad, size, label_h = 60, 320, 56
    w = pad * 5 + size * 4
    h = pad * 2 + size + label_h
    img = Image.new("RGBA", (w, h), SHEET_BG)
    d = ImageDraw.Draw(img)
    try:
        font = ImageFont.truetype(
            "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", 28)
    except OSError:
        font = ImageFont.load_default()
    labels = ["A  tiles", "B  roundtable", "C  bubbles", "D  M-mosaic"]
    for i, (im, label) in enumerate(zip(images, labels)):
        x = pad + i * (size + pad)
        img.paste(im.resize((size, size), Image.LANCZOS), (x, pad),
                  im.resize((size, size), Image.LANCZOS))
        d.text((x, pad + size + 14), label, font=font, fill=INK)
    img.save(os.path.join(OUT, "sheet.png"))


if __name__ == "__main__":
    sheet([cand_a(), cand_b(), cand_c(), cand_d()])
    print("ok ->", OUT)
