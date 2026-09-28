# -*- coding: utf-8 -*-
"""抓取车次详情页 → 每站累计里程 + 各席别票价（含硬卧/软卧）。

- 这是唯一联网的采集模块，与 Go 查询主程序完全解耦；请保持低频、礼貌抓取。
- 支持断点续抓（已下载的 HTML 自动跳过），失败车次会自动重试并记录。
- 产物：
    data/raw/pages/<车次>.html      原始页面
    data/csv/stop_detail.csv        解析后的长表：车次,站序,站名,里程km,到达,发车,席别,票价
    data/raw/fetch.log              运行日志

用法：
    python tools/fetch_fares.py                 # 全量（默认 5 并发）
    python tools/fetch_fares.py --limit 20      # 小规模测试
    python tools/fetch_fares.py --workers 4
"""
import argparse
import csv
import os
import random
import re
import sqlite3
import sys
import threading
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed

ROOT = r'D:\AI project\rail-planner'
DB = os.path.join(ROOT, 'data', 'rail.db')
PAGES = os.path.join(ROOT, 'data', 'raw', 'pages')
OUT_CSV = os.path.join(ROOT, 'data', 'csv', 'stop_detail.csv')
OD_CSV = os.path.join(ROOT, 'data', 'csv', 'od_fare.csv')
LOG = os.path.join(ROOT, 'data', 'raw', 'fetch.log')

UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
URL = "https://www.gaotie.com.cn/checi/{}.html"

_lock = threading.Lock()
_stat = {'ok': 0, 'cached': 0, 'fail': 0}
_failed = []


def log(msg):
    with _lock:
        with open(LOG, 'a', encoding='utf-8') as f:
            f.write(f"{time.strftime('%H:%M:%S')} {msg}\n")


def load_trains():
    """读取数据库中的车次号（去重，已按等级排序）"""
    con = sqlite3.connect(DB)
    rows = con.execute("SELECT DISTINCT train_no, grade FROM train ORDER BY grade, train_no").fetchall()
    con.close()
    out = []
    for no, grade in rows:
        no = (no or '').strip()
        if not no:
            continue
        # 复合车次号（如 K282/K283）取主号
        main = no.split('/')[0].strip()
        if re.fullmatch(r'[A-Z]\d{1,4}', main):
            out.append((main, grade))
    # 去重
    seen, uniq = set(), []
    for no, grade in out:
        if no in seen:
            continue
        seen.add(no)
        uniq.append((no, grade))
    return uniq


def fetch_one(train_no):
    path = os.path.join(PAGES, f"{train_no}.html")
    if os.path.exists(path) and os.path.getsize(path) > 5000:
        return 'cached'
    req = urllib.request.Request(URL.format(train_no), headers={
        'User-Agent': UA,
        'Accept-Language': 'zh-CN,zh;q=0.9',
    })
    last = ''
    for attempt in range(3):
        try:
            with urllib.request.urlopen(req, timeout=25) as r:
                data = r.read()
            if len(data) < 3000:
                raise ValueError(f'body too small ({len(data)})')
            with open(path, 'wb') as f:
                f.write(data)
            time.sleep(random.uniform(0.08, 0.22))  # 礼貌延迟，避免给对方服务器压力
            return 'ok'
        except Exception as e:  # noqa: BLE001
            last = f"{type(e).__name__}:{e}"
            time.sleep(1.2 * (attempt + 1))
    return 'fail:' + last


# ── 解析 ────────────────────────────────────────────────────
RE_STOPNO = re.compile(r'ZX-background">(\d+)</span>')
RE_STATION = re.compile(r'<a href="/chezhan/[^"]*" class="Z-background">([^<]+)</a>')
RE_KM = re.compile(r'里程\((\d+)km\)')
RE_ARR = re.compile(r'到站时间\((\d{2}:\d{2})\)')
RE_DEP = re.compile(r'发车时间\((\d{2}:\d{2})\)')
RE_FARE = re.compile(r'([\u4e00-\u9fa5]{2,6}):¥([\d.]+)')
RE_ELAPSED = re.compile(r'历时\(([^)]+)\)')
RE_STOP_MIN = re.compile(r'停留(\d+)分')

SEAT_COLS = ['硬座', '硬卧上', '硬卧中', '硬卧下', '软卧上', '软卧下', '软座', '无座',
             '二等座', '一等座', '商务座']

# 席别中文名 → 数据库标识
SEAT_KEY = {
    '硬座': 'hard_seat', '硬卧上': 'hard_sleep_up', '硬卧中': 'hard_sleep_mid',
    '硬卧下': 'hard_sleep_low', '软卧上': 'soft_sleep_up', '软卧下': 'soft_sleep_low',
    '软座': 'soft_seat', '无座': 'no_seat', '二等座': 'second',
    '一等座': 'first', '商务座': 'business',
}


def parse_elapsed_minutes(text):
    """'1小时43分' / '33小时4分' / '2天5小时49分' / '54分' → 分钟数"""
    days = hours = mins = 0
    m = re.search(r'(\d+)天', text)
    if m:
        days = int(m.group(1))
    m = re.search(r'(\d+)小时', text)
    if m:
        hours = int(m.group(1))
    m = re.search(r'(\d+)分', text)
    if m:
        mins = int(m.group(1))
    return days * 1440 + hours * 60 + mins


