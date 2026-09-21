# -*- coding: utf-8 -*-
"""v1.0.5 hotfix 部署：已装应用手工升级（停服 + sudo cp 覆盖 + appcenter start）

背景：1.0.5 已通过 install-fpk 装上飞牛，但 install-fpk 对已装应用不覆盖二进制/脚本，
本次只补 console.html 的语法修复，走手工升级流程：
  1. 数据备份
  2. appcenter-cli stop + pkill 停服
  3. 上传新二进制（amd64/arm64）+ 更新 fpk 到 /tmp
  4. sudo cp 覆盖到 /var/apps/<appid>/（fnOS 会把 app.tgz 内容套一层 <app_id>/）
  5. appcenter-cli start
  6. /healthz + /console 验证

凭据：NAS_PASS 环境变量 或 D:/work/M365-fpk/.nas_pass
"""
import os
import sys
import time

try:
    import paramiko
except ImportError:
    sys.exit("[ERROR] 需要 paramiko")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
APP_ID = "agnes-hub"
FALLBACK_PASS = "D:/work/M365-fpk/.nas_pass"


def load_password():
    pw = os.environ.get("NAS_PASS")
    if pw:
        return pw.strip()
    if os.path.exists(FALLBACK_PASS):
        with open(FALLBACK_PASS, encoding="utf-8") as f:
            return f.read().strip()
    sys.exit("[ERROR] 未找到口令：设 NAS_PASS 或写入 %s" % FALLBACK_PASS)


