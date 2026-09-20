#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""构建 agnes-hub-go 的群晖 DSM SPK 安装包。

与飞牛 fpk 的关键差异（决定了本脚本为什么不能复用 build_fpk.py）：

  1. 一个 SPK 只能装一种架构的二进制。群晖官方 INFO 文档对 arch 字段的原话是
     "Please not pack all binary files with different platforms to one package spk file."
     所以这里按架构循环产出多个 SPK，而不是像 fpk 那样把 amd64+arm64 塞进一个包
     再在脚本里用 uname -m 选。
  2. INFO 的每个值都必须带双引号（package="x" 而不是 package=x），
     且 version 必须是「功能号-构建号」（1.0.2-0001），构建号每次发布要递增，
     否则套件中心认为版本没变、不提示升级。
  3. 生命周期脚本是固定文件名的 scripts/ 目录，不是 cmd/main 的子命令分发；
     其中 preinst/postinst/preuninst/postuninst/preupgrade/postupgrade
     六个文件必须全部存在，缺一个会被判定为「套件损坏」。
  4. 数据目录用 SYNOPKG_PKGVAR（/var/packages/<包名>/var），不是 TRIM_PKGVAR。

产出结构（对齐群晖官方 Package Developer Guide）：
    agnes-hub-<arch>-<version>.spk   (外层 tar.gz)
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

默认只出 x86_64：本项目实际只分发群晖 x86_64 机型（DS918+ 等 apollolake 平台）。
armv8 的目标保留在 ARCH_TARGETS 里，需要时用 --arch 打开。