def parse_page(path):
    """解析单个车次页面 → [(站序, 站名, km, 到达, 发车, 累计到达分钟, 累计发车分钟, {席别: 累计票价})]"""
    with open(path, encoding='utf-8', errors='replace') as f:
        html = f.read()
    chunks = re.split(r'<div class="ZX"', html)[1:]
    stops = []
    for ch in chunks:
        m_no = RE_STOPNO.search(ch)
        m_st = RE_STATION.search(ch)
        if not m_no or not m_st:
            continue
        km = RE_KM.search(ch)
        arr = RE_ARR.search(ch)
        dep = RE_DEP.search(ch)
        el = RE_ELAPSED.search(ch)
        arr_elapsed = parse_elapsed_minutes(el.group(1)) if el else 0
        stop_min = RE_STOP_MIN.search(ch)
        dep_elapsed = arr_elapsed + (int(stop_min.group(1)) if stop_min else 0)
        fname = m_st.group(1).strip()
        name = fname[:-1] if fname.endswith('站') and len(fname) > 1 else fname
        fares = {k: float(v) for k, v in RE_FARE.findall(ch)}
        stops.append((
            int(m_no.group(1)),
            name,
            int(km.group(1)) if km else 0,
            arr.group(1) if arr else '',
            dep.group(1) if dep else '',
            arr_elapsed,
            dep_elapsed,
            fares,
        ))
    return stops


def linearize(stops):
    """把「自始发站累计票价」线性化为「里程 × 全程平均单价」。

    抓取页给的是累计价，而国铁长途存在递远递减：跨线车次的尾段累计差会被严重压缩，
    直接相减会算出「商务座比二等座还便宜」这类错误区间价。按全程平均单价线性化后，
    区间票价 = 里程差 × 单价，既保持席别间的正确比例，又保证总价守恒。
    """
    ref_km, ref_fares = 0, {}
    for s in stops:
        if s[2] > 0 and any(v > 0 for v in s[7].values()):
            ref_km, ref_fares = s[2], s[7]
    if ref_km <= 0:
        return stops
    units = {k: v / ref_km for k, v in ref_fares.items() if v > 0}
    out = []
    for s in stops:
        seq, name, km, arr, dep, a_el, d_el, _ = s
        fares = {k: (round(km * u, 1) if km > 0 else 0.0) for k, u in units.items()}
        out.append((seq, name, km, arr, dep, a_el, d_el, fares))
    return out


