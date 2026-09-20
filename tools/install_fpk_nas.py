#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 build_fpk.py 产出的 .fpk 上传到飞牛 fnOS 并用应用中心正式安装（常驻）。

与 deploy_nas.py（前台联调）的区别：本脚本走 appcenter-cli install-fpk 正式安装，
安装后由飞牛应用中心负责生命周期（开机自启 / 桌面快捷方式 / 升级保留数据）。

凭据来源（按优先级，脚本内不存任何明文口令）：
    1. 环境变量 NAS_PASS
    2. 环境变量 NAS_PASS_FILE 指向的文件
    3. 仓库根目录下的 .nas_pass 文件（已 gitignore）
    4. D:/work/M365-fpk/.nas_pass（历史位置兜底）

用法：
    python tools/install_fpk_nas.py --host nas.lianu.com --fpk dist/agnes-hub-go-1.0.3.fpk
"""
import argparse
import os
import sys
import time

try:
    import paramiko
except ImportError:
    sys.exit("[ERROR] 需要 paramiko：pip install paramiko")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
APP_ID = "agnes-hub"
# 历史位置的口令文件兜底（与 M365-Copilot2API 共用同一台 NAS）
FALLBACK_PASS_FILES = (
    os.path.join(ROOT, ".nas_pass"),
    "D:/work/M365-fpk/.nas_pass",
)


def load_password():
    pw = os.environ.get("NAS_PASS")
    if pw:
        return pw.strip()
    path = os.environ.get("NAS_PASS_FILE")
    candidates = ([path] if path else []) + list(FALLBACK_PASS_FILES)
    for p in candidates:
        if p and os.path.exists(p):
            with open(p, encoding="utf-8") as f:
                return f.read().strip()
    sys.exit("[ERROR] 未找到口令：请设 NAS_PASS，或把口令写入 %s" % FALLBACK_PASS_FILES[0])


class Nas:
    def __init__(self, host, user, password, timeout=15):
        self.password = password
        self.client = paramiko.SSHClient()
        self.client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
        self.client.connect(host, username=user, password=password, timeout=timeout)

    def sudo(self, cmd, timeout=60):
        """经 pty 执行 sudo 命令并回收输出（直到命令退出或超时）。"""
        chan = self.client.get_transport().open_session()
        chan.get_pty(width=200, height=50)
        chan.set_combine_stderr(True)
        chan.exec_command("echo '%s' | sudo -S %s" % (self.password, cmd))
        out, start = "", time.time()
        while True:
            if chan.recv_ready():
                out += chan.recv(4096).decode("utf-8", errors="replace")
            if chan.exit_status_ready():
                # 收尾把缓冲读干净
                while chan.recv_ready():
                    out += chan.recv(4096).decode("utf-8", errors="replace")
                break
            if time.time() - start > timeout:
                out += "\n[TIMEOUT after %ss]" % timeout
                break
            time.sleep(0.05)
        try:
            chan.close()
        except Exception:
            pass
        return "\n".join(l for l in out.splitlines()
                         if "Could not chdir to home" not in l and "[sudo] password" not in l)

    def put(self, local, remote):
        sftp = self.client.open_sftp()
        try:
            sftp.put(local, remote)
        finally:
            sftp.close()

    def close(self):
        self.client.close()


def show(title, text):
    print("\n%s" % title)
    for line in (text or "").splitlines():
        print("  " + line)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="nas.lianu.com")
    ap.add_argument("--user", default="admin")
    ap.add_argument("--fpk", default=os.path.join(ROOT, "dist", "agnes-hub-go-1.0.3.fpk"))
    ap.add_argument("--install-timeout", type=int, default=240)
    args = ap.parse_args()

    if not os.path.exists(args.fpk):
        sys.exit("[ERROR] 缺少 fpk：%s（先跑 python tools/build_fpk.py）" % args.fpk)

    password = load_password()
    print("=" * 62)
    print(" agnes-hub 飞牛应用中心正式安装")
    print(" 包：%s（%.1f MB）" % (args.fpk, os.path.getsize(args.fpk) / 1048576))
    print("=" * 62)

    print("\n[1/5] 连接 %s ..." % args.host)
    nas = Nas(args.host, args.user, password)
    print("      ✓ 已连接")

    print("\n[2/5] 安装前状态 ...")
    show("已装应用目录：", nas.sudo("ls -d /vol1/@appcenter/%s 2>/dev/null && "
                                    "grep -ao '1\\.0\\.[0-9]*' $(find /vol1/@appcenter/%s -name 'agnes-hub-go*' -type f 2>/dev/null | head -1) 2>/dev/null | head -1 "
                                    "|| echo '（未安装或无二进制）'" % (APP_ID, APP_ID)))
    show("运行中进程：", nas.sudo("pgrep -af agnes-hub-go | head -3 || echo '（无）'"))
    show("健康检查：", nas.sudo("curl -s -m 3 http://localhost:4142/healthz || echo '（无响应）'"))

    print("\n[3/5] 上传 fpk 到 /tmp ...")
    remote_fpk = "/tmp/" + os.path.basename(args.fpk)
    t0 = time.time()
    nas.put(args.fpk, remote_fpk)
    print("      ✓ 上传完成（%.1fs）" % (time.time() - t0))

    print("\n[4/5] appcenter-cli install-fpk（最长 %ds）..." % args.install_timeout)
    out = nas.sudo("appcenter-cli install-fpk %s" % remote_fpk, timeout=args.install_timeout)
    show("安装输出：", out)

    print("\n[5/5] 安装后验证 ...")
    show("运行中进程：", nas.sudo("pgrep -af agnes-hub-go | head -3 || echo '（无）'"))
    show("健康检查：", nas.sudo("curl -s -m 3 http://localhost:4142/healthz || echo '（无响应）'"))
    show("新版本串：", nas.sudo("for f in $(find /vol1/@appcenter/%s /var/apps/%s -name 'agnes-hub-go*' -type f 2>/dev/null); do "
                                "printf '%%s: ' \"$f\"; grep -ao '1\\.0\\.[0-9]*' \"$f\" | head -1; done" % (APP_ID, APP_ID)))
    show("桌面快捷方式文件：", nas.sudo("find /vol1/@appcenter/%s /var/apps/%s -path '*ui/config' 2>/dev/null | head -3; "
                                        "find /vol1/@appcenter/%s /var/apps/%s -path '*ui/images*' -name 'icon-*.png' 2>/dev/null | head -4" % (APP_ID, APP_ID, APP_ID, APP_ID)))

    nas.sudo("rm -f %s" % remote_fpk)
    nas.close()
    print("\n" + "=" * 62)
    print(" 完成。若进程/健康检查正常，飞牛桌面应出现 Agnes Hub 图标，")
    print(" 点击即打开 http://NAS_IP:4142/console。")
    print("=" * 62)


if __name__ == "__main__":
    main()
