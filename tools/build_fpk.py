#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""构建 agnes-hub-go 的飞牛 fnOS fpk 安装包。

为什么手写 tarfile 而不用 fnpack：
    本机 fnpack.exe 在 MSYS 环境下 `build` 必然失败
    （`Copy pack ... to tmp dir ... error : CreateFile ... The system cannot find the file specified`，
     路径转换所致）。双 tar.gz 结构本身很简单，手搓反而可控。

产出结构：
    .fpk (外层 tar.gz)
      ├── manifest              key=value 文本，不是 JSON（写成 JSON 会让 appcenter 报 code 10111）
      ├── manifest.checksum     内层 app.tgz 的 MD5
      ├── ICON.PNG / ICON_256.PNG
      └── app.tgz               内层 tar.gz
          ├── app/              双架构二进制
          ├── cmd/              生命周期脚本
          ├── config/           privilege + resource（缺失会被应用中心拒绝）
          └── wizard/install

用法：
    python tools/build_fpk.py                 # 输出到 dist/
    FPK_OUT_DIR=D:/somewhere python tools/build_fpk.py
"""
import io
import hashlib
import json
import os
import shutil
import struct
import sys
import tarfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT_DIR = os.environ.get("FPK_OUT_DIR") or os.path.join(ROOT, "dist")
FPK_DIR = os.path.join(ROOT, "fpk-bundle")

APP_ID = "agnes-hub"
VERSION = "1.0.1"
SERVICE_PORT = 4142
# 嵌在二进制里的版本串，upgrade_init 用它判断「这个残留文件是不是本版本的」。
# 必须与 main.go 的 var version 完全一致，否则升级前置清理会把自己刚装的删掉。
VERSION_TAG = "1.0.1"
# 由 VERSION 推导，避免两处手改不同步导致产物名和 manifest 版本对不上。
FPK_NAME = "agnes-hub-go-%s.fpk" % VERSION
APP_DIR = os.path.join(FPK_DIR, "app")
CMD_DIR = os.path.join(FPK_DIR, "cmd")
WIZARD_DIR = os.path.join(FPK_DIR, "wizard")
CONFIG_DIR = os.path.join(FPK_DIR, "config")

MAIN_SCRIPT = '''#!/bin/bash
set -u

APP_DIR="$TRIM_APPDEST"
DATA_DIR=""
if [ -n "${TRIM_PKGVAR:-}" ]; then
  DATA_DIR="$TRIM_PKGVAR/data"
elif [ -f "$APP_DIR/state/datadir" ]; then
  DATA_DIR="$(cat "$APP_DIR/state/datadir")"
fi
[ -z "$DATA_DIR" ] && DATA_DIR="$APP_DIR/data"

PORT="${AGNES_HUB_PORT:-%PORT%}"
LISTEN="0.0.0.0:$PORT"

ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) BIN_NAME="agnes-hub-go" ;;
  aarch64|arm64) BIN_NAME="agnes-hub-go-arm64" ;;
  *) echo "unsupported arch: $ARCH" > "$TRIM_TEMP_LOGFILE"; exit 1 ;;
esac

BIN=""
for cand in "$APP_DIR/app/$BIN_NAME" "$APP_DIR/$BIN_NAME"; do
  [ -x "$cand" ] && BIN="$cand" && break
done
[ -z "$BIN" ] && { echo "binary not found" > "$TRIM_TEMP_LOGFILE"; exit 1; }

mkdir -p "$DATA_DIR" "$APP_DIR/state"
echo "$DATA_DIR" > "$APP_DIR/state/datadir"
echo "$LISTEN" > "$APP_DIR/state/listen"

cd "$(dirname "$BIN")"
nohup "$BIN" -host 0.0.0.0 -port "$PORT" -data "$DATA_DIR" >> "$DATA_DIR/app.log" 2>&1 &
for i in $(seq 1 30); do
  pgrep -f "$BIN" >/dev/null 2>&1 && { sleep 1; exit 0; }
  sleep 1
done
echo "failed to start $BIN" > "$TRIM_TEMP_LOGFILE"
exit 1
'''

UPGRADE_INIT = '''#!/bin/bash
# 升级前清掉旧版二进制（保留数据目录）。
# 不用 pkill -f "agnes-hub-go" —— 会匹配到本脚本自己的命令行。
for pid in $(pgrep -f "agnes-hub-go" 2>/dev/null); do
  [ "$pid" = "$$" ] && continue
  exe=$(readlink "/proc/$pid/exe" 2>/dev/null) || continue
  case "$exe" in
    */agnes-hub-go) kill "$pid" 2>/dev/null || true ;;
  esac
done
sleep 1
BASES=""
[ -n "${TRIM_APPDEST:-}" ] && BASES="$BASES $TRIM_APPDEST"
[ -n "${TRIM_PKGROOT:-}" ] && BASES="$BASES $TRIM_PKGROOT"
for base in $BASES; do
  [ -d "$base" ] || continue
  for cand in "$base/app/agnes-hub-go" "$base/agnes-hub-go"; do
    [ -f "$cand" ] || continue
    grep -aq "%VERSION_TAG%" "$cand" 2>/dev/null || rm -f "$cand"
  done
done
exit 0
'''

INSTALL_INIT = '''#!/bin/bash
set -u
BASES=""
[ -n "${TRIM_APPDEST:-}" ] && BASES="$BASES $TRIM_APPDEST"
[ -n "${TRIM_PKGROOT:-}" ] && BASES="$BASES $TRIM_PKGROOT"
for base in $BASES; do
  [ -d "$base" ] || continue
  for cand in "$base/app/agnes-hub-go" "$base/agnes-hub-go"; do
    [ -f "$cand" ] || continue
    grep -aq "%VERSION_TAG%" "$cand" 2>/dev/null || rm -f "$cand" 2>/dev/null || true
  done
done
exit 0
'''

TRIVIAL = "#!/bin/bash\nexit 0\n"

CHANGELOG = (
    "1.0.1 自更新：内置 GitHub Releases 版本检查与一键更新（SHA256 + 可执行文件魔数双校验）；"
    "Windows 由助手进程在旧进程退出后完成替换并自动重启；修复替换脚本在进程存活时执行导致更新静默失效的问题。"
    "1.0.0 首发：多账号聚合中转、agnes-auto 三模态自动路由、FIFO 严格节拍限流、"
    "软粘性溢出、(账号*池)二维自适应校准、401/403/402 熔断自动复活、生图/视频非幂等不重试。"
)


def _write(path, content, mode=0o755):
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write(content)
    os.chmod(path, mode)


def prepare():
    for d in (APP_DIR, CMD_DIR, WIZARD_DIR, CONFIG_DIR):
        os.makedirs(d, exist_ok=True)

    # 1. 二进制
    for src, dst in (("agnes-hub-go-linux-amd64", "agnes-hub-go"),
                     ("agnes-hub-go-linux-arm64", "agnes-hub-go-arm64")):
        s = os.path.join(ROOT, src)
        if not os.path.exists(s):
            sys.exit("[ERROR] 缺少交叉编译产物 %s，请先执行 build_linux.sh 或 go build" % src)
        shutil.copy2(s, os.path.join(APP_DIR, dst))

    # 2. cmd 生命周期脚本
    _write(os.path.join(CMD_DIR, "main"), MAIN_SCRIPT.replace("%PORT%", str(SERVICE_PORT)))
    _write(os.path.join(CMD_DIR, "upgrade_init"), UPGRADE_INIT.replace("%VERSION_TAG%", VERSION_TAG))
    _write(os.path.join(CMD_DIR, "install_init"), INSTALL_INIT.replace("%VERSION_TAG%", VERSION_TAG))
    for name in ("upgrade_callback", "install_callback", "config_callback",
                 "uninstall_init", "uninstall_callback"):
        _write(os.path.join(CMD_DIR, name), TRIVIAL)
    _write(os.path.join(CMD_DIR, "config_init"),
           "#!/bin/bash\npkill -f 'agnes-hub-go' 2>/dev/null || true\nexit 0\n")

    # 3. wizard（无交互安装向导：空数组即可）
    with open(os.path.join(WIZARD_DIR, "install"), "w", encoding="utf-8") as f:
        json.dump([], f, ensure_ascii=False)

    # 4. config —— 缺失会让应用中心拒绝安装，容易漏
    with open(os.path.join(CONFIG_DIR, "privilege"), "w", encoding="utf-8") as f:
        json.dump({"defaults": {"run-as": "package"},
                   "username": APP_ID, "groupname": APP_ID}, f, ensure_ascii=False, indent=2)
    with open(os.path.join(CONFIG_DIR, "resource"), "w", encoding="utf-8") as f:
        json.dump({}, f, ensure_ascii=False)

    # 5. manifest —— 必须是 key=value 行文本
    lines = [
        "appname=%s" % APP_ID,
        "version=%s" % VERSION,
        "display_name=Agnes Hub",
        "desc=Agnes AI 多账号聚合中转 + RPM 限流排队网关。统一模型 agnes-auto 自动判定文生/生图/生视频；"
        "FIFO 严格节拍、软粘性溢出、二维自适应校准、熔断自动复活。",
        "source=thirdparty",
        "platform=x86",
        "arch=x86_64",
        "maintainer=my788525",
        "maintainer_url=https://github.com/my788525/agnes-hub-go",
        "os_min_version=0.9.0",
        "service_port=%d" % SERVICE_PORT,
        "checkport=false",
        "ctl_stop=true",
        "changelog=%s" % CHANGELOG,
    ]
    with open(os.path.join(FPK_DIR, "manifest"), "w", encoding="utf-8", newline="\n") as f:
        f.write("\n".join(lines) + "\n")

    # 6. 图标（源文件提交在 assets/）
    for name in ("ICON.PNG", "ICON_256.PNG"):
        src = os.path.join(ROOT, "assets", name)
        if os.path.exists(src):
            shutil.copy2(src, os.path.join(FPK_DIR, name))
        else:
            _placeholder_png(os.path.join(FPK_DIR, name), 64 if name == "ICON.PNG" else 256)


def _placeholder_png(path, size):
    import zlib
    sig = b"\x89PNG\r\n\x1a\n"

    def chunk(tag, data):
        body = tag + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF)

    ihdr = chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 2, 0, 0, 0))
    raw = b"".join(b"\x00" + b"\xff\xff\xff" * size for _ in range(size))
    with open(path, "wb") as f:
        f.write(sig + ihdr + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))


def build_inner():
    """内层 app.tgz。uid/gid 归零 + mtime 归零，保证可复现。"""
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz", compresslevel=9,
                      format=tarfile.GNU_FORMAT) as tar:
        exec_names = {"agnes-hub-go", "agnes-hub-go-arm64", "main",
                      "upgrade_init", "install_init"}

        def add(full, arc):
            ti = tar.gettarinfo(full, arcname=arc)
            ti.uid = ti.gid = 0
            ti.uname = ti.gname = "root"
            ti.mtime = 0
            if ti.isdir():
                ti.mode = 0o755
                tar.addfile(ti)
                for child in sorted(os.listdir(full)):
                    add(os.path.join(full, child), arc + "/" + child)
            else:
                ti.mode = 0o755 if os.path.basename(arc) in exec_names else 0o644
                with open(full, "rb") as fh:
                    tar.addfile(ti, fh)

        for entry in sorted(os.listdir(APP_DIR)):
            add(os.path.join(APP_DIR, entry), "app/" + entry)
        for entry in sorted(os.listdir(CMD_DIR)):
            add(os.path.join(CMD_DIR, entry), "cmd/" + entry)
        add(WIZARD_DIR, "wizard")
        add(CONFIG_DIR, "config")

    data = buf.getvalue()
    md5 = hashlib.md5(data).hexdigest()
    with open(os.path.join(FPK_DIR, "manifest.checksum"), "w", encoding="utf-8") as f:
        f.write(md5 + "\n")
    return data, md5


def build_outer(app_data):
    os.makedirs(OUT_DIR, exist_ok=True)
    out = os.path.join(OUT_DIR, FPK_NAME)
    with open(out, "wb") as fout:
        with tarfile.open(fileobj=fout, mode="w:gz", compresslevel=9,
                          format=tarfile.GNU_FORMAT) as outer:
            for name in ("manifest", "manifest.checksum", "ICON.PNG", "ICON_256.PNG"):
                full = os.path.join(FPK_DIR, name)
                ti = outer.gettarinfo(full, arcname=name)
                ti.uid = ti.gid = 0
                ti.uname = ti.gname = "root"
                ti.mtime = 0
                ti.mode = 0o644
                with open(full, "rb") as fh:
                    outer.addfile(ti, fh)

            ti = tarfile.TarInfo(name="app.tgz")
            ti.size = len(app_data)
            ti.uid = ti.gid = 0
            ti.uname = ti.gname = "root"
            ti.mtime = 0
            ti.mode = 0o644
            outer.addfile(ti, io.BytesIO(app_data))
    return out


def main():
    prepare()
    app_data, md5 = build_inner()
    out = build_outer(app_data)
    size = os.path.getsize(out)
    print("内层 app.tgz : %d bytes  md5=%s" % (len(app_data), md5))
    print("fpk 产物     : %s" % out)
    print("体积         : %.1f MB" % (size / 1024 / 1024))
    print()
    print("安装（需 sudo）：appcenter-cli install-fpk %s" % out)


if __name__ == "__main__":
    main()
