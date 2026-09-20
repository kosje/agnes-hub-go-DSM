#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 agnes-hub-go 手工部署到飞牛 fnOS，并做前台联调验证。

⚠️ 为什么是「前台验证」而不是「后台常驻」：
   飞牛会拦截 SSH 会话里的后台进程 —— `nohup ... &` / `setsid` / `&` 一律立即退出，
   `ps` 与 `ss -ltnp` 都看不到。这是系统级沙箱限制，不是应用 bug。
   真正要常驻必须走**应用中心正式安装**（见 tools/build_fpk.py 产出的 .fpk）。
   本脚本的用途是：不打 fpk 也能快速把新二进制推上去、前台跑起来、验完就撤。

凭据来源（按优先级，脚本内不存任何明文口令）：
    1. 环境变量 NAS_PASS
    2. 环境变量 NAS_PASS_FILE 指向的文件
    3. 仓库根目录下的 .nas_pass 文件（已 gitignore）

用法：
    python tools/deploy_nas.py --host <nas-host> --user admin
    python tools/deploy_nas.py --host <nas-host> --user admin --port 4142
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
DEFAULT_PASS_FILE = os.path.join(ROOT, ".nas_pass")

APP_ID = "agnes-hub"
APP_DIR = "/vol1/@appcenter/" + APP_ID
DATA_DIR = "/vol1/@appdata/" + APP_ID + "/data"
CMD_NAMES = ("main", "upgrade_init", "install_init", "upgrade_callback",
             "install_callback", "config_init", "config_callback",
             "uninstall_init", "uninstall_callback")


def load_password():
    pw = os.environ.get("NAS_PASS")
    if pw:
        return pw.strip()
    path = os.environ.get("NAS_PASS_FILE") or DEFAULT_PASS_FILE
    if os.path.exists(path):
        with open(path, encoding="utf-8") as f:
            return f.read().strip()
    sys.exit("[ERROR] 未找到口令：请设 NAS_PASS 环境变量，或把口令写入 %s" % DEFAULT_PASS_FILE)


