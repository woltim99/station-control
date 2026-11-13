# -*- coding: utf-8 -*-
"""Генерация иконки и баннера AirPlay для ТВ-лаунчера."""
import os
from PIL import Image, ImageDraw, ImageFont

root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "res", "drawable")
os.makedirs(root, exist_ok=True)

FONT_BOLD = r"C:\Windows\Fonts\segoeuib.ttf"
FONT_SEMI = r"C:\Windows\Fonts\seguisb.ttf"
FONT_REG = r"C:\Windows\Fonts\segoeui.ttf"


def dot(draw, cx, cy, r, fill):
    draw.ellipse([cx - r, cy - r, cx + r, cy + r], fill=fill)


def play_triangle(draw, cx, cy, s, fill):
    draw.polygon([(cx - s * 0.45, cy - s), (cx - s * 0.45, cy + s), (cx + s * 0.8, cy)], fill=fill)


def airplay_glyph(draw, cx, cy, s, fill):
    # экран (прямоугольник с закруглением)
    w, h = s * 2.0, s * 1.35
    draw.rounded_rectangle([cx - w / 2, cy - h / 2 - s * 0.25, cx + w / 2, cy + h / 2 - s * 0.25],
                           radius=s * 0.28, outline=fill, width=max(2, int(s * 0.16)))
    # треугольник-указатель под экраном
    draw.polygon([(cx - s * 0.62, cy + h / 2 + s * 0.42),
                  (cx + s * 0.62, cy + h / 2 + s * 0.42),
                  (cx, cy + h / 2 - s * 0.28)], fill=fill)


# ---------- иконка 192x192 ----------
N = 4
S = 192 * N
img = Image.new("RGB", (S, S), (21, 21, 23))
d = ImageDraw.Draw(img)
# тёплый отсвет
for i in range(80, 0, -1):
    t = i / 80.0
    r = int(S * 0.9 * t)
    c = int(21 + 26 * (1.0 - t) ** 2)
    d.ellipse([S * 0.70 - r, S * 0.80 - r, S * 0.70 + r, S * 0.80 + r],
              fill=(c, max(21, c - 8), max(23, c - 6)))
airplay_glyph(d, S * 0.5, S * 0.47, S * 0.17, (255, 255, 255))
# скругление углов
mask = Image.new("L", (S, S), 0)
ImageDraw.Draw(mask).rounded_rectangle([0, 0, S - 1, S - 1], radius=S * 0.22, fill=255)
out = Image.new("RGBA", (S, S), (0, 0, 0, 0))
out.paste(img, (0, 0), mask)
out.resize((192, 192), Image.LANCZOS).save(os.path.join(root, "icon.png"))

# ---------- баннер 320x180 ----------
W, H = 320, 180
img = Image.new("RGB", (W * N, H * N), (21, 21, 23))
d = ImageDraw.Draw(img)
# мягкий тёплый градиент справа
for i in range(120, 0, -1):
    a = 7
    r = int((W * N) * 0.7 * (i / 120.0))
    d.ellipse([int(W * N * 0.78) - r, int(H * N * 0.55) - r,
               int(W * N * 0.78) + r, int(H * N * 0.55) + r],
              fill=(21 + a, 21 + a // 3, 23 + a // 3))

# плашка с глифом
cx, cy = int(W * N * 0.235), int(H * N * 0.5)
box = int(86 * N)
d.rounded_rectangle([cx - box // 2, cy - box // 2, cx + box // 2, cy + box // 2],
                    radius=int(22 * N), fill=(32, 32, 36))
airplay_glyph(d, cx, cy, int(15 * N), (255, 255, 255, 255))

f1 = ImageFont.truetype(FONT_BOLD, int(34 * N))
f2 = ImageFont.truetype(FONT_REG, int(16 * N))
tx = int(W * N * 0.40)
d.text((tx, int(H * N * 0.34)), "AirPlay", font=f1, fill=(255, 255, 255), anchor="lm")
d.text((tx, int(H * N * 0.57)), "с экрана телефона", font=f2,
       fill=(255, 255, 255, 140) if False else (150, 150, 154), anchor="lm")

img.resize((W, H), Image.LANCZOS).save(os.path.join(root, "banner.png"))
print("ok", os.listdir(root))
