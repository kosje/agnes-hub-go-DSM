#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""agnes-hub-go 多账号吞吐探针（零配额）。

回答一个问题：多账号轮询能不能让「一个任务」变快、快到翻倍？

方法：所有上游请求打到本地 mock，该 mock 复刻 Agnes 的真实限流语义
（同一个 API Key 的相邻请求间隔小于 MIN_INTERVAL 即返回 429）。
然后把「批量任务」（M 个互相独立的请求）交给真实 hub 进程发出去，
测端到端墙钟耗时，对比不同账号数 / 不同客户端发送模式。

对照组（防止自欺欺人）：先以超过 MIN_INTERVAL 的速率直连 mock，
确认 mock 真的会限流，再开始测 hub。

实验矩阵：
  A 串行(等返回)  exec=0.3s   -> 1 / 2 账号
  B 全并发        exec=0.3s   -> 1 / 2 / 3 账号
  C 串行(等返回)  exec=4.0s   -> 1 / 2 账号      <- 真实文本对话形态
  D 全并发        exec=4.0s   -> 1 / 2 账号      <- 真实批量任务形态
"""

import json
import os
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE = os.path.join(ROOT, "agnes-hub-go.exe")
if not os.path.exists(EXE):
    EXE = os.path.join(ROOT, "agnes-hub-go")

HUB_PORT = 4188
MOCK_PORT = 4189
HUB_BASE = "http://127.0.0.1:%d" % HUB_PORT
MOCK_BASE = "http://127.0.0.1:%d/v1" % MOCK_PORT
DOWN_KEY = "sk-probe-downstream"

# 上游最小安全间隔。略小于 hub 的节拍（60/(20*0.9)=3.333s），
# 这样正常节流下不会 429，429 只在「客户端绕过节拍」时才出现。
MIN_INTERVAL = 2.8
M = 6          # 每个批量任务的请求数
TMP = os.path.join(ROOT, ".probe")


# ---------------------------------------------------------------------------
# mock 上游
# ---------------------------------------------------------------------------
class Upstream:
    def __init__(self):
        self.lock = threading.Lock()
        self.last = {}
        self.hits = 0
        self.limited = 0
        self.exec_sec = 0.3

    def note(self, key):
        with self.lock:
            now = time.time()
            prev = self.last.get(key, 0.0)
            self.last[key] = now
            self.hits += 1
            if prev and (now - prev) < MIN_INTERVAL:
                self.limited += 1
                return False
            return True

    def reset(self):
        with self.lock:
            self.last = {}
            self.hits = 0
            self.limited = 0


UP = Upstream()


class QuietServer(ThreadingHTTPServer):
    """抑制「客户端提前断开」产生的 ConnectionResetError 噪声。

    hub 终止时会强关上游连接，socketserver 默认会把每个这种断开打成一条
    完整 traceback，足以淹没实验结果。这里直接吞掉：断开是预期行为，不是故障。
    """

    daemon_threads = True

    def handle_error(self, request, client_address):
        pass


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _send(self, code, obj):
        body = json.dumps(obj).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.startswith("/__set_exec"):
            sec = float(self.path.split("=")[1])
            UP.exec_sec = sec
            self._send(200, {"ok": True, "exec_sec": sec})
        elif self.path.startswith("/__reset"):
            UP.reset()
            self._send(200, {"ok": True})
        elif self.path.startswith("/__stats"):
            with UP.lock:
                self._send(200, {"hits": UP.hits, "limited": UP.limited})
        else:
            self._send(200, {})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0) or 0)
        if n:
            self.rfile.read(n)
        auth = self.headers.get("Authorization", "")
        key = auth.split(" ", 1)[1] if " " in auth else auth
        if not UP.note(key):
            self._send(429, {"error": {"message": "rate limited", "type": "rate_limit_error"}})
            return
        time.sleep(UP.exec_sec)
        self._send(200, {
            "id": "probe", "object": "chat.completion",
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}}],
        })


# ---------------------------------------------------------------------------
# hub 生命周期
# ---------------------------------------------------------------------------
def port_busy(p):
    with socket.socket() as s:
        s.settimeout(0.3)
        return s.connect_ex(("127.0.0.1", p)) == 0


def write_data(d, n_accounts, affinity):
    shutil.rmtree(d, ignore_errors=True)
    os.makedirs(d, exist_ok=True)

    accs = []
    for i in range(n_accounts):
        accs.append({
            "id": "acc_probe%d" % i,
            "name": "probe%d" % i,
            "api_key": "sk-mock-key-%d" % i,          # 每账号独立 key -> mock 侧独立配额
            "base_url": MOCK_BASE,
            "access_type": "free",
            "enabled": True,
            "classes_enabled": ["*"],
            "model_manifest": {
                "text": ["agnes-2.5-flash"],
                "image": ["agnes-image-2.5-flash"],
                "video": ["agnes-video-2.5"],
            },
            "rpm_overrides": {},
            "max_concurrency": 8,
            "learned_factor": 1,
            "pool_factors": {},
            "stats": {},
        })
    with open(os.path.join(d, "accounts.json"), "w", encoding="utf-8") as f:
        json.dump(accs, f, ensure_ascii=False)

    src = os.path.join(ROOT, "data", "settings.json")
    with open(src, encoding="utf-8") as f:
        settings = json.load(f)
    settings["affinity_mode"] = affinity
    settings["queue_max_wait_ms"] = 120000
    with open(os.path.join(d, "settings.json"), "w", encoding="utf-8") as f:
        json.dump(settings, f, ensure_ascii=False, indent=2)

    with open(os.path.join(d, "downstream_keys.json"), "w", encoding="utf-8") as f:
        json.dump([{
            "key": DOWN_KEY, "name": "probe", "enabled": True, "classes": ["*"],
            "daily_quota": 0, "total_quota": 0, "used_total": 0,
            "used_today": 0, "used_date": "", "created_at": time.time(),
        }], f, ensure_ascii=False)


def start_hub(data_dir, log_path):
    logf = open(log_path, "wb")
    proc = subprocess.Popen(
        [EXE, "-host", "127.0.0.1", "-port", str(HUB_PORT), "-data", data_dir],
        stdout=logf, stderr=subprocess.STDOUT, cwd=ROOT,
    )
    for _ in range(100):
        if port_busy(HUB_PORT):
            time.sleep(0.4)
            return proc
        if proc.poll() is not None:
            raise RuntimeError("hub 启动失败，看 %s" % log_path)
        time.sleep(0.1)
    raise RuntimeError("hub 端口未就绪")


def stop_hub(proc):
    proc.terminate()
    try:
        proc.wait(timeout=8)
    except subprocess.TimeoutExpired:
        proc.kill()
    for _ in range(40):
        if not port_busy(HUB_PORT):
            return
        time.sleep(0.1)


# ---------------------------------------------------------------------------
# 发请求
# ---------------------------------------------------------------------------
def one_request(i):
    payload = json.dumps({
        "model": "agnes-2.5-flash",
        "messages": [{"role": "user", "content": "probe %d" % i}],
    }).encode()
    req = urllib.request.Request(
        HUB_BASE + "/v1/chat/completions", data=payload,
        headers={"Content-Type": "application/json",
                 "Authorization": "Bearer " + DOWN_KEY},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            r.read()
            return r.status
    except urllib.error.HTTPError as e:
        e.read()
        return e.code
    except Exception as e:
        return str(e)


def run_batch(concurrency):
    """返回 (总耗时秒, 状态码列表)。"""
    get(MOCK_PORT, "/__reset")
    t0 = time.time()
    with ThreadPoolExecutor(max_workers=concurrency) as ex:
        codes = list(ex.map(one_request, range(M)))
    return time.time() - t0, codes


def get(port, path):
    try:
        with urllib.request.urlopen("http://127.0.0.1:%d%s" % (port, path), timeout=5) as r:
            return json.loads(r.read() or b"{}")
    except Exception:
        return {}


def set_exec(sec):
    get(MOCK_PORT, "/__set_exec?sec=%s" % sec)


# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------
def main():
    global M

    # 后台运行时 stdout 是块缓冲，实验中途看不到进度，强制按行刷。
    try:
        sys.stdout.reconfigure(line_buffering=True)
    except Exception:
        pass

    # --- 启动 mock ---
    srv = QuietServer(("127.0.0.1", MOCK_PORT), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    print("mock 上游已启动: %s  (MIN_INTERVAL=%.1fs)" % (MOCK_BASE, MIN_INTERVAL))

    # --- 对照组：确认 mock 真的会限流 ---
    UP.reset()
    UP.exec_sec = 0.0
    limited = 0
    with ThreadPoolExecutor(max_workers=M) as ex:
        list(ex.map(lambda i: one_mock(i), range(M)))
    st = get(MOCK_PORT, "/__stats")
    limited = st.get("limited", 0)
    print("对照组：以 %.2fs 间隔... 实际上以并发直连 mock，%d/%d 被打回 429"
          % (MIN_INTERVAL, limited, M))
    if limited == 0:
        print("!! mock 没有限流，测不出东西，终止")
        srv.shutdown()
        return
    print("   -> mock 限流语义有效，可以开始测 hub\n")

    results = []

    def scenario(tag, n_acc, exec_sec, mode, affinity="session_then_key"):
        """mode: 'seq' 串行等返回 / 'burst' 全并发。"""
        set_exec(exec_sec)
        data_dir = os.path.join(TMP, "%s" % tag)
        write_data(data_dir, n_acc, affinity)
        proc = start_hub(data_dir, os.path.join(TMP, tag + ".log"))
        try:
            # 预热：让 hub 完成首次加载
            time.sleep(0.3)
            if mode == "seq":
                # 串行：一次一个，等它完全返回再发下一个
                get(MOCK_PORT, "/__reset")
                t0 = time.time()
                codes = [one_request(i) for i in range(M)]
                elapsed = time.time() - t0
            else:
                elapsed, codes = run_batch(M)
            st = get(MOCK_PORT, "/__stats")
            ok = sum(1 for c in codes if c == 200)
            row = {
                "tag": tag,
                "accounts": n_acc,
                "exec_sec": exec_sec,
                "mode": mode,
                "affinity": affinity,
                "elapsed_s": round(elapsed, 2),
                "ok": ok,
                "total": M,
                "codes": codes,
                "upstream_hits": st.get("hits", 0),
                "upstream_429": st.get("limited", 0),
            }
            results.append(row)
            print("  %-22s acc=%d exec=%.1fs %-5s -> %6.2fs  (%d/%d ok, mock429=%d)"
                  % (tag, n_acc, exec_sec, mode, elapsed, ok, M, row["upstream_429"]))
        finally:
            stop_hub(proc)

    print("=" * 74)
    print("开始实验（每任务 %d 个互相独立的请求）" % M)
    print("=" * 74)
    scenario("A1-seq-1acc-fast", 1, 0.3, "seq")
    scenario("A2-seq-2acc-fast", 2, 0.3, "seq")
    scenario("B1-burst-1acc-fast", 1, 0.3, "burst")
    scenario("B2-burst-2acc-fast", 2, 0.3, "burst")
    scenario("B3-burst-3acc-fast", 3, 0.3, "burst")
    scenario("C1-seq-1acc-real", 1, 4.0, "seq")
    scenario("C2-seq-2acc-real", 2, 4.0, "seq")
    scenario("D1-burst-1acc-real", 1, 4.0, "burst")
    scenario("D2-burst-2acc-real", 2, 4.0, "burst")
    scenario("E2-burst-2acc-nosess", 2, 4.0, "burst", affinity="none")

    # --- 汇总 ---
    print()
    print("=" * 74)
    print("汇总")
    print("=" * 74)
    by = {r["tag"]: r for r in results}

    def ratio(a, b):
        if a in by and b in by and by[b]["elapsed_s"] > 0:
            return by[a]["elapsed_s"] / by[b]["elapsed_s"]
        return float("nan")

    print("快速请求(exec=0.3s) 串行 1->2 账号加速比 : %.2fx" % ratio("A1-seq-1acc-fast", "A2-seq-2acc-fast"))
    print("快速请求(exec=0.3s) 并发 1->2 账号加速比 : %.2fx" % ratio("B1-burst-1acc-fast", "B2-burst-2acc-fast"))
    print("快速请求(exec=0.3s) 并发 1->3 账号加速比 : %.2fx" % ratio("B1-burst-1acc-fast", "B3-burst-3acc-fast"))
    print("真实请求(exec=4.0s) 串行 1->2 账号加速比 : %.2fx   <- 关键" % ratio("C1-seq-1acc-real", "C2-seq-2acc-real"))
    print("真实请求(exec=4.0s) 并发 1->2 账号加速比 : %.2fx   <- 关键" % ratio("D1-burst-1acc-real", "D2-burst-2acc-real"))
    print("粘性关闭(affinity=none) 并发 1->2 加速比: %.2fx"
          % ratio("D1-burst-1acc-real", "E2-burst-2acc-nosess"))

    with open(os.path.join(ROOT, "throughput_probe_result.json"), "w", encoding="utf-8") as f:
        json.dump({"min_interval": MIN_INTERVAL, "batch_size": M, "results": results}, f,
                  ensure_ascii=False, indent=2)
    print("\n明细已写入 throughput_probe_result.json")

    srv.shutdown()


def one_mock(i):
    """对照组用：直连 mock（不走 hub）。"""
    payload = json.dumps({"messages": [{"role": "user", "content": str(i)}]}).encode()
    req = urllib.request.Request(
        MOCK_BASE + "/chat/completions", data=payload,
        headers={"Content-Type": "application/json", "Authorization": "Bearer sk-direct"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            r.read()
            return r.status
    except urllib.error.HTTPError as e:
        e.read()
        return e.code
    except Exception as e:
        return str(e)


if __name__ == "__main__":
    main()
