#!/bin/bash
#
# package-macos.sh — 把 urnetd 打成 macOS .app 并封装成 dmg
#
# 产物:
#   urnetd-<version>.dmg          内含 URnetwork Console.app + Applications 快捷方式
#
# 说明:
#   - urnetd 与 socksproxy 都编成 universal（arm64 + x86_64），Apple Silicon 上原生运行
#   - socksproxy 会一起塞进 app 的 Resources，装上就能用，不依赖外面那份二进制
#   - 用 ad-hoc 签名（codesign -s -）：本机自用够，别人下载首次打开需右键→打开
#   - 唯一仍需外部提供的是 URnetwork 的登录凭据（.auth_state），这是账号体系的必然
#
set -euo pipefail
cd "$(dirname "$0")"

APP_NAME="URnetwork Console"
BUNDLE_ID="com.fangjiefeng.urnetd"
VERSION="1.0.0"
DIST="dist"
APP="$DIST/$APP_NAME.app"
SOCKS_SRC="${SOCKS_SRC:-/Users/fangjiefeng/WorkBuddy/2026-11-04-13-19-01/urnet/proxy/socks}"
SOCKS_FALLBACK="$HOME/Dev/personal/tools/bin/socksproxy"

[ -f icon-1024.png ] || { echo "缺少 icon-1024.png"; exit 1; }

echo "==> 清理"
rm -rf "$DIST"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

echo "==> 编译 urnetd (universal)"
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /tmp/urnetd-amd64 .
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o /tmp/urnetd-arm64 .
lipo -create -output "$APP/Contents/MacOS/urnetd" /tmp/urnetd-amd64 /tmp/urnetd-arm64
chmod +x "$APP/Contents/MacOS/urnetd"

echo "==> 准备 socksproxy"
if [ -d "$SOCKS_SRC" ]; then
	(cd "$SOCKS_SRC" &&
		CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /tmp/socks-amd64 . &&
		CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o /tmp/socks-arm64 .)
	lipo -create -output "$APP/Contents/Resources/socksproxy" /tmp/socks-amd64 /tmp/socks-arm64
	echo "    从源码编译（universal）"
elif [ -f "$SOCKS_FALLBACK" ]; then
	cp "$SOCKS_FALLBACK" "$APP/Contents/Resources/socksproxy"
	echo "    源码不在，复用已有二进制（可能只有 x86_64）"
else
	echo "找不到 socksproxy，中止"; exit 1
fi
chmod +x "$APP/Contents/Resources/socksproxy"

# 授权码操作示意图：放一份在包内，方便替换（程序优先读这个，读不到才用编译进去的）
mkdir -p "$APP/Contents/Resources/help"
cp help/authcode.png "$APP/Contents/Resources/help/authcode.png"

echo "==> 生成图标"
rm -rf icon.iconset && mkdir -p icon.iconset
for s in 16 32 128 256 512; do
	sips -z $s $s icon-1024.png --out "icon.iconset/icon_${s}x${s}.png" >/dev/null
	sips -z $((s * 2)) $((s * 2)) icon-1024.png --out "icon.iconset/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns icon.iconset -o icon.icns
rm -rf icon.iconset
cp icon.icns "$APP/Contents/Resources/icon.icns"

echo "==> 写 Info.plist"
cat >"$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>$APP_NAME</string>
	<key>CFBundleDisplayName</key>
	<string>URnetwork 控制台</string>
	<key>CFBundleExecutable</key>
	<string>urnetd</string>
	<key>CFBundleIdentifier</key>
	<string>$BUNDLE_ID</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>$VERSION</string>
	<key>CFBundleVersion</key>
	<string>$VERSION</string>
	<key>CFBundleIconFile</key>
	<string>icon</string>
	<key>LSMinimumSystemVersion</key>
	<string>13.0</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSHighResolutionCapable</key>
	<true/>
	<key>NSHumanReadableCopyright</key>
	<string>本地工具，数据不出本机</string>
</dict>
</plist>
PLIST
plutil -lint "$APP/Contents/Info.plist" >/dev/null

echo "==> 签名（ad-hoc）"
codesign --force --deep --sign - "$APP" 2>&1 | sed 's/^/    /'
codesign --verify --deep --strict "$APP" && echo "    签名校验通过"

