#!/bin/bash
#
# release.sh — 一条命令出包并发版（GitHub Releases）
#
# 用法:
#   ./release.sh 1.0.1              打包 → 打 tag → 建 Release → 上传三个安装包
#   ./release.sh 1.0.1 --dry-run    只打包，不动 git 也不发版
#
# 做三件事:
#   1. 调 package-macos.sh / package-linux.sh，生成 dmg 和两个 tar.gz
#   2. git tag -a v<版本>，推到远端
#   3. gh release create，把安装包作为 assets 传上去（Release 资产不进 git 历史）
#
set -euo pipefail
cd "$(dirname "$0")"

V="${1:-}"
DRY=""
[ "${2:-}" = "--dry-run" ] && DRY=1

if [ -z "$V" ]; then
	echo "用法: ./release.sh <版本号> [--dry-run]"
	echo "例如: ./release.sh 1.0.1"
	exit 1
fi
case "$V" in
v*) V="${V#v}" ;; # 允许写 v1.0.1
esac

DMG="urnetd-$V.dmg"
TAR1="linux/urnetd-linux-amd64.tar.gz"
TAR2="linux/urnetd-linux-arm64.tar.gz"

echo "==> 版本 $V"
if [ -n "$(git status --porcelain)" ]; then
	echo "工作区有未提交改动，先把它们提交掉（Release 基于 tag，最好干净）："
	git status --short
	exit 1
fi

echo "==> 打包"
VERSION="$V" ./package-macos.sh | tail -2
VERSION="$V" ./package-linux.sh | tail -2

for f in "$DMG" "$TAR1" "$TAR2"; do
	[ -f "$f" ] || { echo "缺少产物: $f"; exit 1; }
done

if [ -n "$DRY" ]; then
	echo
	echo "dry-run：产物已生成，未打 tag、未发版"
	shasum -a 256 "$DMG" "$TAR1" "$TAR2"
	exit 0
fi

echo "==> 打 tag"
git tag -a "v$V" -m "v$V" 2>/dev/null || echo "    tag v$V 已存在，跳过"
git push origin "v$V" 2>/dev/null || echo "    tag 已在远端"

echo "==> 发版并上传"
gh release create "v$V" \
	--title "v$V" \
	--generate-notes \
	"$DMG" "$TAR1" "$TAR2"

echo
echo "完成: https://github.com/jiema123/URnetworkProxy/releases/tag/v$V"
