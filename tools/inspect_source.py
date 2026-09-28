# -*- coding: utf-8 -*-
"""探查 12306 数据集内部结构（仅用于摸清字段，正式导入由 Go 版 raildata 完成）"""
import os
import sqlite3
import zipfile

RAW = r'D:\AI project\rail-planner\data\raw'
EXT = os.path.join(RAW, 'extracted')

if not os.path.isdir(EXT):
    with zipfile.ZipFile(os.path.join(RAW, 'db.zip')) as z:
        z.extractall(EXT)
    print('extracted ->', EXT)

TARGETS = [
    'station_based_code.db',
    'train_basic.db',
    'G/full_code_index.db',
    'G/train_timetable_info.db',
    'K/train_timetable_info.db',
]

for rel in TARGETS:
    path = os.path.join(EXT, rel.replace('/', os.sep))
    print('=' * 70)
    print('DB:', rel)
    if not os.path.exists(path):
        print('  !! missing')
        continue
    con = sqlite3.connect(path)
    cur = con.cursor()
    tables = [r[0] for r in cur.execute(
        "SELECT name FROM sqlite_master WHERE type='table'").fetchall()]
    print('  tables:', tables)
    for t in tables:
        cols = cur.execute(f'PRAGMA table_info("{t}")').fetchall()
        print(f'  -- {t}: ' + ', '.join(f'{c[1]}({c[2]})' for c in cols))
        n = cur.execute(f'SELECT COUNT(*) FROM "{t}"').fetchone()[0]
        print(f'     rows={n:,}')
        for row in cur.execute(f'SELECT * FROM "{t}" LIMIT 3'):
            print('     sample:', row)
    con.close()
