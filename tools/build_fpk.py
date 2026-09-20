#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""构建 agnes-hub-go 的飞牛 fnOS fpk 安装包。

为什么手写 tarfile 而不用 fnpack：
    本机 fnpack.exe 在 MSYS 环境下 `build` 必然失败
    （`Copy pack ... to tmp dir ... error : CreateFile ... The system cannot find the file specified`，
     路径转换所致）。双 tar.gz 结构本身很简单，手搓反而可控。

产出结构（对齐 fnOS 官方可装 fpk，如 M365-Copilot2API-FNOS）：
    .fpk (外层 tar.gz)
      ├── manifest              key=value 文本，不是 JSON（写成 JSON 会让 appcenter 报 code 10111）
      │                         —— 内含 checksum=<app.tgz 的 MD5>，应用中心据此校验完整性
      ├── ICON.PNG / ICON_256.PNG
      ├── cmd/                  生命周期脚本（main/install_init/upgrade_init/... 必须置于外层）
      ├── config/               privilege + resource（缺失会被应用中心拒绝）
      ├── wizard/install        无交互安装向导（空数组即可）
      └── app.tgz               内层 tar.gz，只放 app/ 双架构二进制
          ├── app/agnes-hub-go
          └── app/agnes-hub-go-arm64

关键坑：cmd/config/wizard 必须放在**外层** tar；塞进 app.tgz 会导致
应用中心找不到 cmd/main 而安装失败。fnOS 不要求独立的 manifest.checksum 文件。

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
VERSION = "1.0.2"
SERVICE_PORT = 4142
# 嵌在二进制里的版本串，upgrade_init 用它判断「这个残留文件是不是本版本的」。
# 必须与 main.go 的 var version 完全一致，否则升级前置清理会把自己刚装的删掉。
VERSION_TAG = "1.0.2"
# 由 VERSION 推导，避免两处手改不同步导致产物名和 manifest 版本对不上。
FPK_NAME = "agnes-hub-go-%s.fpk" % VERSION
APP_DIR = os.path.join(FPK_DIR, "app")
CMD_DIR = os.path.join(FPK_DIR, "cmd")
WIZARD_DIR = os.path.join(FPK_DIR, "wizard")
CONFIG_DIR = os.path.join(FPK_DIR, "config")

