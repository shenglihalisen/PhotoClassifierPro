#!/usr/bin/env bash
# 把本仓库（Pro，预览通道）的共享代码同步到另外两个发布通道。
#
# 用法:
#   ./scripts/sync-siblings.sh /path/to/repos
#
# repos 目录下应有 PhotoClassifierPro/ photo-classifier/ photo-classifier-stable/
# 三个 clone。只覆盖共享源码，不动各仓独有的 Dockerfile / CI / README 定位段。
set -euo pipefail

ROOT="${1:?用法: sync-siblings.sh <repos 根目录>}"
SRC="$ROOT/PhotoClassifierPro"
[ -d "$SRC/.git" ] || { echo "找不到 $SRC"; exit 1; }

ITEMS=(qc face engine web cmd main.go csv.go gui_windows.go gui_other.go
       go.mod go.sum build-windows.bat .github/workflows/test.yml)

for R in photo-classifier photo-classifier-stable; do
  DST="$ROOT/$R"
  [ -d "$DST/.git" ] || { echo "跳过（不是 git 仓库）: $DST"; continue; }
  echo "==> $R"
  for item in "${ITEMS[@]}"; do
    mkdir -p "$(dirname "$DST/$item")"
    rm -rf "${DST:?}/$item"
    cp -r "$SRC/$item" "$DST/$item"
  done
  # module 路径、程序显示名、版本号
  find "$DST" -name '*.go' -type f -exec \
    sed -i "s|github.com/shenglihalisen/PhotoClassifierPro|github.com/shenglihalisen/$R|g" {} +
  find "$DST" -name '*.go' -type f -exec \
    sed -i "s|PhotoClassifierPro|$R|g" {} +
  sed -i "s|module github.com/shenglihalisen/PhotoClassifierPro|module github.com/shenglihalisen/$R|" "$DST/go.mod"
  sed -i "s|PhotoClassifierPro|$R|g" "$DST/build-windows.bat"
  # Pro 的版本带 -go 后缀，另外两仓不带
  sed -i 's|const version = "v\(.*\)-go"|const version = "v\1"|' "$DST/main.go"
  echo "    同步完成，请自行 go test ./... 后提交"
done
