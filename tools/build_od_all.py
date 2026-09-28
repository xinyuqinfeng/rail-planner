# -*- coding: utf-8 -*-
"""统计全路网「有直达车次的车站对」规模，生成按热度排序的全国 OD 清单"""
import sqlite3
import sys
import time
from collections import Counter
from itertools import combinations

sys.stdout.reconfigure(encoding='utf-8')
DB = r'D:\AI project\rail-planner\data\rail.db'
OUT = r'D:\AI project\rail-planner\data\raw\od_all.txt'

t0 = time.time()
con = sqlite3.connect(DB)
cur = con.cursor()
rows = cur.execute('SELECT full_code, stop_seq, station_name FROM stop_schedule ORDER BY full_code, stop_seq').fetchall()
con.close()
print('经停记录:', len(rows))

# 按车次分组，组内两两组合
by_train = {}
for fc, seq, name in rows:
    by_train.setdefault(fc, []).append(name)

cnt = Counter()
for fc, stops in by_train.items():
    uniq = list(dict.fromkeys(stops))  # 去重保序
    if len(uniq) < 2:
        continue
    for a, b in combinations(uniq, 2):
        cnt[(a, b)] += 1

print('有直达车次的站对数:', len(cnt))
print('耗时 %.1fs' % (time.time() - t0))

with open(OUT, 'w', encoding='utf-8') as f:
    for (a, b), n in cnt.most_common():
        f.write(f'{a},{b},{n}\n')
print('已写出(按车次数降序) →', OUT)
print()
print('Top 10 热门站对:')
for (a, b), n in cnt.most_common(10):
    print('  %s→%s  %d 趟' % (a, b, n))
