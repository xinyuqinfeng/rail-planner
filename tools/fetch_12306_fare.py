# -*- coding: utf-8 -*-
"""从 12306 官方接口批量抓取**真实区间票价**（含全部席别）。

这是唯一联网的票价采集模块，与 Go 查询主程序解耦；请保持低频礼貌抓取。

接口要点（已验证）：
  1. GET /otn/leftTicket/init 拿基础 Cookie（不需要 RAIL_DEVICEID）
  2. /otn/leftTicket/query 会返回 {"c_url":"leftTicket/queryG"} 做端点重定向
  3. 结果每行按 '|' 分割，第 39 字段 = yp_info，每 10 字符一个价格单元：
     首字符=席别码（O=二等座 M=一等座 9=商务座 3=硬卧 1/2=硬座 W=无座），
     第 2~6 位 ÷10 = 元，第 7~10 位 ≥3000 表示无座

OD 清单：
    --all        全国全部有直达车次的站对（tools/build_od_all.py 生成，19.7 万个，
                 按 1.2s/请求约需 66 小时，支持断点续抓分多次跑，热门优先）
    --limit N    只抓前 N 个
    --ods 文件   自定义清单（每行 出发站,到达站）

产物：
    data/csv/od_fare_12306.csv   真实票价（追加写）
    data/raw/fare12306_done.txt  已完成 OD（断点续抓）
    data/raw/fare12306_meta.json 抓取时间等元信息
"""
import argparse
import csv
import json
import os
import random
import sys
import time
import urllib.parse
import urllib.request

sys.stdout.reconfigure(encoding='utf-8')

ROOT = r'D:\AI project\rail-planner'
CODE_CSV = os.path.join(ROOT, 'data', 'csv', 'station_code.csv')
OD_ALL = os.path.join(ROOT, 'data', 'raw', 'od_all.txt')
OD_SRC = os.path.join(ROOT, 'data', 'csv', 'od_fare.csv')
OUT_CSV = os.path.join(ROOT, 'data', 'csv', 'od_fare_12306.csv')
DONE_TXT = os.path.join(ROOT, 'data', 'raw', 'fare12306_done.txt')
META_JSON = os.path.join(ROOT, 'data', 'raw', 'fare12306_meta.json')

UA = ('Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
      '(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36')
API = 'https://kyfw.12306.cn'
INIT_URL = API + '/otn/leftTicket/init'

SEAT_CODE = {
    'O': 'second', 'M': 'first', '9': 'business', 'P': 'business',
    '3': 'hard_sleep', 'S': 'soft_sleep', '6': 'soft_sleep',
    '4': 'soft_seat', '1': 'hard_seat', '2': 'hard_seat', 'W': 'no_seat',
}


def load_codes():
    out = {}
    with open(CODE_CSV, encoding='utf-8') as f:
        for r in csv.DictReader(f):
            out[r['station_name']] = r['telegraph_code']
    return out


def load_ods(args):
    if args.ods:
        out = []
        with open(args.ods, encoding='utf-8') as f:
            for line in f:
                parts = line.strip().replace('，', ',').split(',')
                if len(parts) >= 2 and parts[0].strip():
                    out.append((parts[0].strip(), parts[1].strip()))
        return out
    if args.all:
        out = []
        with open(OD_ALL, encoding='utf-8') as f:
            for line in f:
                p = line.strip().split(',')
                if len(p) >= 2:
                    out.append((p[0], p[1]))
        return out
    # 默认：现有 od_fare.csv 的 OD 按车次数排序
    from collections import Counter
    cnt = Counter()
    with open(OD_SRC, encoding='utf-8') as f:
        for r in csv.DictReader(f):
            cnt[(r['from_station'], r['to_station'])] += 1
    return [k for k, _ in cnt.most_common()]


class Client:
    def __init__(self):
        self.cookie = ''
        self.path = 'leftTicket/queryG'

    def _req(self, url, headers=None):
        h = {'User-Agent': UA, 'Accept-Language': 'zh-CN,zh;q=0.9'}
        if self.cookie:
            h['Cookie'] = self.cookie
        if headers:
            h.update(headers)
        req = urllib.request.Request(url, headers=h)
        with urllib.request.urlopen(req, timeout=25) as r:
            return r.read(), r.headers.get_all('Set-Cookie') or []

    def refresh(self):
        _, setc = self._req(INIT_URL)
        jar = [c.split(';')[0].strip() for c in setc if '=' in c]
        if jar:
            self.cookie = '; '.join(jar)

    def resolve_path(self, fc, tc, date):
        url = (API + '/otn/leftTicket/query'
               f'?leftTicketDTO.train_date={date}'
               f'&leftTicketDTO.from_station={fc}'
               f'&leftTicketDTO.to_station={tc}&purpose_codes=ADULT')
        try:
            raw, _ = self._req(url, {'Referer': INIT_URL})
            d = json.loads(raw.decode('utf-8-sig'))
            if d.get('c_url'):
                self.path = d['c_url']
        except Exception:  # noqa: BLE001
            pass

    def query(self, fc, tc, date):
        url = (f'{API}/otn/{self.path}'
               f'?leftTicketDTO.train_date={date}'
               f'&leftTicketDTO.from_station={fc}'
               f'&leftTicketDTO.to_station={tc}&purpose_codes=ADULT')
        raw, _ = self._req(url, {'Referer': INIT_URL})
        return json.loads(raw.decode('utf-8-sig'))


