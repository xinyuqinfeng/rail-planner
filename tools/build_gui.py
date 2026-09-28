# -*- coding: utf-8 -*-
"""构建 GUI 版单文件程序：把离线数据库与前端一起内嵌进 exe，双击即可运行。

用法：python tools/build_gui.py
产物：dist/railplan-gui.exe
"""
import hashlib
import os
import shutil
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GO = os.environ.get('GOEXE', r'C:\Users\29782\.workbuddy\binaries\go-tmp\go\bin\go.exe')
DB = os.path.join(ROOT, 'data', 'rail.db')
ASSETS = os.path.join(ROOT, 'cmd', 'railplan-gui', 'assets')
DIST = os.path.join(ROOT, 'dist')

ENV = dict(os.environ,
           GOPROXY='https://goproxy.cn,direct',
           GOPATH=r'C:\Users\29782\.workbuddy\binaries\go-tmp\gopath',
           GOMODCACHE=r'C:\Users\29782\.workbuddy\binaries\go-tmp\gopath\pkg\mod',
           GOCACHE=r'C:\Users\29782\.workbuddy\binaries\go-tmp\gocache',
           GOTOOLCHAIN='local')


def main():
    if not os.path.exists(DB):
        sys.exit(f'未找到数据库 {DB}，请先执行 raildata 导入')
    if not os.path.exists(GO):
        sys.exit(f'未找到 go 工具链 {GO}')

    os.makedirs(ASSETS, exist_ok=True)
    os.makedirs(DIST, exist_ok=True)

    dst = os.path.join(ASSETS, 'rail.db')
    shutil.copy2(DB, dst)
    print(f'内嵌数据库：{os.path.getsize(dst) / 1024 / 1024:.2f} MB')

    # 用内容哈希做版本标识：运行时会按此哈希命名缓存文件，
    # 避免新旧数据库体积相近时被误判为同一版本而复用旧缓存。
    h = hashlib.sha256(open(dst, 'rb').read()).hexdigest()[:12]
    with open(os.path.join(ASSETS, 'version.txt'), 'w', encoding='utf-8') as f:
        f.write(h)
    print(f'数据库版本标识：{h}')

    out = os.path.join(DIST, 'railplan-gui.exe')
    r = subprocess.run([GO, 'build', '-ldflags', '-s -w', '-o', out, './cmd/railplan-gui'],
                       cwd=ROOT, env=ENV, capture_output=True, text=True, encoding='utf-8', errors='replace')
    if r.returncode != 0:
        print(r.stdout or '')
        print(r.stderr or '')
        sys.exit('编译失败')
    print(f'构建成功：{out}  ({os.path.getsize(out) / 1024 / 1024:.2f} MB)')
    print('双击该 exe 即可启动（自动打开浏览器）')


if __name__ == '__main__':
    main()
