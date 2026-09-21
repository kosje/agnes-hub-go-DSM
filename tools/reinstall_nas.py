#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""baipiao-hub/agnes-hub 飞牛 NAS 干净重装：备份 -> 卸载 -> install-fpk(1.0.11) -> 恢复数据 -> 验证。

前提：fpk 已用正确 appname=agnes-hub、清理过 {} / → 字符、display_name=白嫖 Hub。
安全：先完整备份 @appdata/agnes-hub（账号/设置/对话），并把当前 app 目录也备份以便回滚。
"""
import os, sys, tempfile, time
try:
    import paramiko
except ImportError:
    sys.exit("[ERROR] 需要 paramiko")
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FALLBACK_PASS = "D:/work/M365-fpk/.nas_pass"

REINSTALL_SH = r"""#!/bin/sh
APP_ID=agnes-hub
VER=1.0.11
PORT=4142
S=$(echo $VER | tr -d .)
BACKUP=/vol1/@appdata/${APP_ID}-data.PRE${S}.bak
SAFE=/vol1/baipiao_backup_PRE${S}      # 中立位置，卸载不会被删
APPBACKUP=/vol1/@appdata/${APP_ID}-app.PRE${S}.bak

echo "=== 1) 备份数据(账号/设置/对话) ==="
rm -rf "$BACKUP"; cp -a /vol1/@appdata/$APP_ID "$BACKUP"
rm -rf "$SAFE"; cp -a "$BACKUP" "$SAFE"
echo "   数据备份: $BACKUP  (中立副本: $SAFE)"
AC=$(grep -o '\"id\"' "$SAFE/data/accounts.json" 2>/dev/null | wc -l)
echo "   账号数: $AC"
if [ "$AC" -lt 1 ]; then echo "ABORT: 备份账号数为0，中止以免丢失数据"; exit 9; fi
echo "   备份校验通过"

echo "=== 2) 备份当前 app 目录(回滚用) ==="
rm -rf "$APPBACKUP"; mkdir -p "$APPBACKUP"
cp -a /vol1/@appcenter/$APP_ID "$APPBACKUP/appcenter" 2>/dev/null || echo "   appcenter 备份跳过"
cp -a /var/apps/$APP_ID "$APPBACKUP/varapps" 2>/dev/null || echo "   varapps 备份跳过"
echo "   app 备份完成: $APPBACKUP"

echo "=== 3) 停服 ==="
appcenter-cli stop $APP_ID || true
pkill -9 -f baipiao-hub || true
sleep 2
echo "   残留进程: $(pgrep -af baipiao-hub || echo NONE)"

echo "=== 4) 卸载 ==="
appcenter-cli uninstall $APP_ID
sleep 2
echo "   卸载后列表:"; appcenter-cli list 2>/dev/null | grep -i agnes || echo "   agnes-hub 已移除"
echo "   卸载后中立备份是否存活: $([ -d "$SAFE" ] && echo YES || echo NO)"

echo "=== 5) install-fpk 1.0.11 ==="
appcenter-cli install-fpk /tmp/baipiao-hub-$VER.fpk
sleep 3
echo "   安装后列表:"; appcenter-cli list 2>/dev/null | grep -i agnes
if ! appcenter-cli list 2>/dev/null | grep -qi "$APP_ID"; then
  echo "ABORT: install-fpk 未成功安装 $APP_ID（数据已安全备份于 $SAFE）"; exit 7
fi

echo "=== 6) 启动并验证版本 ==="
appcenter-cli start $APP_ID
sleep 4
echo "   healthz: $(curl -s http://127.0.0.1:$PORT/healthz)"

echo "=== 7) 恢复数据 ==="
mkdir -p /vol1/@appdata/$APP_ID/data
cp -a "$SAFE/data/." /vol1/@appdata/$APP_ID/data/ 2>/dev/null
echo "   恢复后 accounts.json 账号数: $(grep -o '\"id\"' /vol1/@appdata/$APP_ID/data/accounts.json 2>/dev/null | wc -l)"

echo "=== 8) 重启加载数据并终检 ==="
appcenter-cli stop $APP_ID || true; sleep 1; appcenter-cli start $APP_ID; sleep 4
echo "   healthz: $(curl -s http://127.0.0.1:$PORT/healthz)"
curl -s -o /dev/null -w "   console HTTP=%{http_code} size=%{size_download}\n" http://127.0.0.1:$PORT/console
echo "   内嵌 editModal=$(grep -ac editAccountModal /vol1/@appcenter/$APP_ID/app/baipiao-hub 2>/dev/null)"
echo "   内嵌 rpmOverride=$(grep -ac rpm_overrides /vol1/@appcenter/$APP_ID/app/baipiao-hub 2>/dev/null)"
"""

def load_password():
    pw = os.environ.get("NAS_PASS")
    if pw: return pw.strip()
    if os.path.exists(FALLBACK_PASS):
        with open(FALLBACK_PASS, encoding="utf-8") as f: return f.read().strip()
    sys.exit("[ERROR] 未找到口令")

class Nas:
    def __init__(self, host, user, password, port, timeout=45):
        self.password = password
        self.client = paramiko.SSHClient()
        self.client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
        self.client.connect(host, username=user, password=password, port=port, timeout=timeout, look_for_keys=False, allow_agent=False)
    def sudo(self, cmd, timeout=240):
        # 注意：sudo 必须用「非 PTY」方式（echo PW | sudo -S cmd），
        # 带 PTY 时 fnOS 的 sudo 会出现密码读错（"3 incorrect password attempts"）。
        chan = self.client.get_transport().open_session()
        chan.set_combine_stderr(True)
        chan.exec_command("echo '%s' | sudo -S %s" % (self.password, cmd))
        out, start = "", time.time()
        while True:
            if chan.recv_ready(): out += chan.recv(4096).decode("utf-8","replace")
            if chan.exit_status_ready():
                while chan.recv_ready(): out += chan.recv(4096).decode("utf-8","replace")
                break
            if time.time()-start > timeout: out += "\n[TIMEOUT]"; break
            time.sleep(0.05)
        try: chan.close()
        except Exception: pass
        return "\n".join(l for l in out.splitlines() if "Could not chdir to home" not in l and "[sudo] password" not in l)
    def put(self, local, remote):
        sftp = self.client.open_sftp()
        try: sftp.put(local, remote)
        finally: sftp.close()
    def close(self): self.client.close()

def main():
    host = sys.argv[1] if len(sys.argv) > 1 else "nas.lianu.com"
    nas = Nas(host, "admin", load_password(), 22)
    # 上传 fpk 与脚本
    nas.put(os.path.join(ROOT, "dist", "baipiao-hub-1.0.11.fpk"), "/tmp/baipiao-hub-1.0.11.fpk")
    with tempfile.NamedTemporaryFile("w", suffix=".sh", delete=False, encoding="utf-8", newline="\n") as tf:
        tf.write(REINSTALL_SH); local_sh = tf.name
    nas.put(local_sh, "/tmp/baipiao_reinstall.sh")
    print(nas.sudo("bash /tmp/baipiao_reinstall.sh", timeout=240))
    nas.close()

if __name__ == "__main__":
    main()