# 对齐 fnOS 官方可装 fpk（M365-Copilot2API-FNOS）的 cmd/main 形态：
# fnOS 应用中心会以 `cmd/main start` 拉起、`cmd/main stop` 停止、
# `cmd/main status` 探活（期望运行中返回 0、未运行返回 3）。
# 无参数调用默认按 start 处理，兼容 appcenter 直接 `cmd/main` 的场景。
MAIN_SCRIPT = '''#!/bin/bash

# fnOS 会把 app.tgz 的内容解压到 /var/apps/<app_id>/target（= /vol1/@appcenter/<app_id>/），
# 并且**额外套一层 <app_id>/ 子目录** —— 即真实二进制在 $APP_DIR/$APP_ID/。
# 参考 jdbeanbot：/vol1/@appcenter/jdbeanbot/node/bin/node ... （扁平，无额外子目录）
# 我们的包内没有顶层 jdbeanbot 那样的扁平布局，所以需要显式找 $APP_ID 子目录。
APP_ID="agnes-hub"
APP_DIR="$TRIM_APPDEST"

# 安全兜底：fnOS 在 hook 阶段未必传 TRIM_TEMP_LOGFILE，未定义时会因 set -u 崩溃
TRIM_TEMP_LOGFILE="${TRIM_TEMP_LOGFILE:-/tmp/agnes-hub-main-fallback.log}"

# 数据目录解析：优先用 fnOS 生命周期变量，否则读取 start 时持久化的路径
DATA_DIR=""
if [ -n "${TRIM_PKGVAR:-}" ]; then
  DATA_DIR="$TRIM_PKGVAR/data"
elif [ -n "$APP_DIR" ] && [ -f "$APP_DIR/$APP_ID/state/datadir" ]; then
  DATA_DIR="$(cat "$APP_DIR/$APP_ID/state/datadir" 2>/dev/null)"
fi
[ -z "$DATA_DIR" ] && DATA_DIR="$APP_DIR/$APP_ID/data"

PORT="${AGNES_HUB_PORT:-%PORT%}"

# 定位二进制：fnOS 实际路径为 $APP_DIR/$APP_ID/<bin>
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) BIN_NAME="agnes-hub-go" ;;
  aarch64|arm64) BIN_NAME="agnes-hub-go-arm64" ;;
  *) echo "unsupported arch: $ARCH" > "$TRIM_TEMP_LOGFILE"; exit 1 ;;
esac

BIN=""
for cand in "$APP_DIR/$APP_ID/$BIN_NAME" "$APP_DIR/$BIN_NAME" "$APP_DIR/app/$BIN_NAME"; do
  if [ -x "$cand" ]; then
    BIN="$cand"
    break
  fi
done
if [ -z "$BIN" ]; then
  echo "binary not found (checked: $APP_DIR/$APP_ID/$BIN_NAME, $APP_DIR/$BIN_NAME, $APP_DIR/app/$BIN_NAME)" > "$TRIM_TEMP_LOGFILE"
  exit 1
fi

mkdir -p "$DATA_DIR" "$APP_DIR/$APP_ID/state"
echo "$DATA_DIR" > "$APP_DIR/$APP_ID/state/datadir"

# -host 0.0.0.0 由 main.go 内部展开为 IPv4(0.0.0.0)+IPv6(::) 双栈，
# 满足飞牛外网 IPv6 域名直达 + 局域网 IPv4 访问。
start_service() {
  cd "$(dirname "$BIN")"
  nohup "$BIN" -host 0.0.0.0 -port "$PORT" -data "$DATA_DIR" >> "$DATA_DIR/app.log" 2>&1 &
  for i in $(seq 1 30); do
    if pgrep -f "$BIN" >/dev/null 2>&1; then
      sleep 1
      exit 0
    fi
    sleep 1
  done
  echo "failed to start $BIN" > "$TRIM_TEMP_LOGFILE"
  exit 1
}

cmd="${1:-}"; shift || true

case "$cmd" in
  start)
    start_service
    ;;
  stop)
    pkill -f "$BIN" 2>/dev/null || true
    exit 0
    ;;
  status)
    if pgrep -f "$BIN" >/dev/null 2>&1; then
      exit 0
    fi
    exit 3
    ;;
  *)
    # 无参数默认 start
    start_service
    ;;
esac
'''

UPGRADE_INIT = '''#!/bin/bash
# 升级前清掉旧版二进制（保留数据目录）。
# 注意：fnOS 会把 app.tgz 内容解压到 $TRIM_APPDEST/$APP_ID/，因此扫描路径要带 $APP_ID。
# 不用 pkill -f "agnes-hub-go" —— 会匹配到本脚本自己的命令行。
APP_ID="agnes-hub"
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
  for cand in "$base/$APP_ID/app/agnes-hub-go" "$base/$APP_ID/agnes-hub-go" "$base/app/agnes-hub-go" "$base/agnes-hub-go"; do
    [ -f "$cand" ] || continue
    grep -aq "%VERSION_TAG%" "$cand" 2>/dev/null || rm -f "$cand"
  done
done
exit 0
'''

