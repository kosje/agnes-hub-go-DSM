#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""构建 agnes-hub-go 的群晖 DSM SPK 安装包。

四个必须守住的群晖约束：

  1. 一个 SPK 只能装一种架构的二进制。群晖官方 INFO 文档对 arch 字段的原话是
     "Please not pack all binary files with different platforms to one package spk file."
     所以这里按架构产出独立的 SPK（默认只出 x86_64）。
  2. INFO 的每个值都必须带双引号（package="x" 而不是 package=x），
     且 version 必须是「功能号-构建号」（1.0.2-0001），构建号每次发布要递增，
     否则套件中心认为版本没变、不提示升级。
  3. thirdparty="yes" 不能漏。DSM 靠它判定这是第三方套件、走「信任层级」那套流程；
     缺了它 DSM 会把包当成群晖官方包、要求有效的官方签名，安装直接被拒。
  4. 生命周期脚本是固定文件名的 scripts/ 目录，其中 start-stop-status 加
     preinst/postinst/preuninst/postuninst/preupgrade/postupgrade 七个文件
     必须全部存在，缺一个会被判定为「套件损坏」。数据目录用 SYNOPKG_PKGVAR。

产出结构（对齐群晖官方 Package Developer Guide）：
    agnes-hub-<arch>-<version>.spk   (外层必须是未压缩 tar)
      ├── INFO                         key="value" 行文本，含 package.tgz 的 checksum
      ├── package.tgz                  内层 tar.gz，只放单架构二进制
      │   └── agnes-hub-go
      ├── scripts/                     start-stop-status + 六个生命周期钩子
      ├── conf/                        privilege（run-as: package）+ resource
      ├── PACKAGE_ICON.PNG             64×64（DSM 7 要求）
      └── PACKAGE_ICON_256.PNG         256×256

用法：
    python tools/build_spk.py                      # 默认只出 x86_64，输出到 dist/
    SPK_BUILD=7 python tools/build_spk.py          # 指定构建号 -> 1.0.2-0007
    python tools/build_spk.py --arch armv8         # 只出 armv8
    python tools/build_spk.py --arch all           # 两个架构都出
    SPK_OUT_DIR=D:/somewhere python tools/build_spk.py
    python tools/build_spk.py --emit-web-logo      # 只重生成 Web UI 顶栏 logo

默认只出 x86_64：本项目实际只分发群晖 x86_64 机型（DS918+ 等 apollolake 平台）。
armv8 的目标保留在 ARCH_TARGETS 里，需要时用 --arch 打开。

构建前需要先交叉编译出对应的 Linux 二进制（源码零改动，Go 标准库自带交叉编译）：
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o agnes-hub-go-linux-amd64 .
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o agnes-hub-go-linux-arm64 .