class Nas:
    def __init__(self, host, user, password, timeout=15):
        self.password = password
        self.client = paramiko.SSHClient()
        self.client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
        self.client.connect(host, username=user, password=password, timeout=timeout)

    def sudo(self, cmd, timeout=120):
        chan = self.client.get_transport().open_session()
        chan.get_pty(width=200, height=50)
        chan.set_combine_stderr(True)
        chan.exec_command("echo '%s' | sudo -S %s" % (self.password, cmd))
        out, start = "", time.time()
        while True:
            if chan.recv_ready():
                out += chan.recv(4096).decode("utf-8", errors="replace")
            if chan.exit_status_ready():
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
    import argparse
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="nas.lianu.com")
    ap.add_argument("--user", default="admin")
    ap.add_argument("--port", default="4142")
    args = ap.parse_args()

    bin_amd64 = os.path.join(ROOT, "dist", "agnes-hub-go-linux-amd64")
    bin_arm64 = os.path.join(ROOT, "dist", "agnes-hub-go-linux-arm64")
    fpk = os.path.join(ROOT, "dist", "agnes-hub-go-1.0.5.fpk")
    for f in (bin_amd64, bin_arm64, fpk):
        if not os.path.exists(f):
            sys.exit("[ERROR] 缺少 %s" % f)

    pw = load_password()
    print("=" * 62)
    print(" agnes-hub 1.0.5 hotfix 手工升级（console.html 修复）")
    print(" host=%s user=%s port=%s" % (args.host, args.user, args.port))
    print("=" * 62)

    print("\n[1/6] 连接 ...")
    nas = Nas(args.host, args.user, pw)
    print("      ok")

    print("\n[2/6] 定位应用目录与当前二进制 ...")
    loc = nas.sudo("ls -d /var/apps/%s 2>/dev/null; "
                   "for v in /vol1/@appcenter /vol1/@appdata /vol2/@appcenter; do ls -d $v/%s 2>/dev/null; done" % (APP_ID, APP_ID))
    show("应用目录：", loc)
    ver = nas.sudo("for f in /var/apps/%s/agnes-hub-go* /vol1/@appcenter/%s/agneshub-go/agneshub-go; do "
                   "[ -f \"$f\" ] && echo \"$f\" && grep -ao '1\\.0\\.[0-9]*' \"$f\" | head -1; done" % (APP_ID, APP_ID))
    show("当前二进制/版本串：", ver)

    print("\n[3/6] 数据备份 ...")
    bak = nas.sudo("B=/vol1/@appdata/%s-data.PRE105fix$(date +%%m%%d%%H%%M); "
                   "cp -a /vol1/@appdata/%s $B && echo 已备份到 $B && du -sh $B" % (APP_ID, APP_ID))
    show("备份：", bak)

    print("\n[4/6] 停服 ...")
    stop = nas.sudo("appcenter-cli stop %s; pkill -9 -f agnes-hub-go; sleep 1; pgrep -af agnes-hub-go || echo 已全部停止" % APP_ID)
    show("停服：", stop)

    print("\n[5/6] 上传并覆盖 ...")
    nas.put(bin_amd64, "/tmp/agnes-hub-go-linux-amd64")
    nas.put(bin_arm64, "/tmp/agnes-hub-go-linux-arm64")
    nas.put(fpk, "/tmp/agnes-hub-go-1.0.5.fpk")
    print("      上传完成（/tmp 下 3 个文件）")
    cp = nas.sudo("UNAME_M=$(uname -m); echo 架构=$UNAME_M; "
                  "SRC=/tmp/agnes-hub-go-linux-amd64; [ \"$UNAME_M\" = \"aarch64\" ] && SRC=/tmp/agnes-hub-go-linux-arm64; "
                  "D=/var/apps/%s; [ -d $D ] || D=$(ls -d /vol1/@appcenter/%s 2>/dev/null); "
                  "echo 目标目录=$D; ls -la $D | head; "
                  "for c in $D/agneshub-go $D/app/agneshub-go $D/bin/agneshub-go; do [ -f $c ] && echo 候选=$c; done" % (APP_ID, APP_ID))
    show("目录探测：", cp)
    cp2 = nas.sudo("UNAME_M=$(uname -m); SRC=/tmp/agnes-hub-go-linux-amd64; [ \"$UNAME_M\" = \"aarch64\" ] && SRC=/tmp/agnes-hub-go-linux-arm64; "
                   "D=/var/apps/%s; BINF=$(for c in $D/agneshub-go $D/app/agneshub-go $D/bin/agneshub-go; do [ -f $c ] && echo $c && break; done); "
                   "echo 覆盖 $BINF; cp -fv $SRC $BINF && chmod +x $BINF && grep -ao '1\\.0\\.5' $BINF | head -1" % APP_ID)
    show("覆盖：", cp2)
    # 顺带更新 fpk 包（供下次 install-fpk / 桌面快捷方式引用）
    cp3 = nas.sudo("for v in /vol1/@appcenter/%s; do F=$v/agnes-hub-go-1.0.5.fpk; [ -f $F ] && cp -fv /tmp/agnes-hub-go-1.0.5.fpk $F; done; "
                   "find /vol1/@appcenter/%s -name '*.fpk' 2>/dev/null | head" % (APP_ID, APP_ID))
    show("fpk 更新：", cp3)

    print("\n[6/6] 启动并验证 ...")
    st = nas.sudo("appcenter-cli start %s" % APP_ID)
    show("启动：", st)
    time.sleep(3)
    hz = nas.sudo("curl -s http://127.0.0.1:%s/healthz" % args.port)
    show("/healthz：", hz)
    con = nas.sudo("curl -s -o /dev/null -w 'console HTTP=%{http_code} size=%{size_download}' http://127.0.0.1:%s/console" % args.port)
    show("/console：", con)
    # 校验二进制内嵌 console.html 的 banner 三元是否完整（修复后的特征串）
    js = nas.sudo("BIN=$(for c in /var/apps/%s/agneshub-go /var/apps/%s/app/agneshub-go; do [ -f $c ] && echo $c && break; done); "
                  "grep -ac 'const banner = uls === 0' $BIN; grep -ac '到达密度 vs 文本池节拍' $BIN" % (APP_ID, APP_ID))
    show("内嵌特征（banner=ul s===0 计数 / 到达密度卡计数）：", js)

    nas.close()
    print("\n完成。请浏览器验证 http://%s:%s/console（建议先 Ctrl+Shift+R 强刷）" % (args.host, args.port))


if __name__ == "__main__":
    main()
