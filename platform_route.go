package main

// 路由输出的解析逻辑放在这里（不带 build tag），
// 目的是让这些纯字符串处理能在任一平台上被单元测试覆盖。

import "strings"

// parseIPRouteDefault 从 `ip -4 route show default` 的输出里取出口网卡名。
//
//	default via 192.168.1.1 dev eth0 proto dhcp src 192.168.1.20 metric 100
func parseIPRouteDefault(out string) string {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		for i, tok := range f {
			if tok == "dev" && i+1 < len(f) {
				return f[i+1]
			}
		}
	}
	return ""
}

// parseBSDDefaultIfc 从 `route -n get default` 的输出里取 interface 行。
//
//	interface: en9
func parseBSDDefaultIfc(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "interface:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		}
	}
	return ""
}

// parseRouteTable 兜底解析 `route -n` 的表格（精简系统可能没有 iproute2）。
//
//	Destination  Gateway    Genmask  Flags Metric Ref Use Iface
//	0.0.0.0      192.168.1.1 0.0.0.0  UG    100    0   0   eth0
func parseRouteTable(out string) string {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 8 && f[0] == "0.0.0.0" && strings.Contains(f[3], "G") {
			return f[7]
		}
	}
	return ""
}
