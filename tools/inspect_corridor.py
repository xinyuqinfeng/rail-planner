# -*- coding: utf-8 -*-
"""聚焦探查：车站表结构、车次基础表、以及汉中/广元/重庆走廊的车次覆盖情况"""
import os
import sqlite3

EXT = r'D:\AI project\rail-planner\data\raw\extracted'


def show(path, title, limit=3):
    print('=' * 70)
    print(title, '->', path)
    if not os.path.exists(path):
        print('  !! missing')
        return None
    con = sqlite3.connect(path)
    cur = con.cursor()
    tables = [r[0] for r in cur.execute(
        "SELECT name FROM sqlite_master WHERE type='table'").fetchall()]
    print(f'  table_count={len(tables)}  first5={tables[:5]}')
    if tables:
        t = tables[0] if tables[0] != 'sqlite_sequence' else tables[1]
        cols = cur.execute(f'PRAGMA table_info("{t}")').fetchall()
        print(f'  [{t}] cols: ' + ', '.join(c[1] for c in cols))
        for row in cur.execute(f'SELECT * FROM "{t}" LIMIT {limit}'):
            print('   sample:', row)
    return con, tables


# 1) 车站码表
show(os.path.join(EXT, 'station_based_code.db'), '车站码表')
# 2) 车次基础表
show(os.path.join(EXT, 'train_basic.db'), '车次基础表')
# 3) 高铁索引表
show(os.path.join(EXT, 'G', 'full_code_index.db'), 'G级索引表')

# 4) 走廊站点覆盖统计：在各等级 timetable 中查找关键站
CORRIDOR = ['汉中', '广元', '重庆北', '重庆西', '成都东', '西安北', '安康', '南充北', '剑门关', '朝天']
print('=' * 70)
print('走廊站点覆盖（各等级 timetable 中含该站的车次数）')
for grade in ['G', 'D', 'K', 'C', 'T', 'Z', 'P', 'S', 'Y']:
    p = os.path.join(EXT, grade, 'train_timetable_info.db')
    if not os.path.exists(p):
        continue
    con = sqlite3.connect(p)
    cur = con.cursor()
    tables = [r[0] for r in cur.execute(
        "SELECT name FROM sqlite_master WHERE type='table'").fetchall()]
    hit = {s: 0 for s in CORRIDOR}
    for t in tables:
        try:
            rows = cur.execute(
                f'SELECT DISTINCT station_name FROM "{t}"').fetchall()
        except Exception:
            continue
        names = {r[0] for r in rows}
        for s in CORRIDOR:
            if s in names:
                hit[s] += 1
    print(f'  [{grade}] trains={len(tables):5d}  ' +
          ' '.join(f'{k}={v}' for k, v in hit.items() if v))
    con.close()
