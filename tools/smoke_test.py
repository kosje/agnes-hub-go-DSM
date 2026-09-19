"""agnes-hub-go 本地冒烟测试（不消耗任何上游配额）。

只验证「进程能起来 + 控制台可用 + 鉴权生效 + agnes-auto 判定走通」，
刻意不调用真实上游：所有判定端点都是干跑，probe 端点不碰。

进程管理交给 Python 而不是 shell，避免 `cd ... && exe ... &` 把整条命令链
一起后台化、导致 kill 打偏、残留进程霸占端口的坑。
"""

import http.cookiejar
import json
import os
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE = os.path.join(ROOT, "agnes-hub-go.exe")
if not os.path.exists(EXE):
    EXE = os.path.join(ROOT, "agnes-hub-go")  # 非 Windows 平台
DATA = os.path.join(ROOT, ".smoke")
LOG = os.path.join(ROOT, ".smoke.log")
PORT = 4199
BASE = f"http://127.0.0.1:{PORT}"

failures = []


def check(name, ok, detail=""):
    print(("  PASS  " if ok else "  FAIL  ") + name + (f"   {detail}" if detail else ""))
    if not ok:
        failures.append(name + (f" ({detail})" if detail else ""))


def port_listening(port):
    with socket.socket() as s:
        s.settimeout(0.5)
        return s.connect_ex(("127.0.0.1", port)) == 0


def request(opener, method, path, payload=None, bearer=None, raw=False):
    data = None
    headers = {}
    if payload is not None:
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        headers["Content-Type"] = "application/json"
    if bearer:
        headers["Authorization"] = "Bearer " + bearer
    req = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    try:
        with opener.open(req, timeout=10) as resp:
            body = resp.read()
            return resp.status, dict(resp.headers), (body if raw else json.loads(body or b"{}"))
    except urllib.error.HTTPError as e:
        body = e.read()
        try:
            parsed = json.loads(body or b"{}")
        except Exception:
            parsed = {"raw": body.decode("utf-8", "replace")}
        return e.code, dict(e.headers), parsed


shutil.rmtree(DATA, ignore_errors=True)
if os.path.exists(LOG):
    os.remove(LOG)