class Nas:
    def __init__(self, host, user, password, timeout=10):
        self.password = password
        self.client = paramiko.SSHClient()
        self.client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
        self.client.connect(host, username=user, password=password, timeout=timeout)

    def sudo(self, cmd, timeout=30):
        """经 pty 执行 sudo 命令并回收输出。"""
        chan = self.client.get_transport().open_session()
        chan.get_pty(width=200, height=50)
        chan.set_combine_stderr(True)
        chan.exec_command("echo '%s' | sudo -S %s" % (self.password, cmd))
        out, start = "", time.time()
        while not chan.exit_status_ready():
            if chan.recv_ready():
                out += chan.recv(4096).decode("utf-8", errors="replace")
            if time.time() - start > timeout:
                break
            time.sleep(0.05)
        chan.close()
        # 飞牛 admin 没有家目录，会打印一行无害告警，统一滤掉
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


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="", help="fnOS 主机名或 IP（必填）")
    ap.add_argument("--user", default="admin")
    ap.add_argument("--port", default="4142")
    ap.add_argument("--keep-data", action="store_true",
                    help="保留已有数据目录（默认也保留，此开关仅作显式声明）")
    args = ap.parse_args()

    if not args.host:
        sys.exit("[ERROR] 请用 --host 指定 fnOS 主机名或 IP，"
                 "例如：--host 192.168.1.10 --user admin")

    password = load_password()
    binaries = {
        "agnes-hub-go": os.path.join(ROOT, "agnes-hub-go-linux-amd64"),
        "agnes-hub-go-arm64": os.path.join(ROOT, "agnes-hub-go-linux-arm64"),
    }
    for name, path in binaries.items():
        if not os.path.exists(path):
            sys.exit("[ERROR] 缺少 %s，请先交叉编译" % path)

    fpk_bundle = os.path.join(ROOT, "fpk-bundle")
    if not os.path.isdir(os.path.join(fpk_bundle, "cmd")):
        sys.exit("[ERROR] 缺少 fpk-bundle/cmd，请先跑 python tools/build_fpk.py")

    print("=" * 62)
    print(" agnes-hub 飞牛手工部署（前台联调模式）")
    print("=" * 62)

    print("\n[1/5] 连接 %s ..." % args.host)
    nas = Nas(args.host, args.user, password)
    print("      ✓ 已连接")

    print("\n[2/5] 准备目录与用户 ...")
    nas.sudo("id %s >/dev/null 2>&1 || useradd -r -s /bin/false %s" % (APP_ID, APP_ID))
    nas.sudo("mkdir -p %s/app %s/cmd %s/config %s/state %s" %
             (APP_DIR, APP_DIR, APP_DIR, APP_DIR, DATA_DIR))
    print("      ✓ 目录就绪")

    print("\n[3/5] 上传二进制与脚本 ...")
    for remote_name, local in binaries.items():
        nas.put(local, "/tmp/%s" % remote_name)
        nas.sudo("cp /tmp/%s %s/app/%s && chmod +x %s/app/%s" %
                 (remote_name, APP_DIR, remote_name, APP_DIR, remote_name))
    for name in CMD_NAMES:
        local = os.path.join(fpk_bundle, "cmd", name)
        if not os.path.exists(local):
            continue
        nas.put(local, "/tmp/_cmd_%s" % name)
        nas.sudo("cp /tmp/_cmd_%s %s/cmd/%s && chmod +x %s/cmd/%s" %
                 (name, APP_DIR, name, APP_DIR, name))
    for name in ("privilege", "resource"):
        local = os.path.join(fpk_bundle, "config", name)
        if os.path.exists(local):
            nas.put(local, "/tmp/_cfg_%s" % name)
            nas.sudo("cp /tmp/_cfg_%s %s/config/" % (name, APP_DIR))
    nas.put(os.path.join(fpk_bundle, "manifest"), "/tmp/_manifest")
    nas.sudo("cp /tmp/_manifest %s/" % APP_DIR)
    nas.sudo("chown -R %s:%s %s %s" % (APP_ID, APP_ID, APP_DIR, DATA_DIR))
    print("      ✓ 上传完成")

    print("\n[4/5] 校验二进制格式 ...")
    print("      " + nas.sudo("file %s/app/agnes-hub-go" % APP_DIR).strip())
    print("      " + nas.sudo(
        "grep -aq '1.0.0-go' %s/app/agnes-hub-go && echo '版本串 1.0.0-go 已嵌入' "
        "|| echo '警告：未找到版本串'" % APP_DIR).strip())

    print("\n[5/5] 前台启动联调（10 秒后自动结束）")
    print("      " + "-" * 56)
    # 前台运行：后台会被沙箱拦截，所以这里就是唯一能验证的方式
    chan = nas.client.get_transport().open_session()
    chan.get_pty(width=200, height=50)
    chan.set_combine_stderr(True)
    chan.exec_command("%s/app/agnes-hub-go -host 0.0.0.0 -port %s -data %s" %
                      (APP_DIR, args.port, DATA_DIR))
    banner, start = "", time.time()
    while not chan.exit_status_ready() and time.time() - start < 10:
        if chan.recv_ready():
            chunk = chan.recv(4096).decode("utf-8", errors="replace")
            banner += chunk
            if "已启动" in chunk:
                break
        time.sleep(0.1)
    for line in banner.splitlines():
        if "Could not chdir" not in line:
            print("      " + line)

    time.sleep(0.6)
    print("\n      健康检查：")
    print("      " + nas.sudo("curl -s http://localhost:%s/healthz || echo 'FAILED'" % args.port).strip())
    print("      鉴权检查（应返回缺少 API Key 的结构化错误）：")
    print("      " + nas.sudo(
        "curl -s -X POST http://localhost:%s/v1/chat/completions "
        "-H 'Content-Type: application/json' -d '{\"model\":\"x\",\"messages\":[]}'"
        % args.port).strip()[:160])
    chan.close()

    nas.close()
    print("\n" + "=" * 62)
    print(" 前台联调结束。要常驻请用应用中心安装 .fpk：")
    print("   python tools/build_fpk.py")
    print("   sudo appcenter-cli install-fpk dist/agnes-hub-go-1.0.0.fpk")
    print("=" * 62)


if __name__ == "__main__":
    main()