注意 go build 会把 internal/web/static/ 下的前端资源 embed 进二进制，所以改了
console.html / chat.html / logo.png 之后必须重新 go build，再打包 SPK。
"""
import hashlib
import argparse
import contextlib
import gzip
import io
import json
import math
import os
import re
import shutil
import struct
import sys
import tarfile
import zlib

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT_DIR = os.environ.get("SPK_OUT_DIR") or os.path.join(ROOT, "dist")
BUNDLE_DIR = os.path.join(ROOT, "spk-bundle")

APP_ID = "agnes-hub"
SERVICE_PORT = 4142
# DSM 桌面应用的 ID，同时用于 INFO 的 dsmappname 与 ui/config 的键（两处必须一致）。
# 不带宽字符：参考的真实第三方套件（homebridge / PeerBanHelper / TorrServer /
# 百度网盘）用的都是 SYNO.SDS.Xxx 或 vendor.app 形式，没有一家用连字符。
DSM_APP_NAME = "SYNO.SDS.agneshub.Application"
# 桌面图标点开后的落地路径（管理控制台）
DSM_APP_URL = "/console"
# 桌面图标要成套给：DSM 会在桌面、开始菜单、任务栏等不同位置按尺寸取用。
# 参考的真实套件（synoedit / wol-spk）都提供这一组。
DSM_ICON_SIZES = (16, 24, 32, 48, 64, 72, 128, 256)
OS_MIN_VER = "7.0-40000"
MAINTAINER = "kosje"
MAINTAINER_URL = "https://github.com/kosje/agnes-hub-go-DSM"

# 分发架构标签 -> 交叉编译产物文件名。
#
# 注意：这里的标签用于命令行和产物文件名，不能直接写进 INFO 的 arch。DSM
# 用具体平台代号（例如 DS918+ 是 apollolake）匹配套件；只写 x86_64 会导致
# 绝大多数 Intel/AMD NAS 在安装阶段报“不支持此平台”。同一份 amd64 二进制
# 可以安全用于下面所有 x86_64 平台，所以 INFO 要列出完整的平台集合。
# 32 位的 armv7（alpine/alpine4k）与 armada370 等老机型不在支持范围内：
# 需要 GOARCH=arm GOARM=7，且这类机器基本已停产。
ARCH_TARGETS = [
    ("x86_64", "agnes-hub-go-linux-amd64"),
    ("armv8", "agnes-hub-go-linux-arm64"),
]

PACKAGE_ARCHES = {
    "x86_64": (
        "apollolake avoton braswell broadwell broadwellnk broadwellnkv2 "
        "broadwellntbap bromolow cedarview coffeelake denverton epyc7002 "
        "epyc7003 epyc7003ntb geminilake geminilakenk grantley icelaked "
        "kvmx64 purley r1000 r1000nk skylaked v1000 v1000nk x64 x86_64"
    ),
    "armv8": "armada37xx rtd1296 rtd1619 rtd1619b aarch64 armv8",
}

# 每次发布 SPK 都必须递增。仍可用 SPK_BUILD 临时覆盖。
DEFAULT_SPK_BUILD = "3"

DESCRIPTION = (
    "Agnes AI 多账号聚合中转 + RPM 限流排队网关。统一模型 agnes-auto 自动判定"
    "文生 / 生图 / 生视频；FIFO 严格节拍限流、软粘性溢出、二维自适应校准、"
    "熔断自动复活。数据保存在套件目录内，全 JSON 文件，免 SSH 即可备份迁移。"
)

# ---------------------------------------------------------------------------
# 生命周期脚本
# ---------------------------------------------------------------------------

# DSM 调用约定：start / stop / status / prestart / prestop。
# status 退出码规范：0=运行中 1=进程已死但 pid 文件仍在 2=lock 文件存在
#                   3=未运行 4=状态未知 150=套件损坏需重装
START_STOP_STATUS = '''#!/bin/sh
# agnes-hub 群晖套件生命周期脚本。
PKG_NAME="%APP_ID%"
PKG_DIR="${SYNOPKG_PKGDEST:-/var/packages/$PKG_NAME/target}"
VAR_DIR="${SYNOPKG_PKGVAR:-/var/packages/$PKG_NAME/var}"
BIN="$PKG_DIR/agnes-hub-go"
DATA_DIR="$VAR_DIR/data"
PIDFILE="$VAR_DIR/agnes-hub.pid"
LOGFILE="$VAR_DIR/agnes-hub.log"
PORT="%PORT%"

# 往套件日志回传错误。SYNOPKG_TEMP_LOGFILE 在部分调用场景下未定义，
# 这里显式兜底，避免把错误信息写到空路径。
msg() {
  if [ -n "${SYNOPKG_TEMP_LOGFILE:-}" ]; then
    echo "$1" > "$SYNOPKG_TEMP_LOGFILE"
  fi
  return 0
}

# 判定是否在运行。除了 pid 存活，还要确认这个 pid 确实是我们的二进制 ——
# 只看 kill -0 会在 PID 被系统复用后误判成「正在运行」，导致套件显示已启动
# 但端口其实没在监听。
is_running() {
  [ -f "$PIDFILE" ] || return 1
  pid="$(cat "$PIDFILE" 2>/dev/null)"
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  exe="$(readlink "/proc/$pid/exe" 2>/dev/null)" || return 1
  case "$exe" in
    */agnes-hub-go) return 0 ;;
  esac
  return 1
}

start_service() {
  if is_running; then
    return 0
  fi
  mkdir -p "$DATA_DIR" || return 1
  cd "$PKG_DIR" || return 1

  # -no-selfupdate：套件由套件中心负责升级。若允许进程内替换二进制，
  # 套件的实际内容就与 INFO 里登记的 checksum 对不上，下次升级必然冲突。
  # -host 0.0.0.0 由 main.go 展开为 IPv4(0.0.0.0)+IPv6(::) 双栈监听。
  nohup "$BIN" -host 0.0.0.0 -port "$PORT" -data "$DATA_DIR" -no-selfupdate \\
    >> "$LOGFILE" 2>&1 &
  echo $! > "$PIDFILE"

  # 就绪探测：有 curl 就打 /healthz，没有就退化为「进程存活」。
  i=0
  while [ "$i" -lt 30 ]; do
    if ! is_running; then
      msg "agnes-hub 进程启动后立即退出，请查看 $LOGFILE"
      rm -f "$PIDFILE"
      return 1
    fi
    if [ -x /usr/bin/curl ]; then
      if /usr/bin/curl -fsS -m 2 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
        return 0
      fi
    else
      sleep 2
      if is_running; then
        return 0
      fi
    fi
    i=$((i + 1))
    sleep 1
  done
  msg "agnes-hub 启动超时（端口 $PORT 未就绪），请查看 $LOGFILE"
  return 1
}

stop_service() {
  if ! is_running; then
    rm -f "$PIDFILE"
    return 0
  fi
  pid="$(cat "$PIDFILE")"
  # 先 SIGTERM：main.go 收到后走优雅关闭，8 秒内处理完在途请求。
  # 直接 SIGKILL 会打断 SSE 流和 JSON 的原子写。
  kill -TERM "$pid" 2>/dev/null
  i=0
  while [ "$i" -lt 15 ]; do
    kill -0 "$pid" 2>/dev/null || break
    i=$((i + 1))
    sleep 1
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -KILL "$pid" 2>/dev/null
    sleep 1
  fi
  rm -f "$PIDFILE"
  return 0
}

case "$1" in
  start)
    start_service
    exit $?
    ;;
  stop)
    stop_service
    exit $?
    ;;
  status)
    if is_running; then
      exit 0
    fi
    if [ -f "$PIDFILE" ]; then
      exit 1
    fi
    exit 3
    ;;
  prestart|prestop)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
'''

# 安装后建数据目录。群晖在升级时保留 var/，只有卸载才删除整个套件目录。
POSTINST = '''#!/bin/sh
PKG_NAME="%APP_ID%"
PKG_DIR="${SYNOPKG_PKGDEST:-/var/packages/$PKG_NAME/target}"
VAR_DIR="${SYNOPKG_PKGVAR:-/var/packages/$PKG_NAME/var}"
mkdir -p "$VAR_DIR/data" 2>/dev/null || true

# 确保 target/ui 与系统 3rdparty 快捷方式软链接建立，保障 DSM 主菜单图标即时出现
if [ -d "$PKG_DIR/ui" ] && [ -d /usr/syno/synoman/webman/3rdparty ]; then
  ln -sfn "$PKG_DIR/ui" "/usr/syno/synoman/webman/3rdparty/$PKG_NAME" 2>/dev/null || true
fi
exit 0
'''

# 升级后同样确保目录在（升级流程会重建 target/，var/ 本身不动）。
POSTUPGRADE = POSTINST

TRIVIAL = "#!/bin/sh\nexit 0\n"


def read_version():
    """从 main.go 读取 var version，避免与打包脚本两处手改不同步。"""
    path = os.path.join(ROOT, "main.go")
    try:
        src = open(path, encoding="utf-8").read()
    except OSError as e:
        sys.exit("[ERROR] 无法读取 %s：%s" % (path, e))
    m = re.search(r'^var version\s*=\s*"([^"]+)"', src, re.M)
    if not m:
        sys.exit("[ERROR] 在 main.go 中找不到 `var version = \"x.y.z\"`，无法确定版本号")
    return m.group(1)


def spk_version(go_version):
    """把 1.0.2 变成群晖要求的 1.0.2-0001。

    群晖 version 必须是「功能号-构建号」；构建号要在每次发布时递增，
    否则套件中心认为版本没变、不会提示升级。用 SPK_BUILD 环境变量指定。
    """
    build = os.environ.get("SPK_BUILD", DEFAULT_SPK_BUILD)
    if not build.isdigit():
        sys.exit("[ERROR] SPK_BUILD 必须是纯数字，当前为 %r" % build)
    return "%s-%04d" % (go_version, int(build))


# ---------------------------------------------------------------------------
# 图标：纯标准库 PNG 编解码 + 矢量精度重绘
# ---------------------------------------------------------------------------

def _png_decode_rgba(path):
    """解码 8 位 RGBA、非隔行的 PNG。返回 (w, h, bytearray)。"""
    d = open(path, "rb").read()
    if d[:8] != b"\x89PNG\r\n\x1a\n":
        sys.exit("[ERROR] %s 不是 PNG 文件" % path)
    off = 8
    idat = b""
    w = h = None
    while off < len(d):
        ln, typ = struct.unpack(">I4s", d[off:off + 8])
        body = d[off + 8:off + 8 + ln]
        if typ == b"IHDR":
            w, h, bd, ct, comp, filt, inter = struct.unpack(">IIBBBBB", body)
            # ct=6 是 RGBA，ct=2 是 RGB（无 alpha）。上游换 logo 后用的是 RGB，
            # 所以两种都收，RGB 在解完滤波后补一条不透明 alpha 通道。
            if bd != 8 or ct not in (2, 6) or inter != 0:
                sys.exit("[ERROR] %s 只支持 8 位 RGB / RGBA 非隔行 PNG"
                         "（当前 bitdepth=%d colortype=%d interlace=%d）"
                         % (path, bd, ct, inter))
        elif typ == b"IDAT":
            idat += body
        elif typ == b"IEND":
            break
        off += 12 + ln

    raw = zlib.decompress(idat)
    bpp = 4 if ct == 6 else 3          # 每像素字节数：RGBA=4，RGB=3
    stride = w * bpp
    px = bytearray(w * h * 4)
    prev = bytearray(stride)
    pos = 0
    for y in range(h):
        ft = raw[pos]
        pos += 1
        line = bytearray(raw[pos:pos + stride])
        pos += stride
        if ft == 1:
            for i in range(bpp, stride):
                line[i] = (line[i] + line[i - bpp]) & 0xFF
        elif ft == 2:
            for i in range(stride):
                line[i] = (line[i] + prev[i]) & 0xFF
        elif ft == 3:
            for i in range(stride):
                a = line[i - bpp] if i >= bpp else 0
                line[i] = (line[i] + ((a + prev[i]) >> 1)) & 0xFF
        elif ft == 4:
            for i in range(stride):
                a = line[i - bpp] if i >= bpp else 0
                b = prev[i]
                c = prev[i - bpp] if i >= bpp else 0
                p = a + b - c
                pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                pr = a if (pa <= pb and pa <= pc) else (b if pb <= pc else c)
                line[i] = (line[i] + pr) & 0xFF
        if bpp == 4:
            px[y * w * 4:(y + 1) * w * 4] = line
        else:
            # RGB -> RGBA，alpha 补不透明
            row = bytearray(w * 4)
            for x in range(w):
                row[x * 4:x * 4 + 3] = line[x * 3:x * 3 + 3]
                row[x * 4 + 3] = 0xFF
            px[y * w * 4:(y + 1) * w * 4] = row
        prev = line
    return w, h, px


def _png_encode_rgba(path, w, h, px):
    sig = b"\x89PNG\r\n\x1a\n"

    def chunk(tag, data):
        body = tag + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF)

    stride = w * 4
    raw = b"".join(b"\x00" + bytes(px[y * stride:(y + 1) * stride]) for y in range(h))
    data = (sig + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 6, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b""))
    with open(path, "wb") as f:
        f.write(data)


# ---------------------------------------------------------------------------
# App 图标：整幅插画裁切 + 线性光缩放 + DSM 圆角遮罩
#
# 图标源是 assets/ICON.PNG —— 一幅「铬质 A 字 + 上方圆点 + 星空光环」的插画
# （413×384，RGB 无 alpha）。它不是扁平 logo，所以没法靠阈值把字形抠出来重绘：
# 背景里有明亮的光柱、光环和山体，实测都会跟字形连成一片，连通域分析拿不到干净的
# 字形包围盒。所以这里改成整幅裁切，思路是：
#   · 按实测的视觉重心裁一个正方形，把「A + 圆点 + 光环」框进来；
#   · 缩放在线性光空间做面积平均 —— 直接在 sRGB 上平均会把暗部抬亮、亮部压暗，
#     缩小后就是一张发灰的糊图（这是照片类图标最容易翻车的地方）；
#   · 轻微提对比、提饱和、加一圈暗角，让图标读起来是一块完整的「瓦片」；
#   · 套 DSM 的圆角方形遮罩（0.22），小尺寸再往里收一点，保证「A」还认得出来。
# ---------------------------------------------------------------------------

# 裁切窗口，用相对源图的比例而不是像素 —— 换同构图的高分辨率源图不用改代码。
# 实测自 413×384 的源图：约等于从 (50, 25) 起裁 310×310。
ICON_CROP_LEFT = 0.1211
ICON_CROP_TOP = 0.0651
ICON_CROP_SIDE = 0.7506

# DSM 图标是圆角方形，0.22 是群晖自家套件的常见圆角比例。
ICON_CORNER_RADIUS = 0.22

# 小尺寸下额外往里收的倍率（按图标边长）。16/24px 里整幅插画会糊成一团，
# 收一点让「A」占到更多像素，至少还认得出是个字母。
ICON_SMALL_ZOOM = ((32, 1.14), (48, 1.07))

# 收尾调色。都是轻手：目的是让暗紫背景更有层次、铬质字形更立体，
# 而不是把原图改头换面。
ICON_CONTRAST = 1.06
ICON_SATURATION = 1.12
ICON_VIGNETTE = 0.30


def _build_srgb_luts():
    """建 sRGB <-> 线性光的查表。

    用 LUT 是为了别在纯 Python 里对每个像素调 pow()：一次 256 级的正向表
    加一张 4096 级的反向表，缩放时只做乘加和一次查表。
    """
    to_lin = [0.0] * 256
    for i in range(256):
        c = i / 255.0
        to_lin[i] = c / 12.92 if c <= 0.04045 else ((c + 0.055) / 1.055) ** 2.4
    n = 4096
    to_srgb = []
    for i in range(n + 1):
        v = i / n
        s = 12.92 * v if v <= 0.0031308 else 1.055 * (v ** (1.0 / 2.4)) - 0.055
        to_srgb.append(0 if s < 0 else (255 if s > 1 else int(round(s * 255))))
    return to_lin, to_srgb


_SRGB_TO_LINEAR, _LINEAR_TO_SRGB = _build_srgb_luts()
_LINEAR_LUT_MAX = len(_LINEAR_TO_SRGB) - 1


def _linear_to_srgb(v):
    """线性光值（0..1）查表换回 sRGB 0..255。"""
    i = int(v * _LINEAR_LUT_MAX + 0.5)
    if i < 0:
        i = 0
    elif i > _LINEAR_LUT_MAX:
        i = _LINEAR_LUT_MAX
    return _LINEAR_TO_SRGB[i]


def _resample_crop(src_w, src_h, px, left, top, side, size):
    """把源图上 (left, top, side×side) 的正方形区域面积平均成 size×size。

    覆盖率按源像素与目标格的交集算，所以非整数倍缩小时不会丢像素、也不会出现
    摩尔纹；累加在线性光空间做，换回 sRGB 时暗部不会发灰。
    """
    lin = _SRGB_TO_LINEAR
    out = bytearray(size * size * 4)
    step = side / size
    for ty in range(size):
        y0 = top + ty * step
        y1 = y0 + step
        iy0 = max(0, int(math.floor(y0)))
        iy1 = min(src_h, int(math.ceil(y1)))
        for tx in range(size):
            x0 = left + tx * step
            x1 = x0 + step
            ix0 = max(0, int(math.floor(x0)))
            ix1 = min(src_w, int(math.ceil(x1)))
            ar = ag = ab = 0.0
            aw = 0.0
            for sy in range(iy0, iy1):
                wy = min(y1, sy + 1.0) - max(y0, float(sy))
                if wy <= 0.0:
                    continue
                row = sy * src_w * 4
                for sxi in range(ix0, ix1):
                    wx = min(x1, sxi + 1.0) - max(x0, float(sxi))
                    if wx <= 0.0:
                        continue
                    wgt = wx * wy
                    i = row + sxi * 4
                    ar += lin[px[i]] * wgt
                    ag += lin[px[i + 1]] * wgt
                    ab += lin[px[i + 2]] * wgt
                    aw += wgt
            o = (ty * size + tx) * 4
            if aw > 0.0:
                out[o] = _linear_to_srgb(ar / aw)
                out[o + 1] = _linear_to_srgb(ag / aw)
                out[o + 2] = _linear_to_srgb(ab / aw)
            out[o + 3] = 255
    return out


def _rrect_span(yc, x0, y0, x1, y1, radius):
    """圆角矩形在 y=yc 这一行的水平跨度 [left, right]；不在矩形内返回 None。"""
    if yc < y0 or yc > y1:
        return None
    if radius <= 0:
        return x0, x1
    if yc < y0 + radius:
        d = (y0 + radius) - yc
    elif yc > y1 - radius:
        d = yc - (y1 - radius)
    else:
        return x0, x1
    if d > radius:
        return None
    dx = math.sqrt(max(0.0, radius * radius - d * d))
    return x0 + radius - dx, x1 - radius + dx


def _raster_shapes(size, shapes):
    """把若干形状的并集栅格化成抗锯齿灰度掩码（bytearray，0..255）。

    逐行求出每个形状在该行的水平跨度，再按像素算跨度覆盖度累加 —— 比超采样快，
    边缘是精确抗锯齿的。sign<0 表示从并集里挖掉。
    """
    mask = bytearray(size * size)
    for y in range(size):
        yc = y + 0.5
        cov = [0.0] * size
        for x0, y0, x1, y1, radius, sign in shapes:
            span = _rrect_span(yc, x0, y0, x1, y1, radius)
            if span is None:
                continue
            left, right = span
            if right <= 0 or left >= size:
                continue
            for x in range(max(0, int(math.floor(left))),
                           min(size - 1, int(math.ceil(right)) - 1) + 1):
                ov = min(right, x + 1.0) - max(left, float(x))
                if ov > 0:
                    cov[x] += sign * ov
        base = y * size
        for x in range(size):
            v = cov[x]
            if v <= 0.0:
                continue
            mask[base + x] = 255 if v >= 1.0 else int(round(v * 255.0))
    return mask


def _grade(size, px):
    """对比 / 饱和 / 暗角收尾，就地改 px。"""
    sat = ICON_SATURATION
    con = ICON_CONTRAST
    vig = ICON_VIGNETTE
    half = size / 2.0
    for y in range(size):
        dy = (y + 0.5 - half) / half
        row = y * size
        for x in range(size):
            i = (row + x) * 4
            r, g, b = px[i], px[i + 1], px[i + 2]
            # 饱和度：把像素往它的灰度值推或拉
            gray = (r * 299 + g * 587 + b * 114) // 1000
            r = gray + (r - gray) * sat
            g = gray + (g - gray) * sat
            b = gray + (b - gray) * sat
            # 对比度：绕中灰缩放
            r = (r - 128.0) * con + 128.0
            g = (g - 128.0) * con + 128.0
            b = (b - 128.0) * con + 128.0
            if vig > 0.0:
                dx = (x + 0.5 - half) / half
                d = math.sqrt(dx * dx + dy * dy) * 0.7071068
                f = 1.0 - vig * d * d
                r *= f
                g *= f
                b *= f
            px[i] = 0 if r < 0.0 else (255 if r > 255.0 else int(r + 0.5))
            px[i + 1] = 0 if g < 0.0 else (255 if g > 255.0 else int(g + 0.5))
            px[i + 2] = 0 if b < 0.0 else (255 if b > 255.0 else int(b + 0.5))
    return px


def _render_app_icon(src_w, src_h, src_px, size):
    """裁切 + 缩放 + 调色 + 圆角遮罩，产出一张 size×size 的 RGBA 图标。"""
    zoom = 1.0
    for limit, z in ICON_SMALL_ZOOM:
        if size <= limit:
            zoom = z
            break

    base_side = ICON_CROP_SIDE * src_w
    cx = ICON_CROP_LEFT * src_w + base_side / 2.0
    cy = ICON_CROP_TOP * src_h + base_side / 2.0
    # 往里收的时候绕同一个中心缩，避免裁切窗口跑偏
    side = base_side / zoom
    left = max(0.0, min(src_w - side, cx - side / 2.0))
    top = max(0.0, min(src_h - side, cy - side / 2.0))

    px = _resample_crop(src_w, src_h, src_px, left, top, side, size)
    _grade(size, px)

    mask = _raster_shapes(size, [(0.0, 0.0, float(size), float(size),
                                  size * ICON_CORNER_RADIUS, 1.0)])
    for i in range(0, size * size * 4, 4):
        px[i + 3] = mask[i // 4]
    return px


def make_icon_set(src_png, targets):
    """从图标源图生成任意尺寸的方形 App 图标。

    targets 是 [(边长, 输出路径), ...]。群晖 SPK 要 64/256，套件中心缩略图要 72/256，
    桌面图标成套要 16/24/32/48/64/72/128/256，所以尺寸做成参数而不是写死。
    """
    w, h, px = _png_decode_rgba(src_png)
    if w < 64 or h < 64:
        sys.exit("[ERROR] %s 尺寸过小（%dx%d），无法生成图标" % (src_png, w, h))
    for size, dst in targets:
        _png_encode_rgba(dst, size, size, _render_app_icon(w, h, px, size))
    return w, h


def make_icons(src_png, out_64, out_256):
    """群晖 SPK 要求的两个方形图标（64×64 与 256×256）。"""
    return make_icon_set(src_png, [(64, out_64), (256, out_256)])


def make_web_logo(src_png, dst, size=64):
    """重新生成 Web UI 顶栏用的 logo（internal/web/static/logo.png）。

    这个文件被 //go:embed 进二进制，所以必须在 go build 之前生成好 —— 不能在
    SPK 打包时注入。改了图标算法或换了源图之后要单独跑一次：
        python tools/build_spk.py --emit-web-logo
    生成的图跟套件图标同一套渲染，避免网页上还挂着老 logo。
    """
    make_icon_set(src_png, [(size, dst)])
    return dst


# ---------------------------------------------------------------------------
# SPK 组装
# ---------------------------------------------------------------------------

@contextlib.contextmanager
def _tar_gz_writer(fileobj):
    """产出 tar.gz，外层 gzip 头不带时间戳，保证构建可复现。

    tarfile 的 "w:gz" 模式会把当前时间写进 gzip 头，导致同一份内容每次构建
    产出不同字节 —— 大小一样但 md5 变化，catalog 里声明的 md5 就失效了。
    这里手工套一层 GzipFile 并把 mtime 归零。tar 条目自身的 mtime 已在
    _add_file 里设为 0。

    gzip 必须显式关闭：tarfile 对「外部传入的 fileobj」不会代为关闭，
    不关就写不出 gzip 尾部的 CRC 与长度，产出的文件是坏的。
    """
    gz = gzip.GzipFile(fileobj=fileobj, mode="wb", compresslevel=9, mtime=0)
    try:
        with tarfile.open(fileobj=gz, mode="w", format=tarfile.GNU_FORMAT) as tar:
            yield tar
    finally:
        gz.close()


def _write(path, content, mode=0o755):
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write(content)
    os.chmod(path, mode)


def _add_file(tar, full, arc, mode=0o644):
    ti = tar.gettarinfo(full, arcname=arc)
    ti.uid = ti.gid = 0
    ti.uname = ti.gname = "root"
    ti.mtime = 0
    ti.mode = mode
    with open(full, "rb") as fh:
        tar.addfile(ti, fh)


def _add_dir_recursive(tar, full, arc, exec_prefixes=("scripts/",)):
    ti = tarfile.TarInfo(name=arc + "/")
    ti.type = tarfile.DIRTYPE
    ti.mode = 0o755
    ti.uid = ti.gid = 0
    ti.uname = ti.gname = "root"
    ti.mtime = 0
    tar.addfile(ti)
    for child in sorted(os.listdir(full)):
        cfull = os.path.join(full, child)
        carc = arc + "/" + child
        if os.path.isdir(cfull):
            _add_dir_recursive(tar, cfull, carc, exec_prefixes)
        else:
            mode = 0o755 if carc.startswith(exec_prefixes) else 0o644
            _add_file(tar, cfull, carc, mode)


def build_package_tgz(binary_path, ui_dir, out_path):
    """内层 package.tgz：放本架构的二进制及 DSM UI 目录，解压后位于 SYNOPKG_PKGDEST 根下。

    群晖官方规范要求：dsmuidir="ui" 指向 package.tgz 解压到 target 后的相对目录，
    DSM 安装服务据此将 target/[dsmuidir] 链接到 /usr/syno/synoman/webman/3rdparty/ 下以显示主菜单图标。
    """
    buf = io.BytesIO()
    with _tar_gz_writer(buf) as tar:
        _add_file(tar, binary_path, "agnes-hub-go", mode=0o755)
        if os.path.isdir(ui_dir):
            _add_dir_recursive(tar, ui_dir, "ui")
    data = buf.getvalue()
    with open(out_path, "wb") as f:
        f.write(data)
    return data


def build_info(arch, spk_ver, checksum, extractsize_kb):
    """INFO 是 key="value" 行文本，每个值都必须带双引号。

    dname / displayname 与 desc / description 各写两遍：后者是群晖官方文档里的
    字段名，前者是大量实际 SPK 在用的字段名，两个都写可以保证套件中心在任何
    DSM 版本上都显示正确的名称与描述。
    """
    lines = [
        'package="%s"' % APP_ID,
        'version="%s"' % spk_ver,
        'dname="Agnes Hub"',
        'displayname="Agnes Hub"',
        'desc="%s"' % DESCRIPTION,
        'description="%s"' % DESCRIPTION,
        'arch="%s"' % PACKAGE_ARCHES[arch],
        'os_min_ver="%s"' % OS_MIN_VER,
        # thirdparty 必须声明。DSM 靠它判定这是第三方套件，从而走「信任层级」
        # 那套流程；缺了它 DSM 会把包当成群晖官方包，要求有效的官方签名，
        # 安装直接被拒。对照真实在用的 DSM 7 第三方 SPK（homebridge 4.1.2、
        # Duplicati、r8152）确认过，它们的 INFO 里都有这一行。
        'thirdparty="yes"',
        'maintainer="%s"' % MAINTAINER,
        'maintainer_url="%s"' % MAINTAINER_URL,
        'distributor="%s"' % MAINTAINER,
        'distributor_url="%s"' % MAINTAINER_URL,
        'helpurl="%s#readme"' % MAINTAINER_URL,
        # 套件启动后，套件中心里的「打开」按钮直接进控制台。
        'adminprotocol="http"',
        'adminport="%d"' % SERVICE_PORT,
        'adminurl="console"',
        # DSM 桌面图标：dsmuidir 指向包内的 ui/ 目录，dsmappname 是应用 ID
        # （必须与 ui/config 里的键一致）。reloadui 让安装后桌面自动刷新，
        # 不用手动注销重登就能看到图标。
        # dsmapppage / dsmapplaunchname 是 DSM 7 新增字段（spksrc 里以
        # version_ge 7.0 为条件输出），漏掉会导致桌面图标注册不上。
        # 取值对齐真实可用的 synoedit：三个字段都用同一个应用 ID。
        'dsmuidir="ui"',
        'dsmappname="%s"' % DSM_APP_NAME,
        'dsmapppage="%s"' % DSM_APP_NAME,
        'dsmapplaunchname="%s"' % DSM_APP_NAME,
        'reloadui="yes"',
        # 4142 是固定端口（客户端配置依赖它），端口冲突时让服务自己启动失败
        # 并写日志，好过安装阶段直接被拒。
        'checkport="no"',
        'ctl_stop="yes"',
        'startable="yes"',
        'silent_install="yes"',
        'silent_upgrade="yes"',
        'silent_uninstall="yes"',
        'support_move="yes"',
        'extractsize="%d"' % extractsize_kb,
        'checksum="%s"' % checksum,
    ]
    return "\n".join(lines) + "\n"


def prepare_arch(arch, binary_src, spk_ver, icon_src):
    """为单个架构准备 SPK 内容目录，返回 (目录, package.tgz 的 md5)。"""
    d = os.path.join(BUNDLE_DIR, arch)
    if os.path.isdir(d):
        shutil.rmtree(d)
    scripts_dir = os.path.join(d, "scripts")
    conf_dir = os.path.join(d, "conf")
    for sub in (scripts_dir, conf_dir):
        os.makedirs(sub, exist_ok=True)

    # 1. DSM 桌面图标（ui/）
    #    只有 INFO 里的 dsmuidir/dsmappname 还不够，必须有 ui/config 把这个
    #    「应用」定义出来 —— 且该目录必须打进 package.tgz（由 dsmuidir="ui" 指定并由 DSM 软链接），
    #    这样安装后 DSM 桌面和主菜单上才能正常显示图标和快捷方式。
    #    图标要成套给：参考的真实套件（synoedit / wol-spk）都提供 16/24/32/48/
    #    64/72/128/256 多种尺寸，config 里用 images/icon_{0}.png 让 DSM 按需取。
    ui_dir = os.path.join(d, "ui")
    ui_images = os.path.join(ui_dir, "images")
    os.makedirs(ui_images, exist_ok=True)
    make_icon_set(icon_src, [(n, os.path.join(ui_images, "icon_%d.png" % n))
                             for n in DSM_ICON_SIZES])
    with open(os.path.join(ui_dir, "config"), "w", encoding="utf-8", newline="\n") as f:
        json.dump({".url": {DSM_APP_NAME: {
            "title": "Agnes Hub",
            "desc": "Agnes AI 多账号聚合中转 + RPM 限流排队网关",
            "icon": "images/icon_{0}.png",
            "type": "url",
            "protocol": "http",
            "port": str(SERVICE_PORT),
            "url": DSM_APP_URL,
            "allUsers": True,
        }}}, f, ensure_ascii=False, indent=4)
        f.write("\n")

    # 2. 内层包：包含二进制以及 ui/ 目录
    pkg_tgz = os.path.join(d, "package.tgz")
    pkg_data = build_package_tgz(binary_src, ui_dir, pkg_tgz)
    md5 = hashlib.md5(pkg_data).hexdigest()

    # 3. 生命周期脚本：六个钩子必须全部存在，缺一个会被判「套件损坏」
    _write(os.path.join(scripts_dir, "start-stop-status"),
           START_STOP_STATUS.replace("%APP_ID%", APP_ID).replace("%PORT%", str(SERVICE_PORT)))
    _write(os.path.join(scripts_dir, "postinst"), POSTINST.replace("%APP_ID%", APP_ID))
    _write(os.path.join(scripts_dir, "postupgrade"), POSTUPGRADE.replace("%APP_ID%", APP_ID))
    for name in ("preinst", "preuninst", "postuninst", "preupgrade"):
        _write(os.path.join(scripts_dir, name), TRIVIAL)

    # 4. conf：privilege 决定运行身份，缺失会被拒绝安装。
    #    run-as: package = 以套件专用账户运行（非 root）。本服务监听 4142（>1024）、
    #    不需要 chown 系统文件，因此完全不需要 root，符合最小权限。
    with open(os.path.join(conf_dir, "privilege"), "w", encoding="utf-8", newline="\n") as f:
        json.dump({"defaults": {"run-as": "package"}}, f, ensure_ascii=False, indent=2)
        f.write("\n")
    #    resource 留空对象：声明端口资源需要 port-config 的配套文件格式，
    #    不声明不影响功能（防火墙规则由用户在 DSM 里自行放行）。
    with open(os.path.join(conf_dir, "resource"), "w", encoding="utf-8", newline="\n") as f:
        json.dump({}, f, ensure_ascii=False, indent=2)
        f.write("\n")

    # 5. 套件图标（外层 PACKAGE_ICON*.PNG）
    make_icons(icon_src, os.path.join(d, "PACKAGE_ICON.PNG"),
               os.path.join(d, "PACKAGE_ICON_256.PNG"))

    # 6. INFO
    extractsize_kb = (os.path.getsize(binary_src) + 1023) // 1024
    _write(os.path.join(d, "INFO"), build_info(arch, spk_ver, md5, extractsize_kb), mode=0o644)

    return d, md5


def build_spk(bundle, out_path):
    """组装 DSM SPK。

    SPK 外层必须是未压缩 tar；只有其中的 package.tgz 使用 gzip。DSM 套件中心
    不会把 gzip 压缩的外层当作有效 SPK，即使常见桌面解压工具可以打开它。
    """
    os.makedirs(OUT_DIR, exist_ok=True)
    with tarfile.open(out_path, mode="w", format=tarfile.GNU_FORMAT) as tar:
        _add_file(tar, os.path.join(bundle, "INFO"), "INFO", mode=0o644)
        _add_file(tar, os.path.join(bundle, "package.tgz"), "package.tgz", mode=0o644)
        _add_dir_recursive(tar, os.path.join(bundle, "scripts"), "scripts")
        _add_dir_recursive(tar, os.path.join(bundle, "conf"), "conf")
        _add_dir_recursive(tar, os.path.join(bundle, "ui"), "ui")
        _add_file(tar, os.path.join(bundle, "PACKAGE_ICON.PNG"), "PACKAGE_ICON.PNG", mode=0o644)
        _add_file(tar, os.path.join(bundle, "PACKAGE_ICON_256.PNG"), "PACKAGE_ICON_256.PNG", mode=0o644)
    validate_spk(out_path)
    return out_path


def validate_spk(path):
    """在发布前检查 DSM 最容易静默拒绝的 SPK 格式约束。"""
    with open(path, "rb") as fh:
        if fh.read(2) == b"\x1f\x8b":
            raise ValueError("SPK 外层被 gzip 压缩；DSM 要求外层为未压缩 tar")

    required = {
        "INFO", "package.tgz", "scripts/start-stop-status",
        "scripts/preinst", "scripts/postinst", "scripts/preuninst",
        "scripts/postuninst", "scripts/preupgrade", "scripts/postupgrade",
        "conf/privilege", "PACKAGE_ICON.PNG", "PACKAGE_ICON_256.PNG",
        "ui/config",
    }
    # DSM 桌面图标：缺了不会导致安装失败，但桌面上不会有图标。
    # 图标要成套齐全，DSM 会按场景取不同尺寸。
    required |= {"ui/images/icon_%d.png" % n for n in DSM_ICON_SIZES}
    # r: 明确只接受未压缩 tar，避免 r:* 又把错误的 gzip 外层悄悄放过。
    with tarfile.open(path, mode="r:") as outer:
        members = {m.name: m for m in outer.getmembers()}
        missing = sorted(required - set(members))
        if missing:
            raise ValueError("SPK 缺少必要条目：%s" % "、".join(missing))

        info_raw = outer.extractfile("INFO").read().decode("utf-8")
        fields = {}
        for line in info_raw.splitlines():
            if "=" in line:
                key, value = line.split("=", 1)
                fields[key] = value.strip().strip('"')
        package_data = outer.extractfile("package.tgz").read()
        if fields.get("checksum") != hashlib.md5(package_data).hexdigest():
            raise ValueError("INFO checksum 与 package.tgz 不一致")
        actual_arches = set(fields.get("arch", "").split())
        known_arch_sets = [set(value.split()) for value in PACKAGE_ARCHES.values()]
        if actual_arches not in known_arch_sets:
            raise ValueError("INFO arch 与已定义的 DSM 平台集合不一致")

        for script in (name for name in required if name.startswith("scripts/")):
            if members[script].mode & 0o111 == 0:
                raise ValueError("%s 不可执行" % script)

        with tarfile.open(fileobj=io.BytesIO(package_data), mode="r:gz") as inner:
            inner_members = {m.name for m in inner.getmembers()}
            if "agnes-hub-go" not in inner_members:
                raise ValueError("package.tgz 缺少 agnes-hub-go 二进制")
            if "ui/config" not in inner_members:
                raise ValueError("package.tgz 缺少 ui/config，DSM 将无法在主菜单显示快捷方式")
            binary = inner.getmember("agnes-hub-go")
            if binary.mode & 0o111 == 0:
                raise ValueError("package.tgz 中的 agnes-hub-go 不可执行")
            binary_file = inner.extractfile(binary)
            if binary_file.read(4) != b"\x7fELF":
                raise ValueError("package.tgz 中的 agnes-hub-go 不是 Linux ELF")


def select_targets(spec):
    """把 --arch 的取值解析成 [(arch, 二进制名), ...]。

    默认只出 x86_64：本项目实际只分发群晖 x86_64 机型（apollolake 等）。
    armv8 目标保留在 ARCH_TARGETS 里，需要时用 --arch armv8 或 --arch all。
    """
    if spec == "all":
        return list(ARCH_TARGETS)
    wanted = [a.strip() for a in spec.split(",") if a.strip()]
    known = {a for a, _ in ARCH_TARGETS}
    unknown = [a for a in wanted if a not in known]
    if unknown:
        sys.exit("[ERROR] 未知架构 %s，可选：%s 或 all"
                 % ("、".join(unknown), "、".join(sorted(known))))
    return [t for t in ARCH_TARGETS if t[0] in wanted]


def main():
    ap = argparse.ArgumentParser(description="打包群晖 DSM 套件（SPK）")
    ap.add_argument("--arch", default="x86_64",
                    help="目标架构，逗号分隔；默认 x86_64。可选 x86_64 / armv8 / all")
    ap.add_argument("--emit-web-logo", action="store_true",
                    help="只重新生成 Web UI 顶栏用的 internal/web/static/logo.png 后退出")
    args = ap.parse_args()

    icon_src = os.path.join(ROOT, "assets", "ICON.PNG")
    if not os.path.exists(icon_src):
        sys.exit("[ERROR] 缺少图标源文件 assets/ICON.PNG")

    # Web UI 的顶栏 logo 是 go:embed 进二进制的，必须在 go build 之前就生成好，
    # 所以它不能像 SPK 图标那样在打包时注入，得单独跑一次这个开关。
    # 改了图标算法 / 换了源图之后记得重跑，否则网页上的 logo 还是旧的。
    if args.emit_web_logo:
        dst = os.path.join(ROOT, "internal", "web", "static", "logo.png")
        make_web_logo(icon_src, dst)
        print("已重新生成 %s" % os.path.relpath(dst, ROOT).replace("\\", "/"))
        print("注意：该文件是 go:embed 进二进制的，需要重新 go build 才会生效。")
        return

    targets = select_targets(args.arch)

    go_version = read_version()
    spk_ver = spk_version(go_version)
    # 图标源用 assets/ICON.PNG —— 当前是一幅「铬质 A 字 + 星空光环」的插画
    # （413×384）。所有尺寸都从这一张图裁切缩放出来，所以源图分辨率越高越好；
    # 裁切窗口按比例定义（ICON_CROP_*），换同构图的高分辨率源图不用改代码。
    missing = [b for _, b in targets if not os.path.exists(os.path.join(ROOT, b))]
    if missing:
        sys.exit("[ERROR] 缺少交叉编译产物：%s\n"
                 "请先执行：\n"
                 "  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags \"-s -w\" -o agnes-hub-go-linux-amd64 .\n"
                 "  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags \"-s -w\" -o agnes-hub-go-linux-arm64 ."
                 % "、".join(missing))

    os.makedirs(BUNDLE_DIR, exist_ok=True)
    print("Go 版本号  : %s" % go_version)
    print("套件版本号 : %s" % spk_ver)
    print("图标源     : %s" % os.path.relpath(icon_src, ROOT).replace("\\", "/"))
    print()

    for arch, binary_name in targets:
        binary_src = os.path.join(ROOT, binary_name)
        bundle, md5 = prepare_arch(arch, binary_src, spk_ver, icon_src)
        out = os.path.join(OUT_DIR, "%s-%s-%s.spk" % (APP_ID, arch, spk_ver))
        build_spk(bundle, out)
        print("  [%s] arch=%s" % (arch, arch))
        print("      二进制   %s (%.1f MB)" % (binary_name, os.path.getsize(binary_src) / 1024 / 1024))
        print("      package.tgz md5 = %s" % md5)
        print("      产物     %s (%.1f MB)" % (out, os.path.getsize(out) / 1024 / 1024))
        print()

    print("安装：DSM 套件中心 → 手动安装 → 选择与 NAS 架构匹配的 SPK")
    print("提示：DSM 7 默认不信任第三方未签名套件，需先在")
    print("      套件中心 → 设置 → 常规 → 信任层级 里选「任何发行者」。")


if __name__ == "__main__":
    main()
