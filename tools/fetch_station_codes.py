# -*- coding: utf-8 -*-
"""低频抓取一次 12306 静态车站码表（站名 → 电报码）。
仅此一处联网，单次请求，供 raildata 导入时填充 station.telegraph_code。
"""
import csv
import os
import ssl
import sys
import time
import urllib.request

URL = "https://kyfw.12306.cn/otn/resources/js/framework/station_name.js"
OUT = r"D:\AI project\rail-planner\data\csv\station_code.csv"


def load_text():
    """优先读取命令行传入的本地 js 文件（可用 curl 下载），否则联网抓取一次。"""
    if len(sys.argv) > 1 and os.path.isfile(sys.argv[1]):
        with open(sys.argv[1], "rb") as f:
            raw = f.read()
        print("使用本地文件:", sys.argv[1])
    else:
        req = urllib.request.Request(URL, headers={
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64)",
            "Referer": "https://kyfw.12306.cn/otn/leftTicket/init",
        })
        print("请求车站码表（单次，低频）...")
        time.sleep(1)  # 访问间隔 >= 1s
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        with urllib.request.urlopen(req, timeout=25, context=ctx) as resp:
            raw = resp.read()
    for enc in ("utf-8", "gbk"):
        try:
            return raw.decode(enc)
        except UnicodeDecodeError:
            continue
    raise SystemExit("解码失败")


def main():
    text = load_text()
    if "@" not in text:
        raise SystemExit("返回内容格式异常，可能被拦截")

    rows = []
    for item in text.split("@")[1:]:
        parts = item.split("|")
        if len(parts) >= 3 and parts[1] and parts[2]:
            rows.append((parts[1], parts[2]))

    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    with open(OUT, "w", encoding="utf-8", newline="") as f:
        w = csv.writer(f)
        w.writerow(["station_name", "telegraph_code"])
        w.writerows(rows)
    print(f"已保存 {len(rows)} 条 -> {OUT}")


if __name__ == "__main__":
    main()
