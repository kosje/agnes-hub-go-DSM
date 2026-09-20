#!/usr/bin/env python3
"""生成群晖「套件源」目录（package source catalog），供 GitHub Pages 静态托管。

群晖套件中心添加套件源后，会带上 `?build=&arch=&language=&major=` 去拉这个 JSON，
据此列出可用套件并判断有无新版本。协议细节依据三份互相独立的实现交叉核对：
  - SynoCommunity/spkrepo  spkrepo/views/nas.py + spkrepo/domain/catalog.py（权威）
  - jdel/sspks             lib/SSpkS/Output/JsonOutput.php
  - kromatv/kroma          packages/synology-repo/src/gen-catalog.ts（现代静态托管实现）
三者一致：DSM 7+ 的响应顶层是 {"packages": [...]}。

**为什么按架构分文件**：catalog 条目里没有 arch 字段 —— 架构过滤是服务端按
请求的 arch 参数做的，静态托管做不到。所以一个架构一份 catalog，
x86_64 用 catalog.json（最常见，也是本项目的默认），其余用 catalog-<arch>.json。

**为什么 SPK 也要放到 Pages 上**：catalog 的 link 默认指向 Pages 上的 SPK 副本，
不是 GitHub Release。Release 的下载地址会 302 跳到 objects.githubusercontent.com，
该域名在国内经常不可达、DSM 也可能不跟跨域跳转，表现为套件中心能列出套件、
点安装却报「下载失败」。Pages 直出静态文件、无跳转，且 catalog 与图标本身就
托管在这里 —— 能拉到 catalog 就一定拉得到包。Release 上仍保留一份作为镜像。

用法：
    python tools/build_catalog.py                     # 用 dist/ 下的 spk
    python tools/build_catalog.py --tag v1.0.2-0001   # 指定 Release tag
    python tools/build_catalog.py --link-from-release # link 指回 Release（不推荐）
"""

import argparse
import hashlib
import json
import os
import shutil
import sys
import tarfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from build_spk import (  # noqa: E402
    APP_ID,
    DESCRIPTION,
    MAINTAINER,
    MAINTAINER_URL,
    SERVICE_PORT,
    make_icon_set,
)

REPO = "kosje/agnes-hub-go-DSM"
PAGES_BASE = "https://kosje.github.io/agnes-hub-go-DSM"
REPO_URL = "https://github.com/%s" % REPO

# 主架构用 catalog.json，其余用 catalog-<arch>.json
PRIMARY_ARCH = "x86_64"

# 套件中心里显示的变更说明（HTML）。发新版时改这里。
CHANGELOG = (
    "首个群晖 DSM 套件版本，基于 agnes-hub-go 1.0.2 构建。<br>"
    "· 新增 -no-selfupdate 开关：套件由套件中心负责升级，禁用进程内替换二进制<br>"
    "· 以套件专用账户运行（非 root），支持开机自启与套件中心启停<br>"
    "· 数据目录位于 /var/packages/agnes-hub/var/data，升级保留、卸载删除"
)

CATALOG_FIELDS_NOTE = "SynoCommunity/spkrepo 的 build_entry_data 与 sspks 的字段清单"


def read_spk_info(path):
    """从 SPK 里读 INFO，取出权威的 version / arch / package 等字段。

    版本号以 SPK 内 INFO 为准 —— 套件中心就是拿安装后的版本与 catalog 的
    version 比对来判断有无更新，两边必须完全一致。
    """
    with tarfile.open(path, "r:gz") as t:
        raw = t.extractfile("INFO").read().decode("utf-8")
    fields = {}
    for line in raw.splitlines():
        line = line.strip()
        if "=" in line:
            k, v = line.split("=", 1)
            fields[k.strip()] = v.strip().strip('"')
    return fields