INSTALL_INIT = '''#!/bin/bash
# 安装前清掉残留旧版二进制（避免版本混乱）。
APP_ID="agnes-hub"
BASES=""
[ -n "${TRIM_APPDEST:-}" ] && BASES="$BASES $TRIM_APPDEST"
[ -n "${TRIM_PKGROOT:-}" ] && BASES="$BASES $TRIM_PKGROOT"
for base in $BASES; do
  [ -d "$base" ] || continue
  for cand in "$base/$APP_ID/app/agnes-hub-go" "$base/$APP_ID/agnes-hub-go" "$base/app/agnes-hub-go" "$base/agnes-hub-go"; do
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

    # 3. wizard（必须是非空数组，且 items 不能为空——fnOS 会报 "wizard items is empty"）
    # 注意：必须有 password 类型字段，否则校验失败（code 10150）
    # 参考 M365/Copilot2API 和 jdbeanbot 的 wizard/install 格式
    wizard_content = json.dumps([{
        "stepTitle": "Agnes Hub 配置",
        "items": [
            {
                "type": "password",
                "field": "wizard_admin_password",
                "label": "管理员密码",
                "helpText": "设置网页管理后台的管理员密码（建议 ≥8 位，含字母和数字）。安装或重新安装时，只要本字段与上次安装时不同，就会直接把管理员密码重置为本值（忘记密码时重装一次即可）。日常重启不会覆盖在网页端修改过的密码。"
            },
            {
                "type": "tips",
                "helpText": "服务端口固定为 4142（无需填写）：安装完成后通过飞牛桌面图标或浏览器访问 http://NAS的IP:4142 进入管理页，添加 AI 账号并生成 API Key。账号与配置保存在应用数据目录，卸载时自动保留。"
            }
        ]
    }], ensure_ascii=False, indent=2)
    with open(os.path.join(WIZARD_DIR, "install"), "w", encoding="utf-8") as f:
        f.write(wizard_content)

    # 4. config —— 缺失会让应用中心拒绝安装，容易漏
    # 注意：resource 必须输出 `{\\n}`（带换行），不能是 `{}`，否则 fnOS 校验失败
    with open(os.path.join(CONFIG_DIR, "privilege"), "w", encoding="utf-8") as f:
        json.dump({"defaults": {"run-as": "package"},
                   "username": APP_ID, "groupname": APP_ID}, f, ensure_ascii=False, indent=2)
        f.write("\n")
    with open(os.path.join(CONFIG_DIR, "resource"), "w", encoding="utf-8") as f:
        json.dump({}, f, ensure_ascii=False)
        f.write("\n")

    # 5. manifest —— 必须按 fnOS 官方可装包（如 M365-Copilot2API-FNOS）的格式：
    #    key 右填满 22 字符 + 空格 + "=" + 空格 + 值。裸 "key=value" 会让
    #    appcenter 解析失败，报 code 10111。
    #    注意：必须包含 desktop_uidir 和 desktop_applaunchname 字段（即使服务无UI），
    #    否则 appcenter 解析时可能报错。
    KEY_WIDTH = 22
    field_pairs = [
        ("appname", APP_ID),
        ("version", VERSION),
        ("display_name", "Agnes Hub"),
        ("desc", "Agnes AI 多账号聚合中转 + RPM 限流排队网关。统一模型 agnes-auto 自动判定文生/生图/生视频；"
                  "FIFO 严格节拍、软粘性溢出、二维自适应校准、熔断自动复活。"),
        ("source", "thirdparty"),
        ("platform", "x86"),
        ("arch", "x86_64"),
        ("maintainer", "my788525"),
        ("maintainer_url", "https://github.com/my788525/agnes-hub-go"),
        ("os_min_version", "0.9.0"),
        ("desktop_uidir", ""),  # 服务无UI，留空
        ("desktop_applaunchname", ""),  # 服务无桌面快捷方式，留空
        ("service_port", str(SERVICE_PORT)),
        ("checkport", "false"),
        ("ctl_stop", "true"),
        ("changelog", CHANGELOG),
    ]

    lines = ["%s = %s" % (k.ljust(22), v) for k, v in field_pairs]
    # fnOS manifest 必须使用 CRLF 行尾（M365 官方包均为 CRLF）
    # 注意：不要加注释行（jdbeanbot 的成功包就没有），注释行可能干扰解析
    with open(os.path.join(FPK_DIR, "manifest"), "wb") as f:
        f.write(("\r\n".join(lines) + "\r\n").encode("utf-8"))

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
    """内层 app.tgz：包含完整的 app 目录树（对齐 M365 结构）。

    对齐 fnOS 官方 fpk（如 M365-Copilot2API-FNOS）：
    - app.tgz 根目录是一个以 APP_ID 命名的子目录
    - 该子目录下放：二进制、config/、cmd/ 等完整目录树
    - 应用中心会将 app.tgz 内容解压到 /var/apps/<app_id>/
    """
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz", compresslevel=9,
                      format=tarfile.GNU_FORMAT) as tar:
        root_arc = APP_ID

        # 添加根目录条目（必须用正斜杠，避免 Windows 产生反斜杠路径）
        ti = tarfile.TarInfo(name=root_arc + "/")
        ti.type = tarfile.DIRTYPE
        ti.mode = 0o755
        ti.uid = ti.gid = 0
        ti.uname = ti.gname = "root"
        ti.mtime = 0
        tar.addfile(ti)

        # 复制二进制到根目录
        for src_name, dst_name in (("agnes-hub-go-linux-amd64", "agnes-hub-go"),
                                    ("agnes-hub-go-linux-arm64", "agnes-hub-go-arm64")):
            src = os.path.join(ROOT, src_name)
            if not os.path.exists(src):
                sys.exit("[ERROR] 缺少交叉编译产物 %s，请先执行 build_linux.sh 或 go build" % src_name)
            arc = root_arc + "/" + dst_name
            _add_file(tar, src, arc, mode=0o755)

        # 复制 config/ 目录
        if os.path.isdir(CONFIG_DIR):
            _add_dir_recursive(tar, CONFIG_DIR, root_arc + "/config")

        # 复制 cmd/ 目录
        if os.path.isdir(CMD_DIR):
            _add_dir_recursive(tar, CMD_DIR, root_arc + "/cmd")

        # 复制 wizard/ 目录
        if os.path.isdir(WIZARD_DIR):
            _add_dir_recursive(tar, WIZARD_DIR, root_arc + "/wizard")

    data = buf.getvalue()
    md5 = hashlib.md5(data).hexdigest()
    return data, md5


def _add_file(outer, full, arc, mode=0o644):
    ti = outer.gettarinfo(full, arcname=arc)
    ti.uid = ti.gid = 0
    ti.uname = ti.gname = "root"
    ti.mtime = 0
    ti.mode = mode
    with open(full, "rb") as fh:
        outer.addfile(ti, fh)


def _add_dir_recursive(outer, full, arc):
    """把目录递归加进外层 tar，脚本统一 0755、其余 0644。"""
    ti = tarfile.TarInfo(name=arc)
    ti.type = tarfile.DIRTYPE
    ti.mode = 0o755
    ti.uid = ti.gid = 0
    ti.uname = ti.gname = "root"
    ti.mtime = 0
    outer.addfile(ti)
    for child in sorted(os.listdir(full)):
        cfull = os.path.join(full, child)
        carc = arc + "/" + child
        if os.path.isdir(cfull):
            _add_dir_recursive(outer, cfull, carc)
        else:
            mode = 0o755 if (carc.startswith("cmd/") or os.access(cfull, os.X_OK)) else 0o644
            _add_file(outer, cfull, carc, mode)


def build_outer(app_data, md5):
    os.makedirs(OUT_DIR, exist_ok=True)
    out = os.path.join(OUT_DIR, FPK_NAME)
    with open(out, "wb") as fout:
        with tarfile.open(fileobj=fout, mode="w:gz", compresslevel=9,
                          format=tarfile.GNU_FORMAT) as outer:
            # 顺序对齐已知可装的 fnOS fpk（M365-Copilot2API-FNOS）：
            # manifest → cmd → config → wizard → icons → app.tgz
            # （不单独带 manifest.checksum 文件；完整性由 manifest 内的 checksum= 字段保证）
            # manifest 内每条 key 都右填满 22 字符，对齐 " = " 分隔列。
            _add_file(outer, os.path.join(FPK_DIR, "manifest"), "manifest")
            for d in ("cmd", "config", "wizard"):
                _add_dir_recursive(outer, os.path.join(FPK_DIR, d), d)
            for name in ("ICON.PNG", "ICON_256.PNG"):
                _add_file(outer, os.path.join(FPK_DIR, name), name)

            ti = tarfile.TarInfo(name="app.tgz")
            ti.size = len(app_data)
            ti.uid = ti.gid = 0
            ti.uname = ti.gname = "root"
            ti.mtime = 0
            ti.mode = 0o644
            outer.addfile(ti, io.BytesIO(app_data))
    return out


def _stamp_manifest_checksum(md5):
    """往 manifest 末尾补 checksum= 字段（对齐 fnOS 官方参考 fpk）。"""
    mp = os.path.join(FPK_DIR, "manifest")
    with open(mp, "r", encoding="utf-8") as f:
        lines = f.read().splitlines()
    if not any(l.split("=", 1)[0].strip() == "checksum" for l in lines):
        lines.append("%s = %s" % ("checksum".ljust(22), md5))
    with open(mp, "w", encoding="utf-8", newline="\r\n") as f:
        f.write("\n".join(lines) + "\n")


def main():
    prepare()
    app_data, md5 = build_inner()
    _stamp_manifest_checksum(md5)
    out = build_outer(app_data, md5)
    size = os.path.getsize(out)
    print("内层 app.tgz : %d bytes  md5=%s" % (len(app_data), md5))
    print("fpk 产物     : %s" % out)
    print("体积         : %.1f MB" % (size / 1024 / 1024))
    print()
    print("安装（需 sudo）：appcenter-cli install-fpk %s" % out)


if __name__ == "__main__":
    main()
