//go:build windows

package main

// 平台相关实现（Windows）：注册表 Run 项自启、UDP 探路取网卡、rundll32 打开链接。

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func platformName() string { return "windows" }

// Windows 上没有 Shadowrocket；Clash Verge / FlClash 有 Windows 版，
// 面板里的「导入 Clash / FlClash」按钮仍然可用。
func supportsAppImport() bool { return false }

func openURL(u string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}

// 排除虚拟网卡：Hyper-V、VMware、VirtualBox、各类 TUN
func ignoreIface(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range []string{"vethernet", "loopback", "tun", "tap", "wg", "tailscale",
		"vmware", "virtualbox", "npcap", "docker", "br-", "zerotier"} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

func appDataDir() string {
	d := os.Getenv("APPDATA")
	if d == "" {
		d = filepath.Join(home(), "AppData", "Roaming")
	}
	return filepath.Join(d, "urnetd")
}

func defaultAuthState() string {
	return filepath.Join(appDataDir(), "auth_state")
}

func authStateHint() string {
	return "Windows 上没有 URnetwork App。请在 macOS / iOS 上登录一次，把该文件\n" +
		"  ~/Library/Containers/network.ur/Data/Documents/network_spaces/ur.network/main/.by/.auth_state\n" +
		"拷贝到上面的路径，或用 URNET_AUTH_STATE 指向你放它的位置；\n" +
		"也可以在面板里用一次性授权码登录。凭据约 30 天过期。"
}

func defaultConfPath() string {
	return filepath.Join(appDataDir(), "urnetd.json")
}

func legacyConfPath() string { return "" }

// primaryIP: 先 UDP 探路问内核，拿到的若是 TUN 假地址再退回遍历网卡
func primaryIP() string {
	if c, err := net.Dial("udp", "223.5.5.5:80"); err == nil {
		if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
			ip := a.IP.String()
			c.Close()
			if !isBadIP(ip) {
				return ip
			}
		} else {
			c.Close()
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ignoreIface(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
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
	}
	return ""
}

// ---------- 开机自启：写 HKCU 的 Run 项 ----------

const runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func doInstall() {
	self := selfPath()
	cmdline := `"` + self + `" app-start`
	if out, err := exec.Command("reg", "add", runKey, "/v", "urnetd", "/t", "REG_SZ",
		"/d", cmdline, "/f").CombinedOutput(); err != nil {
		fmt.Fprintln(os.Stderr, "写入注册表失败:", err, strings.TrimSpace(string(out)))
		fmt.Println("可以手动把下面这条加到「启动」里：")
		fmt.Println("  " + cmdline)
		os.Exit(1)
	}
	fmt.Println("已加入开机启动（HKCU Run 项 urnetd）")
	fmt.Println("现在启动服务…")
	c := exec.Command(self, "app-start")
	c.Dir = filepath.Dir(self)
	if err := c.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
	for i := 0; i < 60; i++ {
		if httpAlive() {
			fmt.Println("已启动 → http://" + listenAddr)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("进程已拉起，但还没监听，看日志或稍等一下")
}

func doUninstall() {
	out, err := exec.Command("reg", "delete", runKey, "/v", "urnetd", "/f").CombinedOutput()
	if err != nil {
		_ = out
		fmt.Println("没有找到自启项，或删除失败（可能本来就没装）")
		return
	}
	fmt.Println("已移除开机启动")
}