def median(xs):
    xs = sorted(xs)
    n = len(xs)
    if n == 0:
        return 0
    return xs[n // 2] if n % 2 else (xs[n // 2 - 1] + xs[n // 2]) / 2


def fix_km(stops, std_km):
    """用全路网标准区间里程重算累计里程。

    同一站对会出现在多趟车次里，取里程差的中位数作为标准值，可修正个别车次页面上的
    错误里程（例如某车次把汉中→广元记成 38km，会让该段票价严重失真）。
    返回 (新 stops, 修正处数)。
    """
    out, km, fixed = [], 0.0, 0
    for i, s in enumerate(stops):
        seq, name, orig, arr, dep, a_el, d_el, fares = s
        if i > 0:
            prev = stops[i - 1]
            key = (prev[1], name)
            d = orig - prev[2]
            if key in std_km:
                nd = std_km[key]
                if abs(nd - d) > max(20.0, 0.25 * nd):
                    fixed += 1
                km += nd
            elif 0 < d < 2000:
                km += d
        out.append((seq, name, int(round(km)), arr, dep, a_el, d_el, fares))
    return out, fixed


def percentile(xs, q):
    """取分位数（用于票价曲线：用 25 分位抑制高票价线路的污染）"""
    xs = sorted(xs)
    if not xs:
        return 0.0
    i = int(len(xs) * q)
    return xs[min(i, len(xs) - 1)]


def build_od_fares(parsed):
    """抽取真实 OD 票价表。

    每趟车的「始发站 → 第 N 站」票价是 12306 上直接可售的真实价格（不是推算值）。
    把这些组合抽出来，就得到一张真实 OD 票价表；查询段票价时优先查它。
    同一 OD 可能被多趟车覆盖，取中位数消除个别脏数据。
    """
    acc = {}
    for no, stops in parsed.items():
        if len(stops) < 2:
            continue
        tt = train_type_of(no)
        origin = stops[0]
        o_km = origin[2]
        for s in stops[1:]:
            km = s[2] - o_km
            if km <= 0:
                continue
            for seat, price in s[7].items():
                if price > 0 and seat in SEAT_KEY:
                    acc.setdefault((origin[1], s[1], tt, SEAT_KEY[seat]), []).append((km, price))

    out = {}
    for key, vals in acc.items():
        km = median([v[0] for v in vals])
        out[key] = (int(round(km)), median([v[1] for v in vals]))
    return out


def train_type_of(train_no):
    """按车次号前缀判断车次类型"""
    p = train_no[0].upper()
    return {'G': '高铁', 'C': '城际', 'S': '城际', 'D': '动车',
            'Z': '普速', 'T': '普速', 'K': '普速', 'Y': '普速', 'P': '普客'}.get(p, '普速')


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--limit', type=int, default=0, help='仅抓前 N 个（测试用）')
    ap.add_argument('--workers', type=int, default=5)
    ap.add_argument('--parse-only', action='store_true', help='跳过抓取，仅重新解析已下载页面')
    args = ap.parse_args()

    os.makedirs(PAGES, exist_ok=True)
    os.makedirs(os.path.dirname(OUT_CSV), exist_ok=True)

    trains = load_trains()
    if args.limit:
        trains = trains[:args.limit]
    print(f'车次总数：{len(trains)}，并发 {args.workers}')
    log(f'=== start, trains={len(trains)} workers={args.workers} ===')

    if not args.parse_only:
        t0 = time.time()
        done = 0
        with ThreadPoolExecutor(max_workers=args.workers) as ex:
            futs = {ex.submit(fetch_one, no): no for no, _ in trains}
            for fu in as_completed(futs):
                no = futs[fu]
                try:
                    r = fu.result()
                except Exception as e:  # noqa: BLE001
                    r = 'fail:' + str(e)
                if r == 'ok':
                    _stat['ok'] += 1
                elif r == 'cached':
                    _stat['cached'] += 1
                else:
                    _stat['fail'] += 1
                    _failed.append((no, r))
                done += 1
                if done % 200 == 0 or done == len(trains):
                    el = time.time() - t0
                    speed = done / el if el > 0 else 0
                    eta = (len(trains) - done) / speed if speed > 0 else 0
                    msg = (f'进度 {done}/{len(trains)} 新增{_stat["ok"]} 缓存{_stat["cached"]} '
                           f'失败{_stat["fail"]} 速度{speed:.1f}/s ETA{eta/60:.1f}min')
                    print('  ' + msg, flush=True)
                    log(msg)

    # 第一遍：解析全部页面，收集相邻区间里程样本
    print('解析页面...')
    parsed, seg_samples = {}, {}
    files = [f for f in os.listdir(PAGES) if f.endswith('.html')]
    for fn in sorted(files):
        no = fn[:-5]
        try:
            stops = parse_page(os.path.join(PAGES, fn))
            if len(stops) < 2:
                continue
            parsed[no] = stops
            for i in range(1, len(stops)):
                d = stops[i][2] - stops[i - 1][2]
                if 0 < d < 2000:
                    seg_samples.setdefault((stops[i - 1][1], stops[i][1]), []).append(d)
        except Exception as e:  # noqa: BLE001
            log(f'parse fail {fn}: {e}')

    # 全路网区间里程交叉校验（取中位数，至少 3 个样本才采信）
    std_km = {k: median(v) for k, v in seg_samples.items() if len(v) >= 3}

    # 输出真实 OD 票价表（优先数据源）
    od = build_od_fares(parsed)
    with open(OD_CSV, 'w', encoding='utf-8', newline='') as f:
        w = csv.writer(f)
        w.writerow(['from_station', 'to_station', 'train_type', 'seat', 'km', 'price'])
        for (fr, to, tt, seat), (km, price) in od.items():
            w.writerow([fr, to, tt, seat, km, round(price, 1)])
    print(f'已写出真实 OD 票价 {len(od)} 条 → {OD_CSV}')

    # 第二遍：修正里程 → 输出宽表（保留原始累计价，段价由查询端计算）
    rows, fixed_total = [], 0
    for no, stops in parsed.items():
        tt = train_type_of(no)
        stops, fx = fix_km(stops, std_km)
        fixed_total += fx
        for (seq, st, km, arr, dep, a_el, d_el, fares) in stops:
            # 保留**原始累计价**：查询端用它判断「该段是否售该席别」，
            # 实际段价再由查询端按真实 OD 表 / 里程曲线计算。
            # 若在这里用曲线重算累计价，相邻里程落在曲线同一台阶时差值会变成 0，
            # 整段会被误判为「不售该席别」而消失。
            vals = [round(fares.get(seat, 0), 1) for seat in SEAT_COLS]
            rows.append([no, tt, seq, st, km, arr, dep, a_el, d_el] + vals)
    print(f'里程校验：标准区间 {len(std_km)} 个，修正异常里程 {fixed_total} 处')

    with open(OUT_CSV, 'w', encoding='utf-8', newline='') as f:
        w = csv.writer(f)
        w.writerow(['train_no', 'train_type', 'stop_seq', 'station_name', 'km',
                    'arrive_time', 'depart_time', 'arrive_elapsed', 'depart_elapsed'] + SEAT_COLS)
        w.writerows(rows)

    print(f'已解析 {len(parsed)} 个车次 → {len(rows)} 条停靠站记录')
    print(f'输出：{OUT_CSV}')
    if _failed:
        print(f'失败车次 {len(_failed)} 个（可重跑续抓），例：{_failed[:5]}')
    log(f'=== done pages={len(files)} rows={len(rows)} fail={len(_failed)} ===')


if __name__ == '__main__':
    main()