echo "==> 组装 dmg 内容"
DMGROOT="$DIST/dmg-root"
mkdir -p "$DMGROOT"
cp -R "$APP" "$DMGROOT/"
ln -s /Applications "$DMGROOT/Applications"
cat >"$DMGROOT/打开控制台.command" <<'CMD'
#!/bin/bash
# 双击即可在浏览器里打开控制台；没在跑就先启动 app
PORT=8877
if ! /usr/bin/nc -z 127.0.0.1 "$PORT" 2>/dev/null; then
	open -a "URnetwork Console" >/dev/null 2>&1 || true
	for _ in $(seq 1 20); do
		/usr/bin/nc -z 127.0.0.1 "$PORT" 2>/dev/null && break
		sleep 0.5
	done
fi
open "http://127.0.0.1:$PORT"
CMD
chmod +x "$DMGROOT/打开控制台.command"
cat >"$DMGROOT/使用说明.txt" <<'NOTE'
URnetwork 控制台
================

这是本地运行的代理管理面板，作用是管理 URnetwork 的出口并生成
Shadowrocket 可用的 SOCKS5 节点。

安装
----
把「URnetwork Console.app」拖到 Applications 文件夹即可。

首次打开
--------
app 没有 Apple 开发者签名（notarize），第一次打开会被 Gatekeeper 拦：
右键点 app →「打开」→ 再点「打开」。之后就不会再问了。

这个 app 没有菜单栏图标、也不占 Dock（纯后台服务）
--------------------------------------------------
所以「再打开面板」有这几种办法，任选：

  1. 双击 dmg 里的「打开控制台.command」（可以拖到桌面随手用）
  2. 浏览器收藏并访问 http://127.0.0.1:8877
  3. 再次双击 URnetwork Console.app —— 检测到已在运行会直接打开面板

怎么停
------
  面板 →「更多设置」→「停止服务并退出」
  或终端：pkill -f urnetd
  （装过开机自启的话，停掉会被自动拉起；要彻底停先执行下面的 uninstall）

使用前提
--------
1. 先去 https://ur.io/ 登录（Google 账号即可），点右上角头像 →
   ACCOUNT TOOLS → Copy Auth Code，拿到一次性授权码
2. 打开 app，会自动起服务并打开浏览器（默认 127.0.0.1:8877）
3. 第一次打开会让你设一个面板密码（自己定，至少 6 位）——这个密码只管
   本机控制台，跟 URnetwork 账号无关；忘了可以在终端执行：
     /Applications/URnetwork\ Console.app/Contents/MacOS/urnetd set-password 新密码
4. 把授权码粘到面板「换凭据」里点登录；然后选地区，用 Shadowrocket
   扫码或点「导入 Shadowrocket」

如果你机器上已经登录过 URnetwork.app，也可以直接沿用它的凭据文件
（~/Library/Containers/network.ur/.../.auth_state），不必再取授权码。

面板密码
--------
第一次打开面板时，页面会让你自己设一个密码（至少 6 位），设完直接进入。
想提前指定或日后修改：

  /Applications/URnetwork\ Console.app/Contents/MacOS/urnetd set-password 你的密码

改完旧会话立即失效，不用重启服务。

面板默认只监听 127.0.0.1，局域网访问不到。要在手机上管理（远端部署），
把监听地址改成 0.0.0.0 并务必设置密码：

  URNETD_LISTEN=0.0.0.0:8877 /Applications/URnetwork\ Console.app/Contents/MacOS/urnetd

开机自启
--------
在 app 内无法点击「安装自启」，需要打开「终端」执行：

  /Applications/URnetwork\ Console.app/Contents/MacOS/urnetd install

卸载自启同理，把 install 换成 uninstall。

注意事项
--------
- 服务只监听 127.0.0.1，局域网内其他设备访问不到
- 凭据 30 天过期，届时打开一次 URnetwork.app 刷新即可
- 也可以手动换凭据：面板「更新凭据」输入框粘贴，或
  urnetd token import <文件>；随时用 urnetd token 看剩余天数
- 别让 URnetwork.app 的 VPN 与本代理同时连着，会触发平台限流
NOTE

echo "==> 生成 dmg"
OUT="$(pwd)/urnetd-$VERSION.dmg"
rm -f "$OUT"
hdiutil create -volname "$APP_NAME" -srcfolder "$DMGROOT" -ov -format UDZO "$OUT" >/dev/null
echo
echo "完成: $OUT"
du -h "$OUT" | awk '{print "大小: "$1}'
lipo -archs "$APP/Contents/MacOS/urnetd" | sed 's/^/urnetd 架构: /'
lipo -archs "$APP/Contents/Resources/socksproxy" | sed 's/^/socksproxy 架构: /'
