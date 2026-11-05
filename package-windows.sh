#!/bin/bash
#
# package-windows.sh — 打出 Windows 版压缩包（面板 + 代理）
#
# 产物:
#   windows/urnetd-windows-amd64.zip
#
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${VERSION:-1.0.0}"
DIST="windows"
SOCKS_SRC="${SOCKS_SRC:-/Users/fangjiefeng/WorkBuddy/2026-11-04-13-19-01/urnet/proxy/socks}"

if [ ! -d "$SOCKS_SRC" ]; then
	echo "找不到 socksproxy 源码目录: $SOCKS_SRC"
	echo "设置 SOCKS_SRC 指向 urnetwork/proxy 的 socks 目录后再试"
	exit 1
fi

rm -rf "$DIST"
mkdir -p "$DIST/stage"

echo "==> 编译 urnetd.exe"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$DIST/stage/urnetd.exe" .

echo "==> 编译 socksproxy.exe"
(cd "$SOCKS_SRC" && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$OLDPWD/$DIST/stage/socksproxy.exe" .)

cat >"$DIST/stage/README.txt" <<'NOTE'
URnetwork 控制台 — Windows 版
=============================

包含
----
urnetd.exe      网页面板 + 进程管理（用它启动/停止一切）
socksproxy.exe  代理本体，由 urnetd 拉起，不要直接运行

第一次用
--------
1. 把两个 exe 放在同一个文件夹，例如 D:\urnetd
2. 双击 urnetd.exe
   - 会弹出"Windows 已保护你的电脑"就点"更多信息 → 仍要运行"
   - 它会起服务并打开浏览器（默认 http://127.0.0.1:8877）
3. 第一次打开会让你设置面板密码（至少 6 位）
4. 拿凭据：浏览器登录 https://ur.io/ → 右上角头像 → ACCOUNT TOOLS →
   Copy Auth Code，把这串一次性授权码粘到面板「节点 → 换凭据」里，
   60 秒内点「用授权码登录」

开机自启
--------
在 urnetd.exe 所在目录打开 PowerShell 或 cmd：

  urnetd.exe install      写入 HKCU\...\Run，并立即启动
  urnetd.exe uninstall    移除自启

客户端
------
Windows 上没有 Shadowrocket，用 Clash Verge / FlClash：
面板「节点」页点「导入 Clash Verge」或「导入 FlClash」即可；
也可以手动把订阅地址填进去：
  http://127.0.0.1:8877/sub/<token>.yaml
订阅地址可以在面板点按钮时看到，或看命令行 urnetd.exe token。

常见问题
--------
- 端口被占用：关掉旧的 urnetd.exe / socksproxy.exe，或在任务管理器里结束进程
- 面板打不开：确认没被防火墙拦，默认只监听 127.0.0.1
- 凭据过期：重新拿一次授权码
NOTE

echo "==> 打包 zip"
OUT="$DIST/urnetd-windows-amd64-$VERSION.zip"
rm -f "$DIST/urnetd-windows-amd64.zip" "$OUT"
(cd "$DIST/stage" && zip -qr "../urnetd-windows-amd64.zip" .)
rm -rf "$DIST/stage"

echo
echo "完成: $DIST/urnetd-windows-amd64.zip"
ls -lh "$DIST/urnetd-windows-amd64.zip" | awk '{print "大小: "$5}'
