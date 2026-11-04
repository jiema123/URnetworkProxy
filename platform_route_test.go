package main

// 路由解析的单测。这些是纯字符串逻辑，所以在 macOS 上跑测试
// 也能覆盖 Linux 分支，不必真的找一台 Linux 机器。

import "testing"

func TestParseIPRouteDefault(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "dhcp 带 src 与 metric",
			in:   "default via 192.168.1.1 dev eth0 proto dhcp src 192.168.1.20 metric 100\n",
			want: "eth0",
		},
		{
			name: "wifi 网卡",
			in:   "default via 10.0.0.1 dev wlan0 proto static metric 600\n",
			want: "wlan0",
		},
		{
			name: "多行只取第一行",
			in:   "default via 10.0.0.1 dev eth0 metric 100\ndefault via 10.0.0.2 dev wlan0 metric 600\n",
			want: "eth0",
		},
		{
			name: "没有默认路由",
			in:   "",
			want: "",
		},
		{
			name: "有 dev 但后面没网卡名",
			in:   "default via 10.0.0.1 dev\n",
			want: "",
		},
	}
	for _, c := range cases {
		if got := parseIPRouteDefault(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestParseBSDDefaultIfc(t *testing.T) {
	in := `   route to: default
destination: default
       mask: default
    gateway: 10.39.108.1
  interface: en9
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING,GLOBAL>
`
	if got := parseBSDDefaultIfc(in); got != "en9" {
		t.Errorf("got %q, want en9", got)
	}
	if got := parseBSDDefaultIfc("nothing here\n"); got != "" {
		t.Errorf("空输出应为空，got %q", got)
	}
}

func TestParseRouteTable(t *testing.T) {
	in := `Kernel IP routing table
Destination     Gateway         Genmask         Flags Metric Ref    Use Iface
0.0.0.0         192.168.1.1     0.0.0.0         UG    100    0        0 eth0
192.168.1.0     0.0.0.0         255.255.255.0   U     100    0        0 eth0
`
	if got := parseRouteTable(in); got != "eth0" {
		t.Errorf("got %q, want eth0", got)
	}
	if got := parseRouteTable("0.0.0.0 192.168.1.1 0.0.0.0 U 100 0 0 eth0\n"); got != "" {
		t.Errorf("非网关行不应命中，got %q", got)
	}
}

func TestIgnoreIface(t *testing.T) {
	// 只验证当前平台的规则，两套都要能过：坏网卡必须被排除，正常网卡必须保留
	for _, bad := range []string{"lo"} {
		if !ignoreIface(bad) {
			t.Errorf("%q 应被排除", bad)
		}
	}
	for _, good := range []string{"eth0", "en0", "wlan0"} {
		if ignoreIface(good) {
			t.Errorf("%q 不应被排除（当前平台）", good)
		}
	}
}
