# -*- coding: utf-8 -*-
"""验收脚本：对「汉中 → 重庆」用例逐条核对 5 项验收标准。

用法（工程根目录）：python tools/acceptance.py
"""
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
EXE = ROOT / "bin" / "railplan.exe"
DB = ROOT / "data" / "rail.db"


def run_search():
    cmd = [str(EXE), "-from", "汉中", "-to", "重庆", "-top", "200", "-v",
           "-db", str(DB)]
    p = subprocess.run(cmd, capture_output=True, cwd=str(ROOT))
    for enc in ("utf-8", "gbk"):
        try:
            return p.stdout.decode(enc)
        except UnicodeDecodeError:
            continue
    return p.stdout.decode("utf-8", "replace")


def to_minutes(text):
    """把 '5小时02分' / '43分' 转成分钟"""
    m = re.search(r"(?:(\d+)小时)?(\d+)分", text)
    if not m:
        return None
    return int(m.group(1) or 0) * 60 + int(m.group(2))


def parse(out):
    direct_min = None
    m = re.search(r"直达基准(（[^）]*）)?：¥([\d.]+) ／ (.+?)\n", out)
    if m:
        direct_min = to_minutes(m.group(3))

    routes = []
    blocks = re.split(r"\n【方案 (\d+)】", out)[1:]
    for i in range(0, len(blocks) - 1, 2):
        idx = int(blocks[i])
        body = blocks[i + 1]
        tm = re.search(r"(\d+) 次中转", body)
        transfers = int(tm.group(1)) if tm else 0  # 0 次中转显示为「直达」
        total = to_minutes(re.search(r"全程 (.+?) ｜", body).group(1))
        price = float(re.search(r"参考总价 ¥([\d.]+)", body).group(1))
        waits = [to_minutes(w) for w in re.findall(r"等待 (.+?)\n", body)]
        legs = re.findall(r"\d+\) (\S+)\s+(\S+)\s+(\S+)\s+(\d{2}:\d{2}) → (\S+)\s+(\d{2}:\d{2})", body)
        vias = re.findall(r"换乘 (\S+?)\s", body)
        routes.append(dict(idx=idx, transfers=transfers, total=total, price=price,
                           waits=waits, legs=legs, vias=vias))
    return direct_min, routes


def main():
    if not EXE.exists():
        print("未找到", EXE, "，请先编译")
        sys.exit(1)
    out = run_search()
    direct_min, routes = parse(out)

    print("=" * 68)
    print("验收用例：汉中 → 重庆")
    print("=" * 68)
    print(f"直达最快：{direct_min} 分钟" if direct_min else "直达最快：无直达")
    print(f"解析到方案：{len(routes)} 条\n")

    results = []

    # ① 生成 汉中→广元→重庆 的 1 次中转方案且排名靠前
    target = None
    for r in routes:
        seq = [l[4] for l in r["legs"]]  # 到达站序列（legs 第5列是到达站）
        if r["transfers"] == 1 and len(r["legs"]) == 2 \
                and r["legs"][0][4] == "广元" and r["legs"][1][4].startswith("重庆"):
            target = r
            break
    ok1 = target is not None and target["idx"] <= 10
    results.append(("① 生成「汉中→广元→重庆」1次中转方案且排名靠前（前10）", ok1,
                    f"命中方案 #{target['idx']}，总价 ¥{target['price']}" if target else "未命中"))

    # ② 换乘间隔 ≥ 25 分钟
    bad = [(r["idx"], w) for r in routes for w in r["waits"] if w is not None and w < 25]
    results.append(("② 所有换乘间隔 ≥ 25 分钟", len(bad) == 0,
                    f"违规 {len(bad)} 处" + (f"，例：方案#{bad[0][0]} 等待{bad[0][1]}分" if bad else "")))

    # ③ 无 4 次及以上中转
    over = [r["idx"] for r in routes if r["transfers"] >= 4]
    results.append(("③ 不出现 4 次及以上中转", len(over) == 0, f"违规 {len(over)} 条"))

    # ④ 总耗时超直达 2 倍的在搜索阶段被剪枝（直达方案本身是基准，不计入）
    if direct_min:
        limit = direct_min * 2.0
        overtime = [r["idx"] for r in routes if r["transfers"] > 0 and r["total"] > limit]
        m = re.search(r"剪枝 深度 (\d+) / 时间 (\d+) / 支配 (\d+) / 价格 (\d+)", out)
        time_pruned = int(m.group(2)) if m else 0
        results.append((f"④ 无超时方案（限 {limit:.0f} 分）且搜索阶段有剪枝",
                        len(overtime) == 0 and time_pruned > 0,
                        f"超时方案 {len(overtime)} 条，搜索期时间剪枝 {time_pruned} 次"))
    else:
        results.append(("④ 超时剪枝", True, "无直达，跳过时间硬剪枝判定"))

    # ⑤ 结果 ≤ 200 且带完整免责声明
    ok5 = len(routes) <= 200 and "真实票价、余票请前往12306官方核验" in out
    results.append(("⑤ 结果 ≤200 条且末尾带完整免责声明", ok5,
                    f"方案数 {len(routes)}，免责声明{'存在' if '免责声明' in out else '缺失'}"))

    print("-" * 68)
    for name, ok, detail in results:
        print(f"[{'PASS' if ok else 'FAIL'}] {name}\n        {detail}")
    print("-" * 68)
    passed = sum(1 for _, ok, _ in results if ok)
    print(f"结果：{passed}/{len(results)} 项通过")
    print("\n----- 前 3 条方案预览 -----")
    seg = out.split("【方案 1】")
    if len(seg) > 1:
        preview = "【方案 1】" + seg[1]
        print("\n".join(preview.splitlines()[:24]))
    sys.exit(0 if passed == len(results) else 1)


if __name__ == "__main__":
    main()