print("=" * 74)
print("启动 agnes-hub-go ...")
logf = open(LOG, "wb")
proc = subprocess.Popen(
    [EXE, "-host", "127.0.0.1", "-port", str(PORT), "-data", DATA],
    stdout=logf,
    stderr=subprocess.STDOUT,
    cwd=ROOT,
)
try:
    jar = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))

    # ---- 等待就绪 ----
    ready = False
    deadline = time.time() + 15
    while time.time() < deadline:
        try:
            status, _, body = request(opener, "GET", "/healthz")
            if status == 200 and body.get("ok"):
                ready = True
                break
        except Exception:
            pass
        time.sleep(0.3)

    print("-" * 74)
    print("启动横幅：")
    logf.flush()
    print(open(LOG, "r", encoding="utf-8", errors="replace").read().rstrip())
    print("-" * 74)

    check("进程已就绪，/healthz 返回 ok", ready)
    if not ready:
        raise SystemExit(1)

    # ---- 控制台鉴权 ----
    status, _, body = request(opener, "POST", "/api/login", {"password": "wrong"})
    check("错误密码被拒绝", status == 401, f"http={status}")

    status, _, body = request(opener, "POST", "/api/login", {"password": "admin123"})
    check("默认口令 admin123 可登录", status == 200 and body.get("ok"), f"http={status}")

    status, _, body = request(opener, "GET", "/api/session")
    check("Cookie 会话生效", body.get("logged_in") is True, f"body={body}")

    # ---- 未授权访问受保护的接口 ----
    anon = urllib.request.build_opener()
    status, _, _ = request(anon, "GET", "/api/accounts")
    check("未登录访问 /api/accounts 被拒", status == 401, f"http={status}")

    # ---- 签发下游密钥 ----
    status, _, body = request(opener, "POST", "/api/keys",
                              {"name": "smoke", "classes": ["*"]})
    raw_key = body.get("key")
    key = raw_key.get("key") if isinstance(raw_key, dict) else raw_key
    check("可签发下游密钥", bool(key), f"key={str(key)[:14]}...")

    # ---- 无 key 调用被拒（这是网关的第一道门） ----
    status, _, body = request(anon, "GET", "/v1/models")
    check("无 key 调用 /v1/models 被拒", status == 401, f"http={status}")

    status, _, body = request(anon, "GET", "/v1/models", bearer=key)
    models = [m.get("id") for m in (body.get("data") or [])]
    check("带 key 可列出模型目录", status == 200 and len(models) > 0, f"共 {len(models)} 个")
    check("agnes-auto 在模型目录中", "agnes-auto" in models)

    # ---- 账号池：按模态分工（不触发任何上游调用） ----
    status, _, body = request(opener, "POST", "/api/accounts", {
        "name": "文本账号",
        "api_key": "sk-fake-text-only",
        "access_type": "free",
        "model_manifest": {"text": ["agnes-2.5-flash"], "image": [], "video": []},
    })
    text_id = body.get("id")
    check("可新增只支持文本的账号", status == 200 and bool(text_id), f"id={text_id}")

    status, _, body = request(opener, "GET", "/api/accounts")
    items = body.get("accounts") or []
    one = next((a for a in items if a.get("id") == text_id), None)
    check("账号清单里 image 清单保持为空（按模态分工不被默认值还原）",
          one is not None and one.get("model_manifest", {}).get("image") == [],
          f"manifest={one.get('model_manifest') if one else None}")
    if one:
        check("文本池有效 RPM = 基线 20 × 安全系数 0.9",
              abs(one.get("rpm_effective", {}).get("text", 0) - 18.0) < 0.01,
              f"effective={one.get('rpm_effective', {}).get('text')}")
        check("未声明该模态的池不出现在声明清单里",
              one.get("declared", {}).get("video") == [],
              f"declared.video={one.get('declared', {}).get('video')}")

    # ---- agnes-auto 判定干跑（零配额，批量模式） ----
    cases = [
        ("帮我画一张赛博朋克风格的城市夜景", "image"),
        ("生成一段视频里的关键帧图片", "image"),
        ("生成一段海边日落的海浪视频", "video"),
        ("解释一下 Transformer 的注意力机制", "text"),
        ("视频压缩工具有哪些推荐", "text"),
    ]
    status, _, body = request(opener, "POST", "/api/intent/preview",
                              {"_samples": [c[0] for c in cases]})
    got_by_input = {c.get("input"): c for c in (body.get("cases") or [])}
    for prompt, expect in cases:
        got = (got_by_input.get(prompt) or {}).get("intent")
        detail = json.dumps(got_by_input.get(prompt) or {}, ensure_ascii=False)[:150]
        check(f"干跑判定「{prompt}」→ {expect}", got == expect, f"实际 {got} / {detail}")

    # ---- 网关侧同一判定（零配额干跑） ----
    # 注意：/v1/intent/preview 只回 JSON，不设 X-Agnes-Hub-Intent 响应头 ——
    # 那个头是真实转发路径（/v1/chat/completions 等）才写的，由 Go 端 e2e 测试覆盖。
    status, _, body = request(anon, "POST", "/v1/intent/preview",
                              {"model": "agnes-auto",
                               "messages": [{"role": "user", "content": "帮我画一只猫"}]},
                              bearer=key)
    check("网关干跑端点判出 image（带 key）",
          status == 200 and body.get("intent") == "image",
          f"http={status} intent={body.get('intent')} by={body.get('intent_by')}")

    # ---- 无可用账号时应给出明确错误，而不是硬闯上游 ----
    status, _, body = request(anon, "POST", "/v1/chat/completions",
                              {"model": "agnes-video-2.5-flash",
                               "messages": [{"role": "user", "content": "海边日落"}]},
                              bearer=key)
    check("视频池无可用账号时返回 503 no_capacity 而不是硬闯上游",
          status == 503 and (body.get("error") or {}).get("code") == "no_capacity",
          f"http={status} body={json.dumps(body, ensure_ascii=False)[:160]}")

finally:
    logf.close()
    print("-" * 74)
    print("停止进程 ...")
    # Windows 上 Popen.terminate() 是 TerminateProcess，退出码 1 属正常，
    # 真正要断言的是「端口释放、进程消失」——避免残留进程霸占端口。
    proc.terminate()
    try:
        proc.wait(timeout=12)
        print(f"  已结束，退出码 {proc.returncode}")
    except subprocess.TimeoutExpired:
        print("  结束超时，强制 kill")
        proc.kill()
        proc.wait(timeout=10)

    for _ in range(20):
        if not port_listening(PORT):
            break
        time.sleep(0.3)
    check("端口已释放，无残留进程", not port_listening(PORT))
    check("进程已彻底结束", proc.poll() is not None)

    shutil.rmtree(DATA, ignore_errors=True)
    if os.path.exists(LOG):
        os.remove(LOG)

print("=" * 74)
if failures:
    print(f"结果: FAIL（{len(failures)} 项）")
    for f in failures:
        print(" -", f)
    sys.exit(1)
print("结果: ALL PASS")
