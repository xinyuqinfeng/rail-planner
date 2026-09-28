#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""一键预置数据：抓取 → 建库 → 打包单文件 GUI。

首次使用执行一次即可（约 20 分钟，全程支持断点续抓，中断后重跑会接着来）：

    python tools/setup_data.py

完成后：
    dist/railplan-gui.exe   双击即用（数据库已内嵌，运行时零网络请求）
    bin/railplan.exe        命令行查询
"""
import os
import shutil
import subprocess
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
os.chdir(ROOT)

DATA_DB = os.path.join('data', 'rail.db')
DETAIL_CSV = os.path.join('data', 'csv', 'stop_detail.csv')
OD_CSV = os.path.join('data', 'csv', 'od_fare.csv')
OD12306_CSV = os.path.join('data', 'csv', 'od_fare_12306.csv')


def log(msg):
    print('\n' + '=' * 62)
    print('  ' + msg)
    print('=' * 62, flush=True)


def run(args, shell=False, check=True):
    r = subprocess.run(args, shell=shell)
    if check and r.returncode != 0:
        print('\n[失败] 命令返回 %d：%s' % (r.returncode, args))
        print('可修复后重新执行本脚本（已抓取的数据会保留，不会重复抓）')
        sys.exit(1)
    return r.returncode


def find_go():
    """定位 go 可执行文件：PATH → GOROOT → 常见安装目录"""
    exe = shutil.which('go')
    if exe:
        return exe
    name = 'go.exe' if os.name == 'nt' else 'go'
    cands = []
    gr = os.environ.get('GOROOT')
    if gr:
        cands.append(os.path.join(gr, 'bin', name))
    cands += [
        r'C:\Program Files\Go\bin\go.exe',
        r'C:\Go\bin\go.exe',
        os.path.join(os.environ.get('LOCALAPPDATA', ''), 'Programs', 'Go', 'bin', 'go.exe'),
        os.path.expanduser('~/go/bin/go.exe'),
        '/usr/local/go/bin/go',
    ]
    for c in cands:
        if c and os.path.exists(c):
            return c
    return None


def main():
    python = sys.executable
    go = find_go()
    print('铁路中转路径规划工具 —— 数据预置')
    print('工程目录：%s' % ROOT)

    # ── 0) 环境检查 ──
    log('0/5  环境检查')
    print('  Go   :', go if go else '未找到（构建数据库与 exe 需要，请安装 https://go.dev/dl/）')
    print('  Python:', sys.version.split()[0])

    if not os.path.isdir('data/csv'):
        os.makedirs('data/csv', exist_ok=True)
    if not os.path.isdir('bin'):
        os.makedirs('bin', exist_ok=True)

    # ── 1) 车站电报码表（单次请求） ──
    if os.path.exists(os.path.join('data', 'csv', 'station_code.csv')):
        print('\n1/5  车站码表已存在，跳过')
    else:
        log('1/5  抓取车站电报码表（单次请求）')
        if not go:
            print('跳过：需要 Go 才能执行 -fetch-stationcodes')
        else:
            run([go, 'run', './cmd/raildata', '-fetch-stationcodes'])

    # ── 2) 抓取车次详情（唯一的批量联网步骤，支持断点续抓） ──
    log('2/5  抓取车次详情（约 20 分钟，可中断后重跑续抓）')
    if os.path.exists(DETAIL_CSV):
        print('  已存在 %s，如需重新抓取请先删除该文件' % DETAIL_CSV)
    else:
        run([python, os.path.join('tools', 'fetch_fares.py')])

    # ── 3) 构建本地数据库 ──
    log('3/5  构建本地数据库（完全离线）')
    if not go:
        print('中止：构建数据库需要 Go 环境（装好后重跑本脚本即可，抓取结果会保留）')
        sys.exit(1)
    run([go, 'build', '-o', os.path.join('bin', 'raildata.exe'), './cmd/raildata'], check=False)
    raildata = os.path.join('bin', 'raildata.exe')
    if not os.path.exists(raildata):
        raildata = os.path.join('bin', 'raildata')
    args = [raildata, '-details', DETAIL_CSV, '-db', DATA_DB]
    if os.path.exists(OD_CSV):
        args += ['-odfare', OD_CSV]
    if os.path.exists(OD12306_CSV):
        args += ['-odfare12306', OD12306_CSV]
    run(args)

    # ── 4) 编译命令行版 ──
    log('4/5  编译命令行版')
    run([go, 'build', '-o', os.path.join('bin', 'railplan.exe'), './cmd/railplan'], check=False)

    # ── 5) 打包单文件 GUI ──
    log('5/5  打包单文件 GUI（把数据库内嵌进 exe）')
    run([python, os.path.join('tools', 'build_gui.py')])

    print('\n' + '=' * 62)
    print('  完成！')
    print('  图形界面：dist/railplan-gui.exe（双击即用，运行时零网络请求）')
    print('  命令行  ：bin/railplan.exe -from 出发地 -to 目的地')
    print('=' * 62)
    print('提示：想进一步提升票价精度，可执行')
    print('      python tools/fetch_12306_fare.py --limit 3000')
    print('      直连 12306 官方接口补抓真实区间票价（低频友善抓取，约 1 小时）')


if __name__ == '__main__':
    t0 = time.time()
    main()
    print('\n总耗时 %.1f 分钟' % ((time.time() - t0) / 60))
