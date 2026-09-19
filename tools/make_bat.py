"""把 tools/bat_src.utf8 生成 GBK + CRLF 的 agnes-hub-go.bat。

为什么必须有这一步
------------------
中文 Windows 的 cmd 按 GBK 逐行解析 .bat。若文件是 UTF-8，含中文的行会被
字节错位切分，报 `'xxx' is not recognized as an internal or external command`，
并且会破坏 `if (...)` 复合块的括号配对 —— 本该跳过的分支反而被执行。

因此约定：
  - 编辑源码请改 tools/bat_src.utf8（UTF-8，便于 git diff 与编辑器）
  - 改完运行本脚本重新生成 agnes-hub-go.bat（GBK + CRLF）
  - 生成后用 tools/check_bat.py 实测分支走向

Write 类工具默认写 UTF-8 + LF，所以**不要**直接编辑 agnes-hub-go.bat。
"""

import os
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRC = os.path.join(os.path.dirname(os.path.abspath(__file__)), "bat_src.utf8")
DST = os.path.join(ROOT, "agnes-hub-go.bat")

text = open(SRC, "r", encoding="utf-8").read()
text = text.replace("\r\n", "\n").replace("\n", "\r\n")
with open(DST, "w", encoding="gbk", newline="") as f:
    f.write(text)

raw = open(DST, "rb").read()
decoded = raw.decode("gbk")
crlf = raw.count(b"\r\n")
bare_lf = raw.count(b"\n") - crlf
full_width_paren = "（" in decoded or "）" in decoded
ellipsis = "…" in decoded
labels = [l.strip() for l in decoded.split("\r\n") if l.strip().startswith(":")]

print(f"已生成 {DST}")
print(f"  字节数      {len(raw)}")
print(f"  CRLF 行数   {crlf}")
print(f"  裸 LF       {bare_lf}   （必须为 0）")
print(f"  全角括号    {full_width_paren}   （必须为 False）")
print(f"  省略号      {ellipsis}   （必须为 False）")
print(f"  标签        {labels}")

problems = []
if crlf == 0:
    problems.append("没有任何 CRLF 换行，cmd 下会出现行拼接问题")
if bare_lf:
    problems.append("存在裸 LF 换行，中文行会被 cmd 解析错位")
if full_width_paren:
    problems.append("含全角括号，echo 文案只用汉字 + 半角标点")
if ellipsis:
    problems.append("含省略号，GBK 下易被截断")
if not labels:
    problems.append("没有找到任何 goto 标签")

if problems:
    print()
    print("结果: FAIL")
    for p in problems:
        print(" -", p)
    sys.exit(1)
print()
print("结果: OK")
