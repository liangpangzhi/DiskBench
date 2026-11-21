#!/usr/bin/env python3
"""
Disk Bench FPK 打包器（二次开发）
================================
复现 fnOS fnpack 的打包格式，用于在 fnpack 在本机不可用时生成可安装的 .fpk。

FPK 结构（经原始 diskbench-x86_64-0.1.36.fpk 逆向确认）：
  - gzip(mtime=0, XFL=0, OS=255) 包裹的 GNU tar (format=2)
  - 外层成员：app.tgz + 根目录文件(LICENSE/ICON/ICON_256/manifest/cmd/config/wizard)
  - app.tgz 内容：package/diskbench/app/ 下的全部内容，路径去掉 "app/" 前缀
    （即 server/、ui/、config/ 三个顶层目录）
  - manifest 的 checksum = app.tgz 的 MD5

用法：
  python build_fpk.py <源打包目录> <输出.fpk>
"""
import os
import sys
import io
import gzip
import tarfile
import hashlib

GNU_FORMAT = tarfile.GNU_FORMAT


def make_tar_gz(entries, mtime=0):
    """entries: list of (arcname, disk_path, is_dir). 返回 gzip 后的 bytes。
    tar 成员 mtime 取磁盘文件实际修改时间（与 fnpack 行为一致）；
    gzip 头 mtime 固定为 0。"""
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w", format=GNU_FORMAT) as tar:
        for arcname, disk_path, is_dir in entries:
            ti = tarfile.TarInfo(name=arcname)
            try:
                ti.mtime = int(os.path.getmtime(disk_path))
            except OSError:
                ti.mtime = mtime
            ti.uid = 0
            ti.gid = 0
            ti.uname = ""
            ti.gname = ""
            if is_dir:
                ti.type = tarfile.DIRTYPE
                ti.mode = 0o777
                ti.size = 0
                tar.addfile(ti)
            else:
                data = open(disk_path, "rb").read()
                ti.type = tarfile.REGTYPE
                ti.mode = 0o666
                ti.size = len(data)
                tar.addfile(ti, io.BytesIO(data))
    # Go gzip.Writer: mtime=0, XFL=0, OS=255
    buf = io.BytesIO()
    with gzip.GzipFile(fileobj=buf, mode="wb", filename="", mtime=0, compresslevel=9) as gz:
        gz.write(raw.getvalue())
    out = bytearray(buf.getvalue())
    out[8] = 0   # XFL
    out[9] = 255  # OS unknown
    return bytes(out)


def collect_dir(base, prefix="", skip_names=()):
    """收集目录下所有条目，返回按 arcname 排序的 (arcname, path, is_dir) 列表。"""
    entries = []
    for root, dirs, files in os.walk(base):
        rel_root = os.path.relpath(root, base).replace("\\", "/")
        if rel_root == ".":
            rel_root = ""
        # 目录条目（跳过 skip）
        dirs[:] = sorted(d for d in dirs if d not in skip_names)
        for d in sorted(dirs):
            arc = f"{prefix}{rel_root}/{d}" if rel_root else f"{prefix}{d}"
            entries.append((arc, os.path.join(root, d), True))
        for f in sorted(files):
            if f in skip_names:
                continue
            arc = f"{prefix}{rel_root}/{f}" if rel_root else f"{prefix}{f}"
            entries.append((arc, os.path.join(root, f), False))
    entries.sort(key=lambda e: e[0])
    return entries


def main():
    src = os.path.abspath(sys.argv[1])
    out = os.path.abspath(sys.argv[2])
    app_dir = os.path.join(src, "app")

    # ---- 1. 生成 app.tgz（打包 app/ 内容，无前缀）----
    app_entries = collect_dir(app_dir, prefix="")
    app_tgz = make_tar_gz(app_entries)
    app_tgz_path = os.path.join(src, "app.tgz")
    with open(app_tgz_path, "wb") as f:
        f.write(app_tgz)
    md5 = hashlib.md5(app_tgz).hexdigest()
    print(f"app.tgz: {len(app_tgz)} bytes, {len(app_entries)} entries, md5={md5}")

    # ---- 2. 更新 manifest 的 checksum ----
    manifest_path = os.path.join(src, "manifest")
    manifest = open(manifest_path, encoding="utf-8").read()
    import re
    if re.search(r"^checksum=.*$", manifest, re.M):
        manifest = re.sub(r"^checksum=.*$", f"checksum={md5}", manifest, flags=re.M)
    else:
        manifest = manifest.rstrip("\n") + f"\nchecksum={md5}\n"
    with open(manifest_path, "w", encoding="utf-8", newline="\n") as f:
        f.write(manifest)
    print("manifest checksum updated")

    # ---- 3. 生成外层 FPK ----
    outer_entries = []
    # app.tgz 放最前（与原始一致）
    outer_entries.append(("app.tgz", app_tgz_path, False))
    # 根目录普通文件
    for name in sorted(os.listdir(src)):
        if name in ("app", "app.tgz"):
            continue
        p = os.path.join(src, name)
        if os.path.isfile(p):
            outer_entries.append((name, p, False))
    # 根目录子目录（cmd/config/wizard），按名称排序
    for name in sorted(os.listdir(src)):
        p = os.path.join(src, name)
        if os.path.isdir(p) and name != "app":
            for arc, dp, isdir in collect_dir(p, prefix=name + "/"):
                outer_entries.append((arc, dp, isdir))
    # 按 arcname 排序，但确保 app.tgz 在前（原始顺序：app.tgz, LICENSE, cmd/, ...）
    outer_entries.sort(key=lambda e: (e[0] != "app.tgz", e[0]))

    fpk = make_tar_gz(outer_entries)
    with open(out, "wb") as f:
        f.write(fpk)
    print(f"FPK written: {out} ({len(fpk)} bytes, {len(outer_entries)} entries)")

    # 校验：能重新打开且关键文件存在
    with tarfile.open(out, "r:gz") as tf:
        names = tf.getnames()
        for required in ("app.tgz", "manifest", "cmd/main", "ICON.PNG"):
            assert required in names, f"missing {required} in FPK"
    print("FPK verification OK")


if __name__ == "__main__":
    main()
