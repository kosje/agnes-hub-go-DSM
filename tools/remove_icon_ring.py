#!/usr/bin/env python3
"""抹掉 assets/ICON.PNG 里 A 字外圈的那道细白圆环。

背景
----
原始素材在铬质「A」外面套了一道细白圆环（圆心 (208, 209)，半径 192px）。
在 256px 上看是「星球光环」，但在 DSM 桌面的 64px 图标上会读成一个
「加载中」的转圈，用户明确要求去掉。

做法
----
不能靠裁切解决：A 字的外接框约 235×198，而圆环内圈的半径只有 176，
把 A 完整框进「圆环以内」需要边长 ≥ 250 的正方形 —— 但那样四个角就会
探出圆环。两者不可兼得，只能改像素。

抹除分两步，都只作用在圆环所在的环带（|r - 192| ≤ 8）内：

1. **按半径插值**：环带上每个像素，沿它自己的半径方向取环带外侧
   （r = 192 ± 10）的两个颜色做线性插值。
   采样距离必须**紧贴环带**（±(BAND+2)），不能取更远 —— 圆环是叠在一片
   较宽的径向辉光上的，采到 r=170 / r=214 会落到辉光之外，补出来的环带比
   两侧都暗，反而成了一道「黑圈」，比原图更难看。这是踩过的坑。

2. **环带内高斯模糊**（σ=2.5）：插值结果在环带扫过山体的位置会留下放射状
   拖影，模糊一下即可抹平。只糊环带，不动别处。

最终 256px 上圆环完全消失，A 字不受影响。

用法
----
    pip install numpy
    python tools/remove_icon_ring.py            # 就地改写 assets/ICON.PNG
    python tools/remove_icon_ring.py --dry-run  # 只写到 --out，不动源图

注意：本脚本依赖 numpy，**只有它是**。build_spk.py 的图标管线是纯 Python 的，
打包时不需要 numpy。源图抹一次就够了，改完把新图提交进仓库即可。
"""
import argparse
import math
import os
import sys

import numpy as np

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, os.path.join(ROOT, "tools"))

import build_spk as B  # noqa: E402

# 圆环几何 —— 由霍夫式扫描实测（对 (cx, cy, R) 暴力搜索，
# 打分 = 圆周平均亮度 − 内外各 12px 处的平均亮度，峰值 117，非常干净）。
RING_CX = 208.0
RING_CY = 209.0
RING_R = 192.0

BAND = 8.0        # 环带半宽
BLUR_SIGMA = 2.5  # 环带内模糊半径


def bilinear(a, px, py, w, h):
    """在 HxWxC 数组上按浮点坐标做双线性采样（越界钳制到边缘）。"""
    px = np.clip(px, 0, w - 1.001)
    py = np.clip(py, 0, h - 1.001)
    x0 = np.floor(px).astype(np.int32)
    y0 = np.floor(py).astype(np.int32)
    fx = (px - x0)[..., None]
    fy = (py - y0)[..., None]
    x1 = np.minimum(x0 + 1, w - 1)
    y1 = np.minimum(y0 + 1, h - 1)
    return (a[y0, x0] * (1 - fx) * (1 - fy) + a[y0, x1] * fx * (1 - fy)
            + a[y1, x0] * (1 - fx) * fy + a[y1, x1] * fx * fy)


def gauss(u, sigma):
    """可分离高斯模糊（边缘用 edge padding，不引入暗边）。"""
    rad = int(math.ceil(sigma * 3))
    k = np.exp(-0.5 * (np.arange(-rad, rad + 1) / sigma) ** 2)
    k /= k.sum()
    a = np.pad(u, ((rad, rad), (0, 0), (0, 0)), mode="edge")
    a = np.apply_along_axis(lambda m: np.convolve(m, k, mode="valid"), 0, a)
    a = np.pad(a, ((0, 0), (rad, rad), (0, 0)), mode="edge")
    a = np.apply_along_axis(lambda m: np.convolve(m, k, mode="valid"), 1, a)
    return a


