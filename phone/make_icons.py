#!/usr/bin/env python3
"""Иконки и заставки приложения «Контакт» — из той же пиксельной трубки 40×88, что рисует страница
(drawHandset в internal/webapp/static/index.html). Перерисовывает PNG на месте, размеры — те, что уже
лежат в android/ и ios/ (их когда-то задал capacitor-assets). Запуск: python3 make_icons.py (нужен Pillow).

Трубка наклонена, как в первом «дзынь» входящего звонка: так иконка — «звонит», а не «лежит».
"""
import math
import pathlib

from PIL import Image, ImageDraw

WALL, WALL_DARK = (0xD5, 0xDB, 0xD6), (0x1D, 0x23, 0x21)
C = dict(out=(0x2A, 0x08, 0x06), base=(0xA3, 0x20, 0x1C), hi=(0xCF, 0x3A, 0x2E), hi2=(0xE2, 0x61, 0x4F),
         lo=(0x7E, 0x15, 0x12), lo2=(0x5C, 0x0D, 0x0A), hole=(0x2A, 0x08, 0x06), holeHi=(0x4A, 0x0F, 0x0B))
W, H = 40, 88


def span(y):
    def cup(y0, y1, x0, x1, r_top, r_bot):
        top = y - y0 < r_top
        r = r_top if top else r_bot
        dy = y - y0 if top else y1 - y
        cut = round(r - math.sqrt(r * r - (r - dy) * (r - dy))) if dy < r else 0
        return x0 + cut, x1 - cut
    if y <= 21:
        return cup(0, 21, 0, 39, 7, 2)
    if y <= 25:
        k = (y - 21) / 4
        return round(k * 5), 39 - round(k * 5)
    if y <= 62:
        return 5, 34
    if y <= 66:
        k = (66 - y) / 4
        return round(k * 5), 39 - round(k * 5)
    return cup(66, 87, 0, 39, 2, 7)


def handset():
    im = Image.new("RGBA", (W, H), (0, 0, 0, 0))
    px = im.load()
    for y in range(H):
        l, r = span(y)
        pl, pr = span(y - 1) if y > 0 else (99, -1)
        nl, nr = span(y + 1) if y < H - 1 else (99, -1)
        for x in range(l, r + 1):
            edge = x in (l, r) or x < pl or x > pr or x < nl or x > nr
            t = (x - l) / max(1, r - l)
            c = C["base"]
            if edge:
                c = C["out"]
            elif t < .1:
                c = C["hi2"]
            elif t < .22:
                c = C["hi"]
            elif t > .9:
                c = C["lo2"]
            elif t > .78:
                c = C["lo"]
            px[x, y] = c + (255,)

    def rect(x, y, w, h, c):
        for yy in range(y, y + h):
            for xx in range(x, x + w):
                px[xx, yy] = c + (255,)
    for y in range(6, 16, 3):  # решётка динамика
        for x in range(11, 29, 3):
            dx, dy = x - 19.5, (y - 10.5) * 1.3
            if dx * dx + dy * dy > 85:
                continue
            rect(x, y, 2, 2, C["hole"])
            rect(x + 1, y + 1, 1, 1, C["holeHi"])
    cx, cy = 19.5, 77  # микрофон
    for a in range(0, 360, 4):
        rect(round(cx + 9.5 * math.cos(math.radians(a))), round(cy + 7.5 * math.sin(math.radians(a))), 1, 1, C["lo"])
    rect(19, 76, 2, 2, C["hole"])
    for i in range(6):
        a = i * math.pi / 3
        rect(round(cx - 1 + 3.6 * math.cos(a)), round(cy - 1 + 3 * math.sin(a)), 2, 2, C["hole"])
    for i in range(10):
        a = i * math.pi / 5 + .3
        rect(round(cx - 1 + 6.6 * math.cos(a)), round(cy - 1 + 5.2 * math.sin(a)), 2, 2, C["hole"])
    return im


ART = handset()


def art(height):
    """Трубка высотой height, пиксели целые (кратный масштаб), наклон −14° — «дзынь»."""
    k = max(1, round(height / H))
    big = ART.resize((W * k, H * k), Image.NEAREST)
    return big.rotate(14, resample=Image.NEAREST, expand=True)


def compose(size, bg, frac, shape=None):
    w, h = size
    im = Image.new("RGBA", size, bg + (255,) if bg else (0, 0, 0, 0))
    a = art(min(w, h) * frac)
    if a.height > h * .92 or a.width > w * .92:  # кратный масштаб перерос — шагом меньше
        k = max(1, round(min(w, h) * frac / H) - 1)
        a = ART.resize((W * k, H * k), Image.NEAREST).rotate(14, resample=Image.NEAREST, expand=True)
    im.alpha_composite(a, ((w - a.width) // 2, (h - a.height) // 2))
    if shape == "round":
        mask = Image.new("L", size, 0)
        ImageDraw.Draw(mask).ellipse((0, 0, w - 1, h - 1), fill=255)
        im.putalpha(mask)
    return im


def main():
    root = pathlib.Path(__file__).parent
    n = 0
    for p in sorted(root.glob("android/app/src/main/res/**/*.png")) + sorted(root.glob("ios/App/App/Assets.xcassets/**/*.png")):
        size, name = Image.open(p).size, p.name
        dark = "night" in str(p) or "dark" in name
        bg = WALL_DARK if dark else WALL
        if name == "ic_launcher_background.png":
            im = Image.new("RGBA", size, WALL + (255,))
        elif name == "ic_launcher_foreground.png":
            im = compose(size, None, .62)
        elif name == "ic_launcher_round.png":
            im = compose(size, WALL, .74, "round")
        elif name.startswith("ic_launcher") or name.startswith("AppIcon"):
            im = compose(size, WALL, .8)
        else:  # заставки
            im = compose(size, bg, .34)
        im.convert("RGB" if name != "ic_launcher_foreground.png" and name != "ic_launcher_round.png" else "RGBA").save(p, optimize=True)
        n += 1
    print(f"перерисовано {n} картинок")


if __name__ == "__main__":
    main()
