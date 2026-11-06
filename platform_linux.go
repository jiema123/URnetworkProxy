//go:build linux

package main

// 平台相关实现（Linux）：systemd user unit 自启、ip route 取默认网卡、xdg-open 打开链接。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func platformName() string { return "linux" }

// Linux 上没有 URnetwork App，凭据要把 macOS/iOS 上登录后的
// .auth_state 拷贝到下面这个位置（或用 URNET_AUTH_STATE 指定）
func defaultAuthState() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(home(), ".config")
	}
	return filepath.Join(dir, "urnetd", "auth_state")
}

func authStateHint() string {
	return "Linux 上没有 URnetwork App。请在 macOS / iOS 上登录一次，把该文件\n" +
		"  ~/Library/Containers/network.ur/Data/Documents/network_spaces/ur.network/main/.by/.auth_state\n" +
		"拷贝到上面的路径，或用 URNET_AUTH_STATE 指向你放它的位置。\n" +
		"凭据约 30 天过期，到期需重新拷一次。"
}

func defaultConfPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(home(), ".config")
	}
	return filepath.Join(dir, "urnetd", "urnetd.json")
}

func legacyConfPath() string { return "" }

// Linux 上没有 Shadowrocket，CONNECT 用的是 Clash / mihomo / sing-box，
// 它们只能吃配置或订阅，没法用 URL scheme 一键导入，所以面板隐藏这个按钮。
func supportsAppImport() bool { return false }

func openURL(u string) error { return exec.Command("xdg-open", u).Run() }

// 排除虚拟网卡与容器网桥：tun/wg/docker/veth/br-/virbr/cni
func ignoreIface(name string) bool {
	for _, p := range []string{"tun", "tap", "wg", "docker", "veth", "br-", "virbr", "cni", "lxc", "podman", "flannel", "tailscale", "zt"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return name == "lo"
}

// primaryIP: 从 `ip route show default` 里取 dev 名，再读该网卡的首个 IPv4。
// 和 macOS 一样，不用 UDP 探路——有 TUN 的时候会拿到假地址。
func primaryIP() string {
	if out, err := exec.Command("ip", "-4", "route", "show", "default").Output(); err == nil {
		if ip := ifaceIPv4(parseIPRouteDefault(string(out))); ip != "" {
			return ip
		}
	}
	// 精简系统可能没有 iproute2，退回 route 表格
	if out, err := exec.Command("route", "-n").Output(); err == nil {
		if ip := ifaceIPv4(parseRouteTable(string(out))); ip != "" {
			return ip
		}
	}
	return ""
}

func ifaceIPv4(name string) string {
	i, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, err := i.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		if s := ip.String(); !isBadIP(s) {
			return s
		}
	}
	return ""
}

// ---------- systemd user unit 自启 ----------

func unitPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(home(), ".config")
	}
	return filepath.Join(dir, "systemd", "user", "urnetd.service")
}

func writeUnit() error {
	p := unitPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	self := selfPath()
	content := `[Unit]
Description=URnetwork Console
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + self + ` app-start
WorkingDirectory=` + filepath.Dir(self) + `
Restart=always
RestartSec=5
StandardOutput=append:/tmp/urnetd.log
StandardError=append:/tmp/urnetd.log

[Install]
WantedBy=default.target
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload 失败: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func doInstall() {
	if err := writeUnit(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", "urnetd.service").CombinedOutput(); err != nil {
		fmt.Fprintln(os.Stderr, "启用失败:", err, strings.TrimSpace(string(out)))
		fmt.Println("unit 已写入:", unitPath())
		fmt.Println("手动启动: systemctl --user enable --now urnetd.service")
		os.Exit(1)
	}
	// 让服务在用户未登录时也运行（服务器/NAS 场景必需）
	u := os.Getenv("USER")
	if u == "" {
		u = fmt.Sprintf("%d", os.Getuid())
	}
	if err := exec.Command("loginctl", "enable-linger", u).Run(); err != nil {
		fmt.Println("提示: 未能开启 linger，注销后服务会停。有权限时可执行:")
		fmt.Println("  sudo loginctl enable-linger " + u)
	}
	for i := 0; i < 60; i++ {
		if httpAlive() {
			fmt.Println("开机自启已开启并立即生效 → http://" + listenAddr)
			fmt.Println("unit:", unitPath())
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("unit 已安装，但服务没起来，看 systemctl --user status urnetd 和 /tmp/urnetd.log")
}

func doUninstall() {
	exec.Command("systemctl", "--user", "disable", "--now", "urnetd.service").Run()
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(unitPath()); err == nil {
		if err := os.Remove(unitPath()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		fmt.Println("已卸载开机自启（urnetd.service）")
	} else {
		fmt.Println("没有安装开机自启")
	}
}

// authStateReadable 探一下凭据文件能不能读。
// 关键点：macOS 上跨 App 容器读文件可能卡在内核里（open 一直不返回），
// 所以这里丢给子进程 + 超时，卡住就杀掉，主进程照常报错。
func authStateReadable(p string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/cat", p)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return errors.New("读取超时（被系统权限拦住了）")
	}
	if err != nil {
		return err
	}
	if buf.Len() == 0 {
		return errors.New("文件是空的")
	}
	return nil
}

// Linux 上凭据就是普通文件，不需要拷贝
func ensureAuthState(cur string) (string, error) { return cur, nil }