def parse_ypinfo(s):
    out = []
    for i in range(0, len(s) - 9, 10):
        seg = s[i:i + 10]
        try:
            price = int(seg[1:6]) / 10.0
        except ValueError:
            continue
        if price <= 0:
            continue
        seat = SEAT_CODE.get(seg[0])
        if seat:
            out.append((seat, price))
    return out


def progress(done, total, ok, fail, t0):
    el = time.time() - t0
    speed = done / el if el > 0 else 0
    eta = (total - done) / speed if speed > 0 else 0
    pct = done * 100 // total if total else 100
    sys.stdout.write('\r[%3d%%] %d/%d  ✓%d ✗%d  %.1f/秒  已用 %s  剩余 %s   '
                     % (pct, done, total, ok, fail, speed,
                        time.strftime('%H:%M:%S', time.gmtime(el)),
                        time.strftime('%H:%M:%S', time.gmtime(eta))))
    sys.stdout.flush()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--limit', type=int, default=0, help='抓取 OD 数上限')
    ap.add_argument('--all', action='store_true', help='抓全国全部站对（19.7 万，断点续抓）')
    ap.add_argument('--ods', default='', help='自定义 OD 清单文件')
    ap.add_argument('--date', default='', help='乘车日期，默认明天')
    ap.add_argument('--min-gap', type=float, default=1.2, help='请求间隔秒（默认 1.2，勿低于 0.8）')
    ap.add_argument('--max-hours', type=float, default=0, help='本次最长运行小时数（0=不限），到点自动收尾下次续跑')
    args = ap.parse_args()

    date = args.date or time.strftime('%Y-%m-%d', time.localtime(time.time() + 86400))
    codes = load_codes()
    ods = load_ods(args)
    done = set()
    if os.path.exists(DONE_TXT):
        done = {l.strip() for l in open(DONE_TXT, encoding='utf-8') if l.strip()}
    todo = [od for od in ods if f'{od[0]}|{od[1]}' not in done]
    if args.limit:
        todo = todo[:args.limit]
    total = len(todo)
    print(f'日期 {date}｜清单 {len(ods)}｜已完成 {len(done)}｜本次待抓 {total}｜限速 {args.min_gap}s')

    cli = Client()
    cli.refresh()
    print('Cookie 已获取')

    new_file = not os.path.exists(OUT_CSV)
    fout = open(OUT_CSV, 'a', encoding='utf-8', newline='')
    w = csv.writer(fout)
    if new_file:
        w.writerow(['from_station', 'to_station', 'train_no', 'train_type', 'seat', 'price', 'date'])
    fdone = open(DONE_TXT, 'a', encoding='utf-8')

    ok = fail = skip = rows = 0
    t0 = time.time()
    deadline = t0 + args.max_hours * 3600 if args.max_hours > 0 else 0

    for i, (fr, to) in enumerate(todo):
        if deadline and time.time() > deadline:
            print('\n到达本次运行时长上限，收尾（下次运行自动续抓）')
            break
        fc, tc = codes.get(fr), codes.get(to)
        if not fc or not tc or fc == tc:
            skip += 1
            fdone.write(f'{fr}|{to}\n')
            continue

        got = False
        for attempt in range(3):
            try:
                if attempt > 0 or i % 300 == 0:
                    cli.refresh()
                cli.resolve_path(fc, tc, date)
                d = cli.query(fc, tc, date)
                res = (d.get('data') or {}).get('result') or []
                for row in res:
                    fl = row.split('|')
                    if len(fl) < 40:
                        continue
                    grade = {'G': '高铁', 'C': '城际', 'D': '动车'}.get(fl[3][:1], '普速')
                    for seat, price in parse_ypinfo(fl[39]):
                        w.writerow([fr, to, fl[3], grade, seat, round(price, 1), date])
                        rows += 1
                got = True
                break
            except Exception as e:  # noqa: BLE001
                if attempt == 2:
                    print('\n  FAIL %s→%s: %s %s' % (fr, to, type(e).__name__, str(e)[:80]))
                else:
                    time.sleep(1.5 * (attempt + 1))
                    cli.refresh()
                    cli.resolve_path(fc, tc, date)
        if got:
            ok += 1
            fdone.write(f'{fr}|{to}\n')
        else:
            fail += 1

        if (i + 1) % 5 == 0 or i + 1 == total:
            progress(i + 1, total, ok, fail, t0)
            if (i + 1) % 100 == 0:
                fout.flush()
                fdone.flush()
        time.sleep(random.uniform(args.min_gap * 0.8, args.min_gap * 1.3))

    fout.close()
    fdone.close()

    # 元信息：最近一次抓取时间与规模
    meta = {'last_fetch': time.strftime('%Y-%m-%d %H:%M:%S'),
            'date': date, 'od_done_total': len(done) + ok, 'rows_this_run': rows}
    json.dump(meta, open(META_JSON, 'w', encoding='utf-8'), ensure_ascii=False, indent=2)

    print('\n本次：成功 %d，失败 %d，跳过 %d，票价行 %d' % (ok, fail, skip, rows))
    print('输出：', OUT_CSV)
    print('元信息：', META_JSON)


if __name__ == '__main__':
    main()