def remove_ring(img, cx=RING_CX, cy=RING_CY, r=RING_R,
                band=BAND, blur_sigma=BLUR_SIGMA):
    """在 RGBA float 数组上抹掉圆环，返回新数组。"""
    h, w = img.shape[:2]
    yy, xx = np.mgrid[0:h, 0:w]
    dx = xx + 0.5 - cx
    dy = yy + 0.5 - cy
    dist = np.sqrt(dx * dx + dy * dy)
    ux = np.where(dist > 0, dx / np.maximum(dist, 1e-9), 0.0)
    uy = np.where(dist > 0, dy / np.maximum(dist, 1e-9), 0.0)

    s = band + 2.0          # 紧贴环带取样，保证与两侧连续
    inner = bilinear(img, cx + ux * (r - s), cy + uy * (r - s), w, h)
    outer = bilinear(img, cx + ux * (r + s), cy + uy * (r + s), w, h)
    wgt = np.clip((dist - (r - s)) / (2 * s), 0.0, 1.0)[..., None]
    fill = inner * (1 - wgt) + outer * wgt

    m3 = np.repeat((np.abs(dist - r) <= band)[..., None], 4, axis=2)
    out = np.where(m3, fill, img)
    if blur_sigma > 0:
        out = np.where(m3, gauss(out, blur_sigma), out)
    return np.clip(out, 0, 255)


def ring_score(img, cx=RING_CX, cy=RING_CY, r=RING_R):
    """圆环的对比度打分：圆周平均亮度 − 内外各 12px 处的平均亮度。

    原图实测 ≈ 117（圆环非常明显）；抹干净之后应降到个位数。
    本脚本**不是幂等的** —— 对已抹除的图再跑一遍，会拿环带外侧的样去插值、
    再糊一次，画面会被多抹一道。所以先用这个打分挡一下。
    """
    h, w = img.shape[:2]
    lum = (img[..., 0] * 299 + img[..., 1] * 587 + img[..., 2] * 114) / 1000.0

    def mean_on(rad):
        tot, n = 0.0, 0
        for k in range(720):
            th = 2 * math.pi * k / 720
            x = int(cx + math.cos(th) * rad + 0.5)
            y = int(cy + math.sin(th) * rad + 0.5)
            if 0 <= x < w and 0 <= y < h:
                tot += lum[y, x]
                n += 1
        return tot / n if n else 0.0

    on = mean_on(r)
    return on - max(mean_on(r - 12), mean_on(r + 12))


def main():
    ap = argparse.ArgumentParser(description="抹掉图标源图里的白色圆环")
    ap.add_argument("--src", default=os.path.join(ROOT, "assets", "ICON.PNG"))
    ap.add_argument("--out", default=None, help="默认就地改写 --src")
    ap.add_argument("--dry-run", action="store_true", help="只写 --out，不动 --src")
    ap.add_argument("--force", action="store_true", help="即便检测不到圆环也照做")
    args = ap.parse_args()

    w, h, raw = B._png_decode_rgba(args.src)
    img = np.frombuffer(bytes(raw), dtype=np.uint8).reshape(h, w, 4).astype(np.float64)

    sc = ring_score(img)
    print("圆环对比度打分：%.1f（原图约 117，抹净后应 < 15）" % sc)
    if sc < 15.0 and not args.force:
        sys.exit("看起来圆环已经抹掉了，不再重复处理（要强行跑加 --force）。")

    out = remove_ring(img)
    out[..., 3] = img[..., 3]
    data = bytearray(np.round(out).astype(np.uint8).tobytes())

    dst = args.out or (None if args.dry_run else args.src)
    if dst is None:
        dst = os.path.join(os.path.dirname(os.path.abspath(args.src)),
                           "ICON.ring-removed.PNG")
    B._png_encode_rgba(dst, w, h, data)
    print("已写出 %s（%dx%d）" % (dst, w, h))
    if dst != args.src:
        print("源图未改动。")


if __name__ == "__main__":
    main()
