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
urnetd.exe        网页面板 + 进程管理（用它启动/停止一切）
socksproxy.exe    代理本体，由 urnetd 拉起，不要直接运行
1-start.bat             双击启动（会先检查两个 exe 在不在）
2-fix-path.bat          两个 exe 放散了？双击它自动找回来放到一起
3-install-autostart.bat 写注册表 Run 项，登录后自动启动
4-uninstall-autostart.bat 移除自启
5-diagnose.bat          出问题时双击它，会打印目录、端口和启动日志

第一次用
--------
1. 把两个 exe 放在同一个文件夹，例如 D:\urnetd（放散了就双击 2-fix-path.bat）
2. 双击 1-start.bat 或 urnetd.exe
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

# ---------- 辅助 bat ----------
cat >"$DIST/stage/1-start.bat" <<'BAT'
@echo off
chcp 65001 >nul
title URnetwork 控制台 — 启动
cd /d "%~dp0"
if not exist "urnetd.exe" (
  echo [错误] 当前目录没有 urnetd.exe，请把 bat 和两个 exe 放在同一个文件夹。
  pause & exit /b 1
)
if not exist "socksproxy.exe" (
  echo [错误] 当前目录没有 socksproxy.exe。
  echo        请先双击 2-fix-path.bat，或把 socksproxy.exe 拖到本文件夹。
  pause & exit /b 1
)
start "" http://127.0.0.1:8877
echo 服务启动中，面板会自动打开；关掉这个窗口就等于停止服务。
urnetd.exe
echo.
echo 服务已退出。按任意键关闭。
pause
BAT

cat >"$DIST/stage/2-fix-path.bat" <<'BAT'
@echo off
chcp 65001 >nul
title URnetwork — 修复路径
cd /d "%~dp0"
echo ===== 检查当前文件夹 =====
if exist "urnetd.exe" (echo   urnetd.exe      存在) else (echo   urnetd.exe      缺失)
if exist "socksproxy.exe" (echo   socksproxy.exe  存在) else (echo   socksproxy.exe  缺失)

if not exist "socksproxy.exe" (
  echo.
  echo 正在当前目录及上下级里找 socksproxy.exe ...
  for /r "%~dp0" %%f in (socksproxy.exe) do (
    echo   找到: %%f
    copy /y "%%f" "%~dp0socksproxy.exe" >nul && echo   已复制到本文件夹
    goto :legacy
  )
  echo   没找到。请重新解压完整的 zip 包。
)

:legacy
rem 兼容早期版本：老 exe 会去找 %USERPROFILE%\Dev\personal\tools\bin\socksproxy.exe
if exist "%USERPROFILE%\Dev\personal\tools\bin\" (
  if exist "%~dp0socksproxy.exe" (
    copy /y "%~dp0socksproxy.exe" "%USERPROFILE%\Dev\personal\tools\bin\socksproxy.exe" >nul
    echo.
    echo 检测到你机器上有旧版目录，已把 socksproxy.exe 复制过去，
    echo 这样旧版 urnetd.exe 也能用。
  )
)
echo.
echo ===== 结果 =====
if exist "socksproxy.exe" (echo 现在可以直接双击 1-start.bat 了) else (echo 仍然缺少 socksproxy.exe)
pause
BAT

cat >"$DIST/stage/3-install-autostart.bat" <<'BAT'
@echo off
chcp 65001 >nul
title URnetwork — 安装开机自启
cd /d "%~dp0"
if not exist "socksproxy.exe" (
  echo 先双击 2-fix-path.bat 把 socksproxy.exe 放好，再装自启。
  pause & exit /b 1
)
urnetd.exe install
echo.
pause
BAT

cat >"$DIST/stage/4-uninstall-autostart.bat" <<'BAT'
@echo off
chcp 65001 >nul
title URnetwork — 卸载开机自启
cd /d "%~dp0"
urnetd.exe uninstall
echo.
pause
BAT

cat >"$DIST/stage/5-diagnose.bat" <<'BAT'
@echo off
chcp 65001 >nul
title URnetwork — 诊断
cd /d "%~dp0"
echo ===== 文件夹内容 =====
dir /b
echo.
echo ===== 端口占用（8877 面板 / 9999 代理）=====
netstat -ano | findstr ":8877 :9999"
echo.
echo ===== 启动 6 秒，抓启动日志 =====
taskkill /f /im urnetd.exe >nul 2>&1
start /b "" cmd /c "urnetd.exe > diag.log 2>&1"
timeout /t 6 /nobreak >nul
echo --- diag.log ---
if exist diag.log type diag.log
echo.
echo ===== 收尾：关掉刚才启动的进程 =====
taskkill /f /im urnetd.exe >nul 2>&1
taskkill /f /im socksproxy.exe >nul 2>&1
echo.
echo 把上面的文字截图发给作者即可（里面有路径和报错原因）。
pause
BAT

# 记事本需要 BOM 才认 UTF-8
printf '\xef\xbb\xbf' >"$DIST/stage/README.txt.tmp"
cat "$DIST/stage/README.txt" >>"$DIST/stage/README.txt.tmp"
mv "$DIST/stage/README.txt.tmp" "$DIST/stage/README.txt"

# bat 必须是 CRLF，否则 cmd 里中文/标签可能出问题
for f in "$DIST/stage"/*.bat; do
	perl -pi -e 's/\r?\n/\r\n/' "$f"
done

echo "==> 打包 zip"
OUT="$DIST/urnetd-windows-amd64-$VERSION.zip"
rm -f "$DIST/urnetd-windows-amd64.zip" "$OUT"
(cd "$DIST/stage" && zip -qr "../urnetd-windows-amd64.zip" .)
rm -rf "$DIST/stage"

echo
echo "完成: $DIST/urnetd-windows-amd64.zip"
ls -lh "$DIST/urnetd-windows-amd64.zip" | awk '{print "大小: "$5}'