构建前需要先交叉编译出对应的 Linux 二进制（源码零改动，Go 标准库自带交叉编译）：
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o agnes-hub-go-linux-amd64 .
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o agnes-hub-go-linux-arm64 .
"""
import hashlib
import argparse
import contextlib
import gzip
import io
import json
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
OS_MIN_VER = "7.0-40000"
MAINTAINER = "kosje"
MAINTAINER_URL = "https://github.com/kosje/agnes-hub-go-DSM"

# 群晖套件架构值 -> 交叉编译产物文件名。
# 架构值取「家族名」而非具体平台代号：x86_64 覆盖 apollolake/avoton/braswell/
# broadwell*/bromolow/cedarview/coffeelake/denverton/geminilake/grantley/kvmx64/
# purley/skylaked/v1000 等全部 Intel/AMD 64 位机型；armv8 覆盖 rtd1296/
# rtd1619/rtd1619b/armada37xx。
# 32 位的 armv7（alpine/alpine4k）与 armada370 等老机型不在支持范围内：
# 需要 GOARCH=arm GOARM=7，且这类机器基本已停产。
ARCH_TARGETS = [
    ("x86_64", "agnes-hub-go-linux-amd64"),
    ("armv8", "agnes-hub-go-linux-arm64"),
]

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
VAR_DIR="${SYNOPKG_PKGVAR:-/var/packages/$PKG_NAME/var}"
mkdir -p "$VAR_DIR/data" 2>/dev/null || true
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
    build = os.environ.get("SPK_BUILD", "1")
    if not build.isdigit():
        sys.exit("[ERROR] SPK_BUILD 必须是纯数字，当前为 %r" % build)
    return "%s-%04d" % (go_version, int(build))


# ---------------------------------------------------------------------------
# 图标：纯标准库裁切 + 缩放
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
            if bd != 8 or ct != 6 or inter != 0:
                sys.exit("[ERROR] %s 只支持 8 位 RGBA 非隔行 PNG（当前 bitdepth=%d colortype=%d interlace=%d）"
                         % (path, bd, ct, inter))
        elif typ == b"IDAT":
            idat += body
        elif typ == b"IEND":
            break
        off += 12 + ln

    raw = zlib.decompress(idat)
    stride = w * 4
    px = bytearray(w * h * 4)
    prev = bytearray(stride)
    pos = 0
    for y in range(h):
        ft = raw[pos]
        pos += 1
        line = bytearray(raw[pos:pos + stride])
        pos += stride
        if ft == 1:
            for i in range(4, stride):
                line[i] = (line[i] + line[i - 4]) & 0xFF
        elif ft == 2:
            for i in range(stride):
                line[i] = (line[i] + prev[i]) & 0xFF
        elif ft == 3:
            for i in range(stride):
                a = line[i - 4] if i >= 4 else 0
                line[i] = (line[i] + ((a + prev[i]) >> 1)) & 0xFF
        elif ft == 4:
            for i in range(stride):
                a = line[i - 4] if i >= 4 else 0
                b = prev[i]
                c = prev[i - 4] if i >= 4 else 0
                p = a + b - c
                pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                pr = a if (pa <= pb and pa <= pc) else (b if pb <= pc else c)
                line[i] = (line[i] + pr) & 0xFF
        px[y * stride:(y + 1) * stride] = line
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


def _box_resize(src_w, src_h, src, dst_w, dst_h):
    """面积平均缩放（box filter），按 alpha 预乘后加权，避免透明边缘出现色边。

    同时适用于放大和缩小：每个目标像素取源图上对应的矩形区域做面积加权平均。
    """
    out = bytearray(dst_w * dst_h * 4)
    sx = src_w / dst_w
    sy = src_h / dst_h
    for ty in range(dst_h):
        y0, y1 = ty * sy, (ty + 1) * sy
        iy0, iy1 = int(y0), min(int(y1 - 1e-9) + 1, src_h)
        for tx in range(dst_w):
            x0, x1 = tx * sx, (tx + 1) * sx
            ix0, ix1 = int(x0), min(int(x1 - 1e-9) + 1, src_w)
            ar = ag = ab = aa = 0.0
            total = 0.0
            for yy in range(iy0, iy1):
                wy = min(y1, yy + 1) - max(y0, yy)
                if wy <= 0:
                    continue
                base = yy * src_w * 4
                for xx in range(ix0, ix1):
                    wx = min(x1, xx + 1) - max(x0, xx)
                    if wx <= 0:
                        continue
                    wgt = wx * wy
                    i = base + xx * 4
                    a = src[i + 3]
                    aa += a * wgt
                    ar += src[i] * a * wgt
                    ag += src[i + 1] * a * wgt
                    ab += src[i + 2] * a * wgt
                    total += wgt
            o = (ty * dst_w + tx) * 4
            if total <= 0 or aa <= 0:
                out[o] = out[o + 1] = out[o + 2] = out[o + 3] = 0
            else:
                out[o] = int(round(ar / aa))
                out[o + 1] = int(round(ag / aa))
                out[o + 2] = int(round(ab / aa))
                out[o + 3] = int(round(aa / total))
    return out


def make_icon_set(src_png, targets):
    """从 assets/ICON.PNG 生成任意尺寸的方形图标。

    源图是「左侧方形 App 图标 + 右侧标语文字」的横幅（当前 270×114）。
    方形图标就贴在左上角，所以取 min(宽,高) 作为边长从 (0,0) 裁切。
    若以后把源图换成真正的方形图标，这个规则会自然退化为「整图」。

    targets 是 [(边长, 输出路径), ...]。群晖 SPK 要 64/256，
    套件中心的缩略图要 72/256，所以尺寸做成参数而不是写死。
    """
    w, h, px = _png_decode_rgba(src_png)
    side = min(w, h)
    if side < 16:
        sys.exit("[ERROR] %s 尺寸过小（%dx%d），无法生成图标" % (src_png, w, h))

    # 裁出左上角 side×side 的方形
    crop = bytearray(side * side * 4)
    for y in range(side):
        s = y * w * 4
        crop[y * side * 4:(y + 1) * side * 4] = px[s:s + side * 4]

    for size, dst in targets:
        scaled = _box_resize(side, side, crop, size, size)
        _png_encode_rgba(dst, size, size, scaled)
    return w, h, side


def make_icons(src_png, out_64, out_256):
    """群晖 SPK 要求的两个方形图标（64×64 与 256×256）。"""
    return make_icon_set(src_png, [(64, out_64), (256, out_256)])


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


def build_package_tgz(binary_path, out_path):
    """内层 package.tgz：只放本架构的二进制，解压后位于 SYNOPKG_PKGDEST 根下。

    群晖官方明确要求不要把多平台二进制打进同一个 spk，所以这里只有一份。
    """
    buf = io.BytesIO()
    with _tar_gz_writer(buf) as tar:
        _add_file(tar, binary_path, "agnes-hub-go", mode=0o755)
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
        'arch="%s"' % arch,
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

    # 1. 内层包
    pkg_tgz = os.path.join(d, "package.tgz")
    pkg_data = build_package_tgz(binary_src, pkg_tgz)
    md5 = hashlib.md5(pkg_data).hexdigest()

    # 2. 生命周期脚本：六个钩子必须全部存在，缺一个会被判「套件损坏」
    _write(os.path.join(scripts_dir, "start-stop-status"),
           START_STOP_STATUS.replace("%APP_ID%", APP_ID).replace("%PORT%", str(SERVICE_PORT)))
    _write(os.path.join(scripts_dir, "postinst"), POSTINST.replace("%APP_ID%", APP_ID))
    _write(os.path.join(scripts_dir, "postupgrade"), POSTUPGRADE.replace("%APP_ID%", APP_ID))
    for name in ("preinst", "preuninst", "postuninst", "preupgrade"):
        _write(os.path.join(scripts_dir, name), TRIVIAL)

    # 3. conf：privilege 决定运行身份，缺失会被拒绝安装。
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

    # 4. 图标
    make_icons(icon_src, os.path.join(d, "PACKAGE_ICON.PNG"),
               os.path.join(d, "PACKAGE_ICON_256.PNG"))

    # 5. INFO
    extractsize_kb = (os.path.getsize(binary_src) + 1023) // 1024
    _write(os.path.join(d, "INFO"), build_info(arch, spk_ver, md5, extractsize_kb), mode=0o644)

    return d, md5


def build_spk(bundle, out_path):
    os.makedirs(OUT_DIR, exist_ok=True)
    with open(out_path, "wb") as fout:
        with _tar_gz_writer(fout) as tar:
            _add_file(tar, os.path.join(bundle, "INFO"), "INFO", mode=0o644)
            _add_file(tar, os.path.join(bundle, "package.tgz"), "package.tgz", mode=0o644)
            _add_dir_recursive(tar, os.path.join(bundle, "scripts"), "scripts")
            _add_dir_recursive(tar, os.path.join(bundle, "conf"), "conf")
            _add_file(tar, os.path.join(bundle, "PACKAGE_ICON.PNG"), "PACKAGE_ICON.PNG", mode=0o644)
            _add_file(tar, os.path.join(bundle, "PACKAGE_ICON_256.PNG"), "PACKAGE_ICON_256.PNG", mode=0o644)
    return out_path


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
    args = ap.parse_args()
    targets = select_targets(args.arch)

    go_version = read_version()
    spk_ver = spk_version(go_version)
    icon_src = os.path.join(ROOT, "assets", "ICON.PNG")
    if not os.path.exists(icon_src):
        sys.exit("[ERROR] 缺少图标源文件 assets/ICON.PNG")

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
    print("图标源     : assets/ICON.PNG")
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