def make_entry(spk_path, link, thumb_urls):
    info = read_spk_info(spk_path)
    data = open(spk_path, "rb").read()
    version = info["version"]

    return {
        "package": info.get("package", APP_ID),
        "version": version,
        "dname": "Agnes Hub",
        "desc": DESCRIPTION,
        "price": 0,
        # 群晖只在下载量超过 1000 时才显示，第三方源填 0 即可
        "download_count": 0,
        "recent_download_count": 0,
        "link": link,
        "size": len(data),
        "md5": hashlib.md5(data).hexdigest(),
        "thumbnail": [thumb_urls[0]],
        "thumbnail_retina": [thumb_urls[1]],
        "snapshot": [],
        "qinst": True,
        "qstart": True,
        "qupgrade": True,
        "deppkgs": None,
        "conflictpkgs": None,
        "start": True,
        "startable": "yes",
        "maintainer": MAINTAINER,
        "maintainer_url": MAINTAINER_URL,
        "distributor": MAINTAINER,
        "distributor_url": MAINTAINER_URL,
        "support_url": REPO_URL + "/issues",
        "changelog": CHANGELOG,
        "thirdparty": True,
        "category": 0,
        "subcategory": 0,
        "type": 0,
        "silent_install": True,
        "silent_uninstall": True,
        "silent_upgrade": True,
    }


def catalog_name_for(arch):
    return "catalog.json" if arch == PRIMARY_ARCH else "catalog-%s.json" % arch


