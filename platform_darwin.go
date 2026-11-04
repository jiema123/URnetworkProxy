//go:build darwin

package main

// 平台相关实现（macOS）：launchd 自启、route 取默认网卡、open 打开链接。

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func platformName() string { return "darwin" }

// 凭据由 URnetwork.app 登录后写入自己的沙盒容器
func defaultAuthState() string {
	return filepath.Join(home(), "Library/Containers/network.ur/Data/Documents/network_spaces/ur.network/main/.by/.auth_state")
}

func authStateHint() string {
	return "先打开一次 URnetwork.app 并登录，让它生成本地会话。"
}

// 配置放标准位置，别写死在开发目录里
func defaultConfPath() string {
	return filepath.Join(home(), "Library", "Application Support", "urnetd", "urnetd.json")
}

// 早期版本把配置写在这个路径，读到旧文件就自动迁移
func legacyConfPath() string {
	return filepath.Join(home(), "Dev", "personal", "tools", "urnetd", "urnetd.json")
}

// Shadowrocket 注册了 socks5:// scheme，open 可以把它唤起
func supportsAppImport() bool { return true }

func openURL(u string) error { return exec.Command("open", u).Run() }

// 排除 TUN / 桥接网卡：macOS 上是 utun/llw/bridge/ap（lo 顺带排掉，macOS 实际叫 lo0）
func ignoreIface(name string) bool {
	if name == "lo" {
		return true
	}
	return strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "llw") ||
		strings.HasPrefix(name, "bridge") || strings.HasPrefix(name, "ap")
}

// primaryIP: 取默认路由网卡上的 IPv4。
// 注意不能用 UDP 探路那套（Dial 8.8.8.8 取 LocalAddr），本机装了 Clash，
// 那条路由被 TUN 接管，拿到的会是 198.18.0.1 这种假地址。
func primaryIP() string {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return ""
	}
	return ifaceIPv4(parseBSDDefaultIfc(string(out)))
}

func ifaceIPv4(ifc string) string {
	if ifc == "" {
		return ""
	}
	i, err := net.InterfaceByName(ifc)
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

// ---------- launchd 自启 ----------

func plistPath() string {
	return filepath.Join(home(), "Library/LaunchAgents", urnetdLabel+".plist")
}

func domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func xmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// canLoadNow 探测当前会话有没有权限向 gui 域注册 job（受限会话会返回 EIO）
func canLoadNow() bool {
	probe := filepath.Join(home(), "Library/LaunchAgents", urnetdLabel+".probe.plist")
	content := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>` + urnetdLabel + `.probe</string>
<key>ProgramArguments</key><array><string>/bin/echo</string><string>x</string></array></dict></plist>
`
	if err := os.WriteFile(probe, []byte(content), 0o600); err != nil {
		return false
	}
	defer os.Remove(probe)
	if err := exec.Command("launchctl", "bootstrap", domain(), probe).Run(); err != nil {
		return false
	}
	exec.Command("launchctl", "bootout", domain()+"/"+urnetdLabel+".probe").Run()
	return true
}

func writePlist() error {
	plist := plistPath()
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	self := selfPath()
	content := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + urnetdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEsc(self) + `</string>
		<string>app-start</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>WorkingDirectory</key>
	<string>` + xmlEsc(filepath.Dir(self)) + `</string>
	<key>StandardOutPath</key>
	<string>/tmp/urnetd.log</string>
	<key>StandardErrorPath</key>
	<string>/tmp/urnetd.log</string>
</dict>
</plist>
`
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return err
	}
	if err := exec.Command("plutil", "-lint", plist).Run(); err != nil {
		return errors.New("plist 格式校验失败")
	}
	return nil
}

func doInstall() {
	if err := writePlist(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !canLoadNow() {
		fmt.Println("开机自启已安装:", plistPath())
		fmt.Println("下次登录 / 重启会启动 urnetd（代理与网页一起），不需要再操作。")
		fmt.Println("当前会话没有 gui 域注册权限，无法立刻加载（不影响下次登录）。")
		fmt.Println("想现在就用: ./urnetd")
		return
	}
	exec.Command("launchctl", "bootout", domain()+"/"+urnetdLabel).Run()
	time.Sleep(700 * time.Millisecond)
	if err := exec.Command("launchctl", "bootstrap", domain(), plistPath()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "加载失败:", err)
		os.Exit(1)
	}
	exec.Command("launchctl", "enable", domain()+"/"+urnetdLabel).Run()
	for i := 0; i < 60; i++ {
		if httpAlive() {
			fmt.Println("开机自启已开启并立即生效 → http://" + listenAddr)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("plist 已安装，但服务没起来，看 /tmp/urnetd.log")
}

func doUninstall() {
	exec.Command("launchctl", "bootout", domain()+"/"+urnetdLabel).Run()
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(plistPath()); err == nil {
		os.Remove(plistPath())
		fmt.Println("已卸载开机自启（" + urnetdLabel + "）")
	} else {
		fmt.Println("没有安装开机自启")
	}
}
