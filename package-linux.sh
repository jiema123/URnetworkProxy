#!/bin/bash
#
# package-linux.sh — 打出 Linux 版压缩包（含面板与代理两个二进制）
#
# 产物:
#   linux/urnetd-linux-amd64.tar.gz
#   linux/urnetd-linux-arm64.tar.gz
#
# 包内:
#   urnetd        面板 + 进程管理（自带 install/uninstall，写 systemd user unit）
#   socksproxy    代理本体
#   socksproxy.service  可选的 systemd 模板（不想用 urnetd 托管时用）
#   说明.txt
#
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${VERSION:-1.0.0}"
DIST="linux"
SOCKS_SRC="${SOCKS_SRC:-/Users/fangjiefeng/WorkBuddy/2026-11-04-13-19-01/urnet/proxy/socks}"
SOCKS_FALLBACK="$HOME/Dev/personal/tools/bin/socksproxy"

rm -rf "$DIST"
mkdir -p "$DIST"

for arch in amd64 arm64; do
	echo "==> 编译 linux/$arch"
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "/tmp/urnetd-l-$arch" .

	STAGE="$DIST/stage-$arch"
	mkdir -p "$STAGE"
	cp "/tmp/urnetd-l-$arch" "$STAGE/urnetd"

	if [ -d "$SOCKS_SRC" ]; then
		(cd "$SOCKS_SRC" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "/tmp/socks-l-$arch" .)
		cp "/tmp/socks-l-$arch" "$STAGE/socksproxy"
	elif [ -f "$SOCKS_FALLBACK" ]; then
		echo "    源码不在，socksproxy 为 darwin 版本，仅占位（需自行替换）"
		cp "$SOCKS_FALLBACK" "$STAGE/socksproxy.placeholder"
	fi

	mkdir -p "$STAGE/help"
	cp help/authcode.png "$STAGE/help/authcode.png"

	cat >"$STAGE/说明.txt" <<'NOTE'
URnetwork 控制台 — Linux 版
===========================

包含两个程序
------------
urnetd       网页面板 + 进程管理，自带启停与开机自启
socksproxy   代理本体，由 urnetd 拉起，一般不直接运行

放置
----
建议放到同一目录，例如 /opt/urnetd：

  sudo mkdir -p /opt/urnetd
  sudo tar -xzf urnetd-linux-amd64.tar.gz -C /opt/urnetd
  sudo chmod +x /opt/urnetd/urnetd /opt/urnetd/socksproxy

前提（重要）
------------
本程序不含登录能力，凭据来自 URnetwork 官方 App 写出的 .auth_state。
Linux 上需要你把该文件放到下面任一位置，并设置环境变量指向它：

  export URNET_AUTH_STATE=/path/to/.auth_state

做法：在 macOS / iOS 上登录 URnetwork 后，从
  ~/Library/Containers/network.ur/Data/Documents/network_spaces/ur.network/main/.by/.auth_state
拷贝过来即可。凭据 30 天过期，到期需重新拷一次。

更省事的三种方式
----------------
1) 授权码（最推荐，不需要密码、不用拷文件）
   在 URnetwork App 里：Account Tools → Copy Auth Code，
   得到一串一次性授权码（1 分钟有效、只能用一次），然后：

     urnetd token code            # 粘贴授权码回车
     urnetd token code -          # 或从标准输入读
     echo 授权码 | urnetd token code -

   面板上也有「用授权码登录」输入框，粘完立刻点。

2) 面板上直接粘贴 JWT：打开网页 →「凭据与地址」→「凭据 JWT」，
   把 JWT 或整个 .auth_state 内容贴进去，点更新（会自动重启代理）。

3) 命令行导入文件：

     urnetd token show                     # 在有 App 的机器上导出 token
     urnetd token import /path/to/.auth_state
     urnetd token                          # 查看有效期

凭据文件变动后，urnetd 会在 30 秒内自动重启代理使之生效。
每次写入前会把旧文件备份成 .auth_state.bak，方便回滚。

如果你的 URnetwork 账号有密码（不是 Google/Apple 登录）
-------------------------------------------------------
那就完全不需要 macOS 了，直接用官方接口换 token：

     urnetd token login 账号@example.com
     # 或： echo 密码 | urnetd token login 账号@example.com -

面板上也有对应输入框（「账号登录」+「记住账号密码」）。
勾选记住后，会在凭据快过期时（剩 5 天内）自动续期，每 6 小时检查一次。

启动
----
  /opt/urnetd/urnetd            # 前台，默认 http://127.0.0.1:8877

面板密码
--------
本机用：第一次打开网页会让你自己设一个密码（至少 6 位），设完直接进入。
服务器用（面板可能监听在 0.0.0.0，网页端无法自助设置，需先指定）：

  /opt/urnetd/urnetd set-password 你的密码

改完旧会话立即失效，不用重启服务。

远端部署：手机要访问面板和节点，需要
  1) 面板监听对外地址：URNETD_LISTEN=0.0.0.0:8877（必须已设密码，否则拒绝访问）
  2) 节点对外地址填公网 IP 或域名，面板上有个「对外地址」输入框，
     填 auto 会自动查询公网 IP
  3) 云厂商安全组/防火墙放行 8877（面板）与 9999（节点）

注意：SOCKS5 的账号密码是明文传输的，把 9999 直接暴露到公网并不安全。
更稳的做法是只开 8877 面板，节点走 SSH 隧道或 WireGuard。

开机自启（systemd user unit）
-----------------------------
  /opt/urnetd/urnetd install

它会写 ~/.config/systemd/user/urnetd.service 并 enable --now。
如果机器没有图形登录会话，需要开启 linger 让服务常驻：

  sudo loginctl enable-linger $USER

卸载：

  /opt/urnetd/urnetd uninstall

客户端
------
Linux 上没有 Shadowrocket，用 Clash / mihomo / sing-box 等，
把节点填成 SOCKS5 即可：

  服务器 127.0.0.1（或本机局域网 IP）
  端口   9999
  用户名 urnet
  密码   见面板上的 URI

分流交给这些客户端做，和 Shadowrocket 的规则逻辑一样。
手机端仍然用二维码扫码导入 Shadowrocket。
NOTE

	cat >"$STAGE/socksproxy.service" <<'UNIT'
# 可选：不用 urnetd 托管、只想跑代理时使用
#   sudo cp socksproxy.service /etc/systemd/system/
#   sudo systemctl enable --now socksproxy
[Unit]
Description=URnetwork SOCKS5 proxy
After=network-online.target

[Service]
ExecStart=/opt/urnetd/socksproxy --auth-state /path/to/.auth_state --country Japan --addr 0.0.0.0:9999 --socks-user urnet --socks-password CHANGE_ME
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
UNIT

	tar -czf "$DIST/urnetd-linux-$arch.tar.gz" -C "$STAGE" .
	rm -rf "$STAGE"
	rm -f "/tmp/urnetd-l-$arch" "/tmp/socks-l-$arch"
done

echo
echo "完成:"
ls -lh "$DIST"/*.tar.gz | awk '{print "  "$9"  "$5}'