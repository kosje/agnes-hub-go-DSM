"""在临时目录里实测 agnes-hub-go.bat 的语法与分支走向。

不真正启动服务：只把 exe 调用那一行换成打印桩，其余段落原样保留，
这样连 :RUN 段的提示文案也一起被覆盖到。

PowerShell 工具禁止调用 cmd.exe，Git Bash 的 cmd //c 会被 MSYS 路径转换破坏，
因此按既有约定用 Python subprocess 起 cmd.exe。
"""

import os
import shutil
import socket
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BAT = os.path.join(ROOT, "agnes-hub-go.bat")
BAT_SRC = os.path.join(os.path.dirname(os.path.abspath(__file__)), "bat_src.utf8")
BIN_NAME = "agnes-hub-go.exe"
EXE_LINE = '"%BIN%" -host %HOST% -port %PORT% -data "%DATADIR%"'
STUB_LINE = "echo [stub] skipped-exe host=%HOST% port=%PORT% datadir=%DATADIR%"

with open(BAT, "r", encoding="gbk") as f:
    full = f.read()

if EXE_LINE not in full:
    print("FAIL: 未找到 exe 调用行，bat 可能已被改动")
    sys.exit(1)

# 整份 bat 都用上，只把真正启动服务的那一行替换掉
body = full.replace(EXE_LINE, STUB_LINE)
# pause 一律注释掉，避免测试在等待按键上白等
body = body.replace("pause", "rem pause")

failures = []


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def listen_on(port):
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", port))
    s.listen(1)
    return s


def run_case(name, make_exe, port, extra_prelude="", expect=(), forbid=()):
    tmp = tempfile.mkdtemp(prefix="agnesbat_")
    try:
        text = body.replace('set "PORT=4142"', f'set "PORT={port}"', 1)
        if extra_prelude:
            text = text.replace('cd /d "%~dp0"', 'cd /d "%~dp0"\r\n' + extra_prelude, 1)
        bat = os.path.join(tmp, "t.bat")
        with open(bat, "w", encoding="gbk", newline="\r\n") as f:
            f.write(text)
        if make_exe:
            with open(os.path.join(tmp, BIN_NAME), "wb") as f:
                f.write(b"stub")

        proc = subprocess.run(
            ["cmd.exe", "/c", bat],
            capture_output=True,
            stdin=subprocess.DEVNULL,
            cwd=tmp,
            timeout=90,
        )
        out = (proc.stdout + proc.stderr).decode("gbk", errors="replace")
        print("=" * 72)
        print(f"[{name}] port={port} exit={proc.returncode}")
        print(out.rstrip())
        print("=" * 72)

        if "not recognized" in out or "不是内部或外部命令" in out:
            failures.append(f"{name}: 出现未识别命令 —— bat 编码或语法有问题")
        if "用法: netstat" in out or "Usage: netstat" in out:
            failures.append(f"{name}: netstat 收到了字面量管道符（bat 里不应写 ^|）")
        for token in expect:
            if token not in out:
                failures.append(f"{name}: 缺少预期输出 {token!r}")
        for token in forbid:
            if token in out:
                failures.append(f"{name}: 出现了不该有的输出 {token!r}")
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    return out


# 用例 1：exe 已存在、端口空闲 → 直接走 RUN 分支，且参数拼装正确
p1 = free_port()
run_case(
    "已编译-直达RUN",
    make_exe=True,
    port=p1,
    expect=[
        f"skipped-exe host=127.0.0.1 port={p1}",
        "关闭本窗口即停止服务",
        "统一模型  agnes-auto",
        f"http://127.0.0.1:{p1}/console",
        "已退出，退出码 0",
    ],
    forbid=["[错误]", "[警告]", "尝试用本机 Go 工具链编译"],
)

# 用例 2：无 exe 且无 go → 走 NOGO 分支并给出可操作的提示
run_case(
    "无exe无go-NOGO",
    make_exe=False,
    port=free_port(),
    extra_prelude='set "PATH=C:\\Windows\\system32;C:\\Windows"',
    expect=["[错误]", "请二选一", "重新双击本文件"],
    forbid=["skipped-exe", "[警告]"],
)

# 用例 3：目标端口已被占用 → 走 PORTBUSY 分支，且绝不启动 exe
# 真实场景：Python 版 agnes-hub 默认也用 4142，两者会直接撞车。
p3 = free_port()
holder = listen_on(p3)
try:
    run_case(
        "端口被占用-PORTBUSY",
        make_exe=True,
        port=p3,
        expect=["[警告]", f"端口 {p3} 已被占用", "处理方式二选一",
                f'set "PORT={p3}"', "LISTENING"],
        forbid=["skipped-exe"],
    )
finally:
    holder.close()

print()
if failures:
    print("结果: FAIL")
    for item in failures:
        print(" -", item)
    sys.exit(1)
print("结果: ALL PASS —— bat 编码、分支与参数拼装均正常")
if not os.path.exists(BAT_SRC):
    print("注意: 未找到 tools/bat_src.utf8，后续无法用 make_bat.py 重新生成 bat")