LANDING = """<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Agnes Hub — 群晖 DSM 套件源</title>
<style>
  :root {{ color-scheme: light dark; }}
  body {{ max-width: 860px; margin: 0 auto; padding: 2.5rem 1.25rem 4rem;
         font: 16px/1.7 -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif; }}
  h1 {{ font-size: 1.9rem; margin: 0 0 .4rem; }}
  .sub {{ opacity: .65; margin-bottom: 2rem; }}
  h2 {{ font-size: 1.15rem; margin: 2.2rem 0 .7rem;
        padding-bottom: .35rem; border-bottom: 1px solid rgba(128,128,128,.28); }}
  code, pre {{ font-family: ui-monospace, Consolas, monospace; }}
  code {{ background: rgba(128,128,128,.16); padding: .12em .4em; border-radius: 4px;
          font-size: .9em; word-break: break-all; }}
  pre {{ background: rgba(128,128,128,.12); padding: .85rem 1rem; border-radius: 8px;
         overflow-x: auto; font-size: .87rem; }}
  table {{ border-collapse: collapse; width: 100%; margin: .6rem 0; font-size: .93rem; }}
  th, td {{ border: 1px solid rgba(128,128,128,.32); padding: .5rem .7rem; text-align: left; }}
  th {{ background: rgba(128,128,128,.12); }}
  .note {{ border-left: 3px solid #4a90d9; padding: .6rem 1rem; margin: 1rem 0;
           background: rgba(74,144,217,.09); border-radius: 0 6px 6px 0; }}
  ol, ul {{ padding-left: 1.4rem; }}
  li {{ margin: .3rem 0; }}
</style>
</head>
<body>
<h1>Agnes Hub — 群晖 DSM 套件源</h1>
<p class="sub">Agnes AI 多账号聚合中转 + RPM 限流排队网关，打包为群晖套件。</p>

<h2>在群晖上添加套件源</h2>

<p><strong>第 1 步 · 先放开信任层级</strong>（不放开的话第三方套件装不上）</p>
<ol>
  <li>打开 <strong>套件中心</strong>，点右上角 <strong>设置</strong></li>
  <li>切到 <strong>常规</strong> 标签，找到 <strong>信任层级</strong></li>
  <li>选 <strong>「任何发行者」</strong> → 点 <strong>确定</strong></li>
</ol>
<p class="sub">三个选项的差别：<em>Synology Inc.</em> 只允许群晖官方套件；
<em>Synology Inc. 和信任的发行者</em> 还要有证书；<em>任何发行者</em> 才放行未签名的第三方套件。</p>

<p><strong>第 2 步 · 添加套件源</strong></p>
<ol>
  <li>同一个 <strong>设置</strong> 窗口，切到 <strong>套件来源</strong> 标签</li>
  <li>点 <strong>新增</strong></li>
  <li><strong>名称</strong>：随便填，比如 <code>Agnes Hub</code></li>
  <li><strong>位置</strong>：填下面这个地址</li>
</ol>
<table>
  <tr><th>你的机型</th><th>要填的「位置」</th></tr>
  {source_rows}
</table>
<ol start="5">
  <li>点 <strong>确定</strong></li>
</ol>

<p><strong>第 3 步 · 安装</strong></p>
<p>添加成功后，切到套件中心的 <strong>「社群」</strong> 标签页（不是「所有套件」），
就能看到 Agnes Hub，点「安装」即可。以后有新版本会直接在这里提示更新。</p>

<div class="note">
  <strong>为什么地址里没有架构信息</strong>：群晖 catalog 的条目里没有架构字段 ——
  架构过滤是服务端按请求的 <code>arch</code> 参数做的。本套件源是 GitHub Pages
  静态托管，做不到按参数返回不同内容，所以一个架构一份文件。本项目只分发
  <strong>x86_64</strong>（覆盖 DS918+ 等 apollolake 平台的全部 Intel/AMD 机型）。
</div>

<h2>当前版本</h2>
<table>
  <tr><th>套件</th><th>版本</th><th>架构</th><th>大小</th><th>MD5</th></tr>
  {version_rows}
</table>

<h2>安装后</h2>
<ul>
  <li>控制台：<code>http://&lt;NAS 地址&gt;:{port}/console</code>，初始密码 <code>admin123</code>，请立即修改</li>
  <li>客户端接入：<code>http://&lt;NAS 地址&gt;:{port}/v1</code>，模型名 <code>agnes-auto</code></li>
  <li>数据目录：<code>/var/packages/agnes-hub/var/data</code>（含 <code>accounts.json</code>，请自行备份）</li>
</ul>

<h2>关于升级</h2>
<p>套件由套件中心管理，<strong>内置自更新已禁用</strong>（启动时带 <code>-no-selfupdate</code>）。
原因是套件的 <code>INFO</code> 里登记了 <code>package.tgz</code> 的 checksum，
若允许进程内替换二进制，套件实际内容就会与已安装版本对不上，下次升级必然冲突。
本套件源会把新版本列进套件中心，直接点「更新」即可。</p>
<p>升级<strong>保留</strong>数据目录，只有卸载套件才会连同删除。</p>

<h2>手动下载</h2>
<p>不想加套件源的话，直接下这个文件，再用「套件中心 → 手动安装」装：</p>
<p>{download_list}</p>
<p class="sub">这些链接指向本站（GitHub Pages），不经过 GitHub Release 的跳转 ——
国内网络访问 Release 下载地址常会失败。</p>

<p class="sub" style="margin-top:2.5rem">
  源码与完整文档：<a href="{repo}">{repo_short}</a>
</p>
</body>
</html>
"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dist", default="dist", help="SPK 所在目录（默认 dist/）")
    ap.add_argument("--out", default="docs", help="输出目录（默认 docs/）")
    ap.add_argument("--icon", default="assets/ICON.PNG", help="图标源文件")
    ap.add_argument("--tag", default=None,
                    help="Release tag（默认 v<SPK 版本>，如 v1.0.2-0001）；"
                         "仅 --link-from-release 时会用到")
    ap.add_argument("--link-from-release", action="store_true",
                    help="把 link 指向 GitHub Release 而不是 Pages（默认走 Pages）")
    ap.add_argument("--arch", default=PRIMARY_ARCH,
                    help="要纳入 catalog 的架构，逗号分隔；默认 %s，用 all 表示全部"
                         % PRIMARY_ARCH)
    args = ap.parse_args()

    all_spks = sorted(f for f in os.listdir(args.dist) if f.endswith(".spk"))
    if not all_spks:
        sys.exit("[ERROR] %s 下没有 .spk，先跑 tools/build_spk.py" % args.dist)

    # 按文件名里的 -<arch>- 过滤。dist/ 里可能同时存在多个架构的包
    # （build_spk.py --arch all 会产出两个），但本项目的分发只面向 x86_64。
    if args.arch == "all":
        spks = all_spks
    else:
        wanted = {a.strip() for a in args.arch.split(",") if a.strip()}
        spks = [f for f in all_spks if any("-%s-" % a in f for a in wanted)]
        skipped = [f for f in all_spks if f not in spks]
        if skipped:
            print("跳过（不在 --arch=%s 范围内）：%s" % (args.arch, "、".join(skipped)))
    if not spks:
        sys.exit("[ERROR] 没有匹配 --arch=%s 的 SPK，dist/ 里有：%s"
                 % (args.arch, "、".join(all_spks)))

    os.makedirs(args.out, exist_ok=True)

    # 图标：套件中心列表用 72×72，视网膜屏用 256×256
    make_icon_set(args.icon, [
        (72, os.path.join(args.out, "%s-72.png" % APP_ID)),
        (256, os.path.join(args.out, "%s-256.png" % APP_ID)),
    ])
    thumb_urls = ["%s/%s-72.png" % (PAGES_BASE, APP_ID),
                  "%s/%s-256.png" % (PAGES_BASE, APP_ID)]

    written = []
    meta = []
    for name in spks:
        path = os.path.join(args.dist, name)
        info = read_spk_info(path)
        arch = info["arch"]
        version = info["version"]
        tag = args.tag or ("v%s" % version)

        # link 默认指向 Pages 上的 SPK 副本，而不是 GitHub Release。
        # 原因：Release 的下载地址会 302 跳到 objects.githubusercontent.com，
        # 这个域名在国内经常不可达，DSM 也可能不跟跨域跳转 —— 表现为套件中心
        # 能列出套件但点安装报「下载失败」。而 Pages 是直出静态文件，无跳转，
        # 且 catalog 与图标本身就托管在这里，能拉到 catalog 就一定能拉包。
        if args.link_from_release:
            link = "%s/releases/download/%s/%s" % (REPO_URL, tag, name)
        else:
            shutil.copy2(path, os.path.join(args.out, name))
            link = "%s/%s" % (PAGES_BASE, name)

        entry = make_entry(path, link, thumb_urls)
        out_name = catalog_name_for(arch)
        out_path = os.path.join(args.out, out_name)
        with open(out_path, "w", encoding="utf-8", newline="\n") as fh:
            json.dump({"packages": [entry]}, fh, ensure_ascii=False, indent=2)
            fh.write("\n")
        written.append(out_path)
        meta.append({
            "arch": arch,
            "version": version,
            "size": entry["size"],
            "md5": entry["md5"],
            "file": name,
            "catalog": out_name,
            "tag": tag,
        })
        print("  [%s] %s" % (arch, out_path))
        print("      version=%s  size=%d  md5=%s" % (version, entry["size"], entry["md5"]))

    # 落地页
    arch_label = {
        "x86_64": "Intel / AMD 64 位机型（apollolake、avoton、braswell、broadwell、"
                  "bromolow、cedarview、coffeelake、denverton、geminilake、grantley、"
                  "kvmx64、purley、skylaked、v1000 等）",
        "armv8": "ARM64 机型（rtd1296、rtd1619、rtd1619b、armada37xx，如 DS223 / DS423 系列）",
    }
    source_rows = "\n  ".join(
        "<tr><td>%s</td><td><code>%s/%s</code></td></tr>"
        % (arch_label.get(m["arch"], m["arch"]), PAGES_BASE, m["catalog"])
        for m in sorted(meta, key=lambda x: x["arch"] != PRIMARY_ARCH)
    )
    version_rows = "\n  ".join(
        "<tr><td>%s</td><td>%s</td><td>%s</td><td>%.2f MB</td><td><code>%s</code></td></tr>"
        % (APP_ID, m["version"], m["arch"], m["size"] / 1024 / 1024, m["md5"])
        for m in sorted(meta, key=lambda x: x["arch"])
    )
    download_list = "<br>\n".join(
        '<a href="%s/%s"><code>%s</code></a>（%s，%.2f MB）'
        % (PAGES_BASE, m["file"], m["file"], m["arch"], m["size"] / 1024 / 1024)
        for m in sorted(meta, key=lambda x: x["arch"])
    )
    landing = LANDING.format(source_rows=source_rows, version_rows=version_rows,
                             download_list=download_list,
                             port=SERVICE_PORT, repo=REPO_URL, repo_short=REPO)
    index_path = os.path.join(args.out, "index.html")
    with open(index_path, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(landing)
    print("  落地页 %s" % index_path)

    print()
    print("套件源地址（DSM → 套件中心 → 设置 → 套件来源 → 新增）：")
    for m in sorted(meta, key=lambda x: x["arch"] != PRIMARY_ARCH):
        print("  %-8s %s/%s" % (m["arch"], PAGES_BASE, m["catalog"]))
    print()
    print("注意：catalog 里的 version 必须与 SPK 内 INFO 的 version 完全一致，")
    print("      套件中心就是靠它判断有无更新。")
    if args.link_from_release:
        print("      link 指向 Release，需确保对应 tag 已存在。")
    else:
        print("      SPK 已复制到 %s/ 并随 Pages 发布；记得一并提交。" % args.out)
    print("      字段依据：%s。" % CATALOG_FIELDS_NOTE)

if __name__ == "__main__":
    main()
