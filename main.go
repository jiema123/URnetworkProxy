// urnetd — URnetwork 出口管理器（P0）
//
// 一个二进制 = HTTP API + 内嵌前端，只监听 127.0.0.1。
// 它不管代理协议本身，只做四件事：
//  1. 监管 socksproxy 子进程（一个地区一个进程，一个端口）
//  2. 切换地区（停旧起新，失败自动回滚）
//  3. 探测真实出口 IP（自己实现 SOCKS5 握手，不引第三方库）
//  4. 生成节点 URI / 二维码
//
// 编译：go build -o urnetd .
// 运行：./urnetd        # 默认监听 127.0.0.1:8877
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
)

// ---------- 配置 ----------

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// socksproxy 的文件名（Windows 带 .exe，不然 exec 找不到）
// Windows 上优先找 socksproxy.exe；也认没后缀的 socksproxy
// （旧版兜底路径本身就没写 .exe，用户手动拷过去常常没后缀）
func socksProxyNames() []string {
	if platformName() == "windows" {
		return []string{"socksproxy.exe", "socksproxy"}
	}
	return []string{"socksproxy"}
}

// 找 socksproxy 时探过的路径，报错时打印出来，省得用户猜
var binSearchPaths []string

func binSearchHint() string {
	if len(binSearchPaths) == 0 {
		return ""
	}
	return "（找过这些位置）:\n  " + strings.Join(binSearchPaths, "\n  ")
}

// 按「和 urnetd 同目录」→「app 包里的 Contents/Resources」→ PATH → 老版本固定目录 依次找。
// 不写死绝对路径，否则换机器/换用户名就找不到。
func defaultBinPath() string {
	names := socksProxyNames()
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		if abs, err := filepath.Abs(exe); err == nil {
			exe = abs
		}
		dir := filepath.Dir(exe)
		dirs = append(dirs,
			dir,
			filepath.Join(dir, "..", "Resources"), // macOS .app：Contents/MacOS → Contents/Resources
			filepath.Join(dir, "..", "Resources", "bin"),
		)
	}
	// 老版本把 socksproxy 放在这个固定目录，作为最后兜底
	dirs = append(dirs, filepath.Join(home(), "Dev", "personal", "tools", "bin"))

	var cands []string
	for _, d := range dirs {
		for _, n := range names {
			cands = append(cands, filepath.Join(d, n))
		}
	}
	binSearchPaths = append(binSearchPaths, cands...)
	for _, p := range cands {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			binSearchPaths = append(binSearchPaths, "PATH: "+p)
			return p
		}
	}
	if len(cands) > 0 {
		return cands[0]
	}
	return names[0]
}

// appVersion 打在 release notes 和 banner 里，方便确认对方跑的是哪个版本
const appVersion = "1.1.4"

var (
	binPath    = env("URNET_BIN", defaultBinPath())
	authState  = env("URNET_AUTH_STATE", defaultAuthState())
	socksUser  = env("URNET_SOCKS_USER", "urnet")
	proxyPort  = envInt("URNET_PORT", 9999)
	listenAddr = env("URNETD_LISTEN", "127.0.0.1:8877")
	confFile   = env("URNETD_CONF", defaultConfPath())
	qrCache    = filepath.Join(os.TempDir(), "urnetd-qr.png")
)

// ---------- 状态 ----------

type Ring struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func NewRing(max int) *Ring { return &Ring{max: max} }

func (r *Ring) Add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, s)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

// note 记录 urnetd 自己的操作：既进环形日志（页面上能看到），
// 也打到 stdout（落进 /tmp/urnetd.log）。代理的输出量很大，
// 400 行的环形缓冲很快就会被冲掉，单独留一份才查得到。
func (s *Server) note(msg string) {
	s.logs.Add(msg)
	fmt.Println(msg)
}

func (r *Ring) Tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.lines) {
		n = len(r.lines)
	}
	out := make([]string, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}

type Region struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type Server struct {
	opMu sync.Mutex // 串行化 start/stop/switch

	mu       sync.Mutex
	cmd      *exec.Cmd
	done     chan struct{}
	country  string
	running  bool
	ready    bool
	busy     bool
	lastErr  string
	pid      int
	started  time.Time
	exitIP   string
	exitCity string
	exitCC   string
	latency  int64
	probeErr string
	crashes  int

	logs *Ring

	rMu       sync.Mutex
	regions   []Region
	regionsAt time.Time
}

func NewServer() *Server {
	return &Server{logs: NewRing(400)}
}

type savedConf struct {
	Country    string `json:"country,omitempty"`
	Salt       string `json:"salt,omitempty"`
	PassHash   string `json:"passHash,omitempty"`
	PublicHost string `json:"publicHost,omitempty"`
	SocksPass  string `json:"socksPass,omitempty"`
	// 可选：记住账号密码用于自动续期（明文，文件 0600）
	UserAuth string `json:"userAuth,omitempty"`
	Password string `json:"password,omitempty"`
}

// 老版本代码里写死的 SOCKS5 密码。只在升级已有的旧配置时沿用，
// 保证之前导入到客户端的节点不失效；新装用户一律随机生成。
const legacySocksPass = "0n1fTlEE71Yc76"

var (
	socksPassOnce sync.Once
	socksPassVal  string
)

// socksPassword 决定代理端口用哪个 SOCKS5 密码：环境变量 > 配置 > 新生成
func socksPassword() string {
	if v := os.Getenv("URNET_SOCKS_PASS"); v != "" {
		return v
	}
	socksPassOnce.Do(func() {
		c := loadConf()
		if c.SocksPass != "" {
			socksPassVal = c.SocksPass
			return
		}
		if c.PassHash != "" || c.Salt != "" {
			c.SocksPass = legacySocksPass
		} else {
			c.SocksPass = randomPassword(14)
		}
		saveConf(c)
		socksPassVal = c.SocksPass
	})
	return socksPassVal
}

func loadConf() savedConf {
	// 老版本把配置写在开发目录，发现旧文件就搬过来（密码等设置不丢）。
	// 只在用默认路径时迁移，避免把旧配置误灌进用户自己指定的 URNETD_CONF。
	if confFile == defaultConfPath() {
		if _, err := os.Stat(confFile); os.IsNotExist(err) {
			if legacy := legacyConfPath(); legacy != "" {
				if b, err := os.ReadFile(legacy); err == nil {
					var c savedConf
					if json.Unmarshal(b, &c) == nil {
						saveConf(c)
						return c
					}
				}
			}
		}
	}
	var c savedConf
	if b, err := os.ReadFile(confFile); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func saveConf(c savedConf) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(confFile), 0o755); err != nil {
		return err
	}
	// 含口令哈希，只给属主读写
	if err := os.WriteFile(confFile, b, 0o600); err != nil {
		return err
	}
	return os.Chmod(confFile, 0o600)
}

func loadCountry() string {
	if c := loadConf(); c.Country != "" {
		return c.Country
	}
	return "Japan"
}

// saveCountry 必须是读-改-写：早先直接覆盖整个结构体会把口令字段冲掉
func saveCountry(country string) {
	c := loadConf()
	c.Country = country
	saveConf(c)
}

// ---------- 端口与地址工具 ----------

func portBusy(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 700*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// addrBusy 判断某个监听地址的端口上是否已经有服务
func addrBusy(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 700*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// 排除 Clash / Surge 之类 TUN 的假 IP 与链路本地地址
func isBadIP(s string) bool {
	return strings.HasPrefix(s, "198.18.") || strings.HasPrefix(s, "198.19.") ||
		strings.HasPrefix(s, "169.254.") || s == "" || s == "<nil>"
}

func lanAddrs() (primary string, all []string) {
	primary = primaryIP()
	ifaces, err := net.Interfaces()
	if err != nil {
		return primary, []string{primary}
	}
	for _, ifc := range ifaces {
		if ignoreIface(ifc.Name) {
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
			s := ip.String()
			if isBadIP(s) {
				continue // Clash 等 TUN 的假 IP
			}
			all = append(all, s)
		}
	}
	if primary != "" && !isBadIP(primary) {
		found := false
		for _, a := range all {
			if a == primary {
				found = true
			}
		}
		if !found {
			all = append([]string{primary}, all...)
		} else {
			sort.SliceStable(all, func(i, j int) bool { return all[i] == primary && all[j] != primary })
		}
	}
	if len(all) == 0 && primary != "" {
		all = []string{primary}
	}
	return primary, all
}

// ---------- JWT ----------

func jwtDaysLeft() int {
	tok, err := readByJwt(authState)
	if err != nil {
		return -1
	}
	info, err := decodeJwt(tok)
	if err != nil {
		return -1
	}
	return info.DaysLeft
}

// ---------- 极简 SOCKS5 客户端（用户名/密码认证） ----------

func socks5Dial(proxyAddr, user, pass, host string, port int, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(timeout))
	fail := func(e error) (net.Conn, error) { conn.Close(); return nil, e }

	// greeting: 5, 2 methods: no-auth + user/pass
	if _, err := conn.Write([]byte{0x05, 0x02, 0x00, 0x02}); err != nil {
		return fail(err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return fail(err)
	}
	if buf[0] != 0x05 {
		return fail(errors.New("socks 版本不匹配"))
	}
	switch buf[1] {
	case 0x00: // 无需认证
	case 0x02: // RFC1929
		req := []byte{0x01, byte(len(user))}
		req = append(req, user...)
		req = append(req, byte(len(pass)))
		req = append(req, pass...)
		if _, err := conn.Write(req); err != nil {
			return fail(err)
		}
		rep := make([]byte, 2)
		if _, err := io.ReadFull(conn, rep); err != nil {
			return fail(err)
		}
		if rep[1] != 0x00 {
			return fail(errors.New("socks 认证失败"))
		}
	default:
		return fail(fmt.Errorf("socks 不支持的认证方式 %d", buf[1]))
	}

	// CONNECT（域名交给代理端解析）
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req); err != nil {
		return fail(err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fail(err)
	}
	if head[1] != 0x00 {
		return fail(fmt.Errorf("socks CONNECT 被拒绝，code=%d", head[1]))
	}
	// 吃掉绑定地址
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return fail(err)
		}
		skip = int(l[0])
	default:
		return fail(errors.New("socks 回复地址类型未知"))
	}
	if _, err := io.ReadFull(conn, make([]byte, skip+2)); err != nil {
		return fail(err)
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

type ipInfo struct {
	IP      string `json:"ip"`
	City    string `json:"city"`
	Country string `json:"country"`
}

func probeExit() (ipInfo, int64, error) {
	proxyAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(proxyPort))
	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h, p, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			pn, _ := strconv.Atoi(p)
			return socks5Dial(proxyAddr, socksUser, socksPassword(), h, pn, 10*time.Second)
		},
		TLSClientConfig: &tls.Config{ServerName: "ipinfo.io"},
	}
	client := &http.Client{Transport: tr, Timeout: 25 * time.Second}
	start := time.Now()
	resp, err := client.Get("https://ipinfo.io/json")
	if err != nil {
		return ipInfo{}, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return ipInfo{}, 0, err
	}
	var info ipInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return ipInfo{}, 0, err
	}
	return info, time.Since(start).Milliseconds(), nil
}

// ---------- 进程管理 ----------

func (s *Server) pump(r io.Reader, readyCh chan struct{}) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 256*1024)
	once := sync.Once{}
	for sc.Scan() {
		line := sc.Text()
		s.logs.Add(line)
		if strings.Contains(line, "socks5 server is listening") {
			once.Do(func() { close(readyCh) })
		}
	}
}

func (s *Server) killExternal() {
	// 端口被脚本或旧进程占着时清掉它（平台各自实现）
	if killSquatters(binPath) {
		time.Sleep(600 * time.Millisecond)
	}
}

func (s *Server) startLocked(country string) error {
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("找不到 socksproxy: %s%s\n把 socksproxy 放到 urnetd 旁边（同一目录），或用 URNET_BIN 指定", binPath, binSearchHint())
	}
	if _, err := os.Stat(authState); err != nil {
		return fmt.Errorf("找不到 auth_state: %s\n%s", authState, authStateHint())
	}
	if portBusy(proxyPort) {
		s.note("[urnetd] 端口 " + strconv.Itoa(proxyPort) + " 被占用，尝试清理旧进程")
		s.killExternal()
		if portBusy(proxyPort) {
			return fmt.Errorf("端口 %d 仍被占用，请先手动停掉", proxyPort)
		}
	}

	args := []string{
		"--auth-state", authState,
		"--country", country,
		"--addr", fmt.Sprintf("0.0.0.0:%d", proxyPort),
		"--socks-user", socksUser,
		"--socks-password", socksPassword(),
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command(binPath, args...)
	cmd.Stdout = pw
	cmd.Stderr = pw
	setProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return err
	}
	pw.Close()

	readyCh := make(chan struct{})
	go s.pump(pr, readyCh)

	select {
	case <-readyCh:
	case <-time.After(50 * time.Second):
		stopProc(cmd, true)
		cmd.Wait()
		pr.Close()
		return errors.New("启动超时（50s），看日志面板")
	}

	done := make(chan struct{})
	s.mu.Lock()
	s.cmd = cmd
	s.done = done
	s.pid = cmd.Process.Pid
	s.country = country
	s.running = true
	s.ready = true
	s.started = time.Now()
	s.exitIP, s.exitCity, s.exitCC, s.latency = "", "", "", 0
	s.lastErr = ""
	s.mu.Unlock()

	saveCountry(country)
	s.note("[urnetd] 已启动 " + country + "，pid " + strconv.Itoa(cmd.Process.Pid))

	// 唯一的 Wait 点：进程退出后释放管道并更新状态
	go func() {
		cmd.Wait()
		pr.Close()

		s.mu.Lock()
		unexpected := s.cmd == cmd
		if unexpected {
			s.running = false
			s.ready = false
			s.pid = 0
			s.crashes++
			s.lastErr = "代理进程意外退出"
		}
		country := s.country
		crashes := s.crashes
		s.mu.Unlock()
		close(done)

		// 非用户主动停止的情况：自动重试，最多 3 次，避免静默断网
		if unexpected && crashes <= 3 {
			s.note("[urnetd] 进程意外退出，8 秒后自动重启（第 " + strconv.Itoa(crashes) + " 次）")
			go func() {
				time.Sleep(8 * time.Second)
				s.mu.Lock()
				stillDown := !s.running && !s.busy
				s.mu.Unlock()
				if stillDown {
					s.switchTo(country)
				}
			}()
		} else if unexpected {
			s.note("[urnetd] 连续失败 3 次，停止重试")
			s.mu.Lock()
			s.lastErr = "连续启动失败，请检查凭据是否过期"
			s.mu.Unlock()
		}
	}()

	go s.refreshExit()
	return nil
}

func (s *Server) refreshExit() {
	info, ms, err := probeExit()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		// 探测失败只是暂时现象（建连中、切换中），不写进 lastErr，
		// 否则界面上会一直挂着一条已经过期的错误。
		s.probeErr = err.Error()
		return
	}
	s.exitIP = info.IP
	s.exitCity = info.City
	s.exitCC = info.Country
	s.latency = ms
	s.probeErr = ""
	s.lastErr = ""
}

func (s *Server) stopLocked() {
	s.mu.Lock()
	cmd := s.cmd
	done := s.done
	s.cmd = nil
	s.done = nil
	s.running = false
	s.ready = false
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	stopProc(cmd, false)
	if done != nil {
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			stopProc(cmd, true)
			<-done
		}
	}
	s.mu.Lock()
	s.pid = 0
	s.mu.Unlock()
	s.note("[urnetd] 已停止")
}

// Switch 是用户触发的切换：清零崩溃计数后走完整流程
func (s *Server) Switch(country string) {
	s.mu.Lock()
	s.crashes = 0
	s.mu.Unlock()
	s.switchTo(country)
}

func (s *Server) switchTo(country string) {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	s.mu.Lock()
	s.busy = true
	prev := s.country
	s.lastErr = ""
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()

	s.stopLocked()
	time.Sleep(400 * time.Millisecond)

	if err := s.startLocked(country); err != nil {
		s.mu.Lock()
		s.lastErr = err.Error()
		s.mu.Unlock()
		s.note("[urnetd] 切换到 " + country + " 失败: " + err.Error())
		if prev != "" && prev != country {
			s.note("[urnetd] 回滚到 " + prev)
			if err2 := s.startLocked(prev); err2 != nil {
				s.note("[urnetd] 回滚也失败: " + err2.Error())
			}
		}
		return
	}
	time.Sleep(700 * time.Millisecond)
	s.refreshExit()
}

// ---------- 地区列表 ----------

func (s *Server) loadRegions(force bool) ([]Region, error) {
	s.rMu.Lock()
	if !force && time.Since(s.regionsAt) < 10*time.Minute && len(s.regions) > 0 {
		out := s.regions
		s.rMu.Unlock()
		return out, nil
	}
	s.rMu.Unlock()

	b, err := os.ReadFile(authState)
	if err != nil {
		return nil, fmt.Errorf("读 auth_state 失败: %w", err)
	}
	var st struct {
		ByJwt string `json:"by_jwt"`
	}
	if json.Unmarshal(b, &st) != nil || st.ByJwt == "" {
		return nil, errors.New("auth_state 里没有 by_jwt")
	}

	req, _ := http.NewRequest("GET", "https://api.bringyour.com/network/provider-locations", nil)
	req.Header.Set("Authorization", "Bearer "+st.ByJwt)
	resp, err := (&http.Client{Timeout: 25 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	var payload struct {
		Locations []struct {
			Type  string `json:"location_type"`
			Name  string `json:"name"`
			Count int    `json:"provider_count"`
		} `json:"locations"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	var out []Region
	for _, l := range payload.Locations {
		if l.Type == "country" {
			out = append(out, Region{Name: l.Name, Count: l.Count})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })

	s.rMu.Lock()
	s.regions = out
	s.regionsAt = time.Now()
	s.rMu.Unlock()
	return out, nil
}

// ---------- 二维码 ----------

func makeQR(uri string) error {
	// 纯 Go 实现，不再依赖 python3 / qrencode，打包成 app 也能独立工作
	png, err := qrcode.Encode(uri, qrcode.Medium, 560)
	if err != nil {
		return err
	}
	return os.WriteFile(qrCache, png, 0o644)
}

// ---------- HTTP ----------

func (s *Server) nodeURI(host string) string {
	s.mu.Lock()
	c := s.country
	s.mu.Unlock()
	if c == "" {
		c = "unknown"
	}
	name := strings.ReplaceAll("URnetwork-"+c, " ", "%20")
	return fmt.Sprintf("socks5://%s:%s@%s:%d#%s", socksUser, socksPassword(), host, proxyPort, name)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	primary, addrs := lanAddrs()
	s.mu.Lock()
	state := map[string]any{
		"country":    s.country,
		"running":    s.running,
		"ready":      s.ready,
		"busy":       s.busy,
		"pid":        s.pid,
		"port":       proxyPort,
		"exitIp":     s.exitIP,
		"exitCity":   s.exitCity,
		"exitCC":     s.exitCC,
		"latencyMs":  s.latency,
		"lastErr":    s.lastErr,
		"probeErr":   s.probeErr,
		"primary":    primary,
		"addrs":      addrs,
		"jwtDays":    jwtDaysLeft(),
		"canPush":    supportsAppImport(),
		"platform":   platformName(),
		"publicHost": loadConf().PublicHost,
		"nodeHost":   nodeHostFrom(""),
	}
	if !s.started.IsZero() {
		state["uptimeSec"] = int(time.Since(s.started).Seconds())
	}
	s.mu.Unlock()

	s.rMu.Lock()
	state["regionCount"] = len(s.regions)
	s.rMu.Unlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(state)
}

func (s *Server) handleRegions(w http.ResponseWriter, r *http.Request) {
	list, err := s.loadRegions(r.URL.Query().Get("refresh") == "1")
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(list)
}

func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	var body struct {
		Country string `json:"country"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Country) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "缺少 country"})
		return
	}
	s.mu.Lock()
	busy := s.busy
	s.mu.Unlock()
	if busy {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(map[string]string{"error": "正在切换，请稍候"})
		return
	}
	go s.Switch(strings.TrimSpace(body.Country))
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "started": body.Country})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.stopLocked()
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	busy := s.busy
	running := s.running
	c := s.country
	s.mu.Unlock()
	if busy || running {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(map[string]string{"error": "已在运行或正在切换"})
		return
	}
	if c == "" {
		c = loadCountry()
	}
	go s.Switch(c)
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "country": c})
}

// ---------- 对外地址（远端部署时手机要用的那个地址） ----------

var (
	pubMu  sync.Mutex
	pubVal string
	pubAt  time.Time
)

// publicIP 问公共接口要本机的公网出口 IP（直连，不走自己的代理）
func publicIP() string {
	pubMu.Lock()
	defer pubMu.Unlock()
	if pubVal != "" && time.Since(pubAt) < 10*time.Minute {
		return pubVal
	}
	resp, err := (&http.Client{Timeout: 6 * time.Second}).Get("https://api.ipify.org")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	ip := strings.TrimSpace(string(b))
	if net.ParseIP(ip) == nil {
		return ""
	}
	pubVal, pubAt = ip, time.Now()
	return ip
}

// configuredHost 取配置里的对外地址，auto 表示自动查公网 IP
func configuredHost() string {
	h := os.Getenv("URNET_PUBLIC_HOST")
	if h == "" {
		h = loadConf().PublicHost
	}
	if h == "auto" {
		return publicIP()
	}
	return h
}

// nodeHostFrom 决定二维码/URI 里写哪个地址：
// 显式传入 > 配置的对外地址 > 默认路由网卡地址
func nodeHostFrom(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if h := configuredHost(); h != "" {
		return h
	}
	primary, _ := lanAddrs()
	return primary
}

func isHostname(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" || len(part) > 63 {
			return false
		}
		for _, r := range part {
			ok := r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
			if !ok {
				return false
			}
		}
	}
	return true
}

func (s *Server) handlePublicHost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method == "POST" {
		var body struct {
			Host string `json:"host"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "参数错误"})
			return
		}
		h := strings.TrimSpace(body.Host)
		if h != "" && h != "auto" && net.ParseIP(h) == nil && !isHostname(h) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "不是合法的 IP 或域名"})
			return
		}
		c := loadConf()
		c.PublicHost = h
		if err := saveConf(c); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}
	json.NewEncoder(w).Encode(map[string]string{
		"publicHost": loadConf().PublicHost,
		"effective":  configuredHost(),
	})
}

func (s *Server) handleNode(w http.ResponseWriter, r *http.Request) {
	host := nodeHostFrom(r.URL.Query().Get("host"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]string{"uri": s.nodeURI(host), "host": host})
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	host := nodeHostFrom(r.URL.Query().Get("host"))
	uri := s.nodeURI(host)
	if err := makeQR(uri); err != nil {
		w.WriteHeader(501)
		w.Write([]byte(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, qrCache)
}

// 本机一键导入：Shadowrocket 注册了 socks5:// scheme，
// 用 open 把 URI 交给系统即可弹出「添加节点」确认框。
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	host := nodeHostFrom(r.URL.Query().Get("host"))
	uri := s.nodeURI(host)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !supportsAppImport() {
		w.WriteHeader(501)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "当前系统没有可直接导入的代理客户端，请扫码或用复制 URI",
			"uri":   uri,
		})
		return
	}
	if err := openURL(uri); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "uri": uri})
		return
	}
	s.note("[urnetd] 已推送到 Shadowrocket: " + host)
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "uri": uri})
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method == "POST" {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "参数错误"})
			return
		}
		jwt, ok := extractJwt(body.Token)
		if !ok {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "没有识别到 JWT，请粘贴完整 token 或 .auth_state 内容"})
			return
		}
		info, err := decodeJwt(jwt)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if err := writeByJwt(authState, jwt); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// 换凭据后必须重启代理，否则新 token 不会被用上
		s.mu.Lock()
		c := s.country
		s.mu.Unlock()
		if c == "" {
			c = loadCountry()
		}
		go s.Switch(c)
		s.note("[urnetd] 凭据已更新，重启代理以生效")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":         true,
			"expiresAt":  info.ExpiresAt.Format("2006-01-02 15:04"),
			"daysLeft":   info.DaysLeft,
			"expired":    info.Expired,
			"network":    info.NetworkName,
			"restarting": true,
		})
		return
	}

	info, tok, err := tokenStatus()
	c := loadConf()
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"valid": false, "error": err.Error(), "path": authState, "hasCred": c.UserAuth != ""})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"valid":     !info.Expired,
		"expiresAt": info.ExpiresAt.Format("2006-01-02 15:04"),
		"daysLeft":  info.DaysLeft,
		"network":   info.NetworkName,
		"finger":    fingerprint(tok),
		"path":      authState,
		"hasCred":   c.UserAuth != "",
		"userAuth":  c.UserAuth,
	})
}

func (s *Server) handleAuthCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "参数错误"})
		return
	}
	jwt, err := loginWithAuthCode(body.Code)
	if err != nil {
		s.note("[urnetd] 授权码登录失败: " + err.Error())
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	info, err := decodeJwt(jwt)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if err := writeByJwt(authState, jwt); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	country := s.country
	s.mu.Unlock()
	if country == "" {
		country = loadCountry()
	}
	go s.Switch(country)
	s.note("[urnetd] 授权码登录成功，凭据已更新（剩 " + strconv.Itoa(info.DaysLeft) + " 天）")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":        true,
		"network":   info.NetworkName,
		"expiresAt": info.ExpiresAt.Format("2006-01-02 15:04"),
		"daysLeft":  info.DaysLeft,
	})
}

func (s *Server) handleLoginCred(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	var body struct {
		UserAuth string `json:"userAuth"`
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "参数错误"})
		return
	}
	body.UserAuth = strings.TrimSpace(body.UserAuth)
	if body.UserAuth == "" || body.Password == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "账号和密码都要填"})
		return
	}
	jwt, network, err := loginWithPassword(body.UserAuth, body.Password)
	if err != nil {
		s.note("[urnetd] 账号密码登录失败: " + err.Error())
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	info, err := decodeJwt(jwt)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if err := writeByJwt(authState, jwt); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if body.Remember {
		c := loadConf()
		c.UserAuth = body.UserAuth
		c.Password = body.Password
		saveConf(c)
	} else {
		c := loadConf()
		if c.UserAuth != "" || c.Password != "" {
			c.UserAuth, c.Password = "", ""
			saveConf(c)
		}
	}
	s.mu.Lock()
	country := s.country
	s.mu.Unlock()
	if country == "" {
		country = loadCountry()
	}
	go s.Switch(country)
	s.note("[urnetd] 账号密码登录成功，凭据已更新（" + network + "）")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":        true,
		"network":   network,
		"expiresAt": info.ExpiresAt.Format("2006-01-02 15:04"),
		"daysLeft":  info.DaysLeft,
		"remember":  body.Remember,
	})
}

// 面板上的「停止并退出」：停掉代理后整个进程退出。
// 注意：如果装了开机自启（launchd / systemd 的 KeepAlive），会被重新拉起，
// 那种情况下要先 uninstall 才停得掉。
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "bye": true})
	s.note("[urnetd] 收到退出请求，正在停止")
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.opMu.Lock()
		s.stopLocked()
		s.opMu.Unlock()
		time.Sleep(300 * time.Millisecond)
		os.Exit(0)
	}()
}

// ---------- Clash / mihomo 订阅 ----------

func yamlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// clashYAML 生成只含一个节点的最小可用配置。
// 故意不写 mixed-port 等端口字段：Clash Verge 会用自己设置里的端口覆盖，
// 写死反而容易和用户已有配置打架。
func (s *Server) clashYAML(host string) string {
	s.mu.Lock()
	country := s.country
	s.mu.Unlock()
	if country == "" {
		country = loadCountry()
	}
	name := "URnetwork-" + country
	var b strings.Builder
	b.WriteString("# 由 urnetd 生成 —— 只是一个本地 SOCKS5 节点，分流规则请在客户端里配\n")
	b.WriteString("dns:\n")
	b.WriteString("  # 客户端若开了 dns.respect-rules，缺这一项会被 mihomo 判为无效配置\n")
	b.WriteString("  proxy-server-nameserver:\n")
	b.WriteString("    - 223.5.5.5\n")
	b.WriteString("    - 1.1.1.1\n")
	b.WriteString("proxies:\n")
	b.WriteString("  - name: " + yamlQuote(name) + "\n")
	b.WriteString("    type: socks5\n")
	b.WriteString("    server: " + yamlQuote(host) + "\n")
	b.WriteString("    port: " + strconv.Itoa(proxyPort) + "\n")
	b.WriteString("    username: " + yamlQuote(socksUser) + "\n")
	b.WriteString("    password: " + yamlQuote(socksPassword()) + "\n")
	b.WriteString("    udp: true\n")
	b.WriteString("proxy-groups:\n")
	b.WriteString("  - name: \"URnetwork\"\n")
	b.WriteString("    type: select\n")
	b.WriteString("    proxies:\n")
	b.WriteString("      - " + yamlQuote(name) + "\n")
	b.WriteString("rules:\n")
	b.WriteString("  - MATCH,URnetwork\n")
	return b.String()
}

// handleSub 订阅地址：/sub/<token>.yaml，用令牌代替登录态
func (s *Server) handleSub(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.URL.Path, "/sub/")
	tok = strings.TrimSuffix(tok, ".yaml")
	if !checkSubToken(tok) {
		http.Error(w, "token 不对", http.StatusForbidden)
		return
	}
	host := nodeHostFrom(r.URL.Query().Get("host"))
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// 客户端多半拿文件名当订阅名，所以带上地区
	s.mu.Lock()
	country := s.country
	s.mu.Unlock()
	if country == "" {
		country = "node"
	}
	fn := "URnetwork-" + strings.ReplaceAll(country, " ", "-") + ".yaml"
	w.Header().Set("Content-Disposition", "attachment; filename=\""+fn+"\"")
	w.Write([]byte(s.clashYAML(host)))
}

// pushSub 把订阅地址用某个客户端的深链推过去。
// Clash 系客户端都认 <scheme>://install-config?url=<订阅地址>，自己来拉配置。
func (s *Server) pushSub(w http.ResponseWriter, r *http.Request, scheme, clientName string) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	host := nodeHostFrom(r.URL.Query().Get("host"))
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		port = "8877"
	}
	subURL := "http://127.0.0.1:" + port + "/sub/" + subToken() + ".yaml?host=" + url.QueryEscape(host)
	link := scheme + "://install-config?url=" + url.QueryEscape(subURL)
	s.mu.Lock()
	country := s.country
	s.mu.Unlock()
	if country != "" {
		link += "&name=" + url.QueryEscape("URnetwork-"+country)
	}
	if err := openURL(link); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "subUrl": subURL})
		return
	}
	s.note("[urnetd] 已把订阅推给 " + clientName + ": " + host)
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "subUrl": subURL, "link": link})
}

// 一键导入 Clash Verge
func (s *Server) handlePushClash(w http.ResponseWriter, r *http.Request) {
	s.pushSub(w, r, "clash", "Clash Verge")
}

// 一键导入 FlClash（它同时注册了 clash / clashmeta / flclash，用 flclash 才不歧义）
func (s *Server) handlePushFlClash(w http.ResponseWriter, r *http.Request) {
	s.pushSub(w, r, "flclash", "FlClash")
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 200
	if v := r.URL.Query().Get("tail"); v != "" {
		if x, err := strconv.Atoi(v); err == nil {
			n = x
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(s.logs.Tail(n))
}

const urnetdLabel = "com.fangjiefeng.urnetd"

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "urnetd"
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func httpAlive() bool {
	c, err := net.DialTimeout("tcp", listenAddr, 800*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "install":
		doInstall()
	case "uninstall":
		doUninstall()
	case "version", "-v", "--version":
		fmt.Println("urnetd", appVersion, "("+platformName()+")")
		fmt.Println("  可执行文件:", selfPath())
		fmt.Println("  socksproxy:", binPath)
		if len(binSearchPaths) > 1 {
			fmt.Println("  查找顺序:")
			for _, p := range binSearchPaths {
				mark := " "
				if p == binPath {
					mark = "✓"
				}
				fmt.Println("   ", mark, p)
			}
		}
	case "token":
		doToken(os.Args[2:])
	case "set-password":
		pw := ""
		if len(os.Args) > 2 {
			pw = os.Args[2]
		}
		if pw == "" {
			pw = randomPassword(12)
			fmt.Println("生成随机密码:", pw)
		}
		if len(pw) < 6 {
			fmt.Fprintln(os.Stderr, "密码太短，至少 6 位")
			os.Exit(1)
		}
		if err := setPassword(pw); err != nil {
			fmt.Fprintln(os.Stderr, "写入失败:", err)
			os.Exit(1)
		}
		fmt.Println("面板密码已更新（旧会话立即失效，正在运行的服务无需重启）")
	case "-h", "--help", "help":
		fmt.Println("urnetd — URnetwork 出口管理器")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  ./urnetd              前台运行，默认 http://" + listenAddr)
		fmt.Println("  ./urnetd install      装成 LaunchAgent，登录自动启动")
		fmt.Println("  ./urnetd uninstall    卸载开机自启")
		fmt.Println("  ./urnetd app-start    同前台运行（launchd 用）")
		fmt.Println("  ./urnetd set-password <密码>   设置/修改面板密码（不带参数则随机生成）")
		fmt.Println("  ./urnetd token         查看凭据有效期")
		fmt.Println("  ./urnetd token show    打印完整 JWT（拷到别的机器用）")
		fmt.Println("  ./urnetd token import <文件|->   导入 JWT 或 .auth_state")
		fmt.Println()
		fmt.Println("环境变量: URNETD_LISTEN / URNET_PORT / URNET_BIN / URNET_AUTH_STATE")
		fmt.Println("          URNET_SOCKS_USER / URNET_SOCKS_PASS")
	default:
		serve()
	}
}

//go:embed help/authcode.png
var authCodeHelpEmbedded []byte

//go:embed help/volt.gif
var voltGIF []byte

// helpImage 优先读磁盘上的同目录文件，方便替换示意图而不用重新编译；
// 读不到就退回编译进二进制的那份。
//
// 可替换位置（按优先级）：
//
//	$URNET_HELP_IMG
//	<可执行文件目录>/help/authcode.png          （命令行版）
//	<.app>/Contents/Resources/help/authcode.png （app 版）
func helpImage() []byte {
	cands := []string{}
	if p := os.Getenv("URNET_HELP_IMG"); p != "" {
		cands = append(cands, p)
	}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		cands = append(cands,
			filepath.Join(d, "help", "authcode.png"),
			filepath.Join(d, "..", "Resources", "help", "authcode.png"),
		)
	}
	for _, p := range cands {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return b
		}
	}
	return authCodeHelpEmbedded
}

func serve() {
	// 已经有实例在跑：不要再起一个（会撞端口），直接把浏览器叫起来
	if addrBusy(listenAddr) {
		fmt.Println("urnetd 已经在运行，直接打开面板:", "http://"+listenAddr)
		if os.Getenv("__CFBundleIdentifier") != "" {
			openURL("http://" + listenAddr)
		}
		return
	}

	s := NewServer()

	if err := os.MkdirAll(filepath.Dir(confFile), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	fmt.Printf("urnetd %s 监听 http://%s\n", appVersion, listenAddr)
	fmt.Printf("  socksproxy: %s\n", binPath)
	fmt.Printf("  auth_state: %s\n", authState)
	fmt.Printf("  代理端口:   %d\n", proxyPort)

	// 首次启动没有密码：如果是环境变量给了就直接用，否则提示去页面里设
	if ensurePassword() {
		fmt.Println("  面板密码已按 URNETD_PASSWORD 写入")
	} else if loadConf().PassHash == "" {
		fmt.Println()
		if isLoopbackListen() {
			fmt.Println("  首次使用：浏览器打开 http://" + listenAddr + " 会引导你设置面板密码")
		} else {
			fmt.Println("  注意：还没设置面板密码，非本机访问会被拒绝")
			fmt.Println("  先执行: urnetd set-password <你的密码>")
		}
		fmt.Println()
	}

	// 双击 .app 启动时，LaunchServices 会注入 __CFBundleIdentifier。
	// 那种情况下启动完顺手把浏览器打开；由 launchd 用 app-start 拉起时不会触发。
	if os.Getenv("__CFBundleIdentifier") != "" {
		go func() {
			time.Sleep(900 * time.Millisecond)
			openURL("http://" + listenAddr)
		}()
	}

	// 启动时若端口已被外部进程占用，先接管
	if portBusy(proxyPort) {
		fmt.Println("  端口已被占用，尝试接管…")
		s.killExternal()
	}

	go func() {
		time.Sleep(300 * time.Millisecond)
		s.Switch(loadCountry())
	}()

	// 预载地区列表，同时每 10 分钟自动刷新一次
	go func() {
		time.Sleep(2 * time.Second)
		s.loadRegions(false)
		for range time.Tick(10 * time.Minute) {
			s.loadRegions(true)
		}
	}()

	// 每 30 秒重新探测出口 IP（出口是随机分配的，会变）
	go func() {
		for range time.Tick(30 * time.Second) {
			s.mu.Lock()
			ok := s.running && s.ready && !s.busy
			s.mu.Unlock()
			if ok {
				s.refreshExit()
			}
		}
	}()

	// 监视凭据文件：App 刷新或命令行导入后，自动重启代理让新 token 生效
	go func() {
		var last time.Time
		if st, err := os.Stat(authState); err == nil {
			last = st.ModTime()
		}
		for range time.Tick(30 * time.Second) {
			st, err := os.Stat(authState)
			if err != nil || !st.ModTime().After(last) {
				continue
			}
			last = st.ModTime()
			s.mu.Lock()
			running, c := s.running, s.country
			s.mu.Unlock()
			if !running || c == "" {
				continue
			}
			s.note("[urnetd] 凭据文件已变动，重启代理以生效")
			go s.Switch(c)
		}
	}()

	// 存了账号密码的话，每 6 小时检查一次，快过期就自动续期
	go func() {
		for range time.Tick(6 * time.Hour) {
			c := loadConf()
			if c.UserAuth == "" || c.Password == "" {
				continue
			}
			info, _, err := tokenStatus()
			if err == nil && !info.Expired && info.DaysLeft > 5 {
				continue
			}
			jwt, _, err := loginWithPassword(c.UserAuth, c.Password)
			if err != nil {
				s.note("[urnetd] 自动续期失败: " + err.Error())
				continue
			}
			if err := writeByJwt(authState, jwt); err != nil {
				s.note("[urnetd] 自动续期写入失败: " + err.Error())
				continue
			}
			s.note("[urnetd] 凭据已自动续期（文件变动会触发代理重启）")
		}
	}()

	mux := http.NewServeMux()
	// 免登录：登录页与登录/登出接口
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			s.handleLoginPost(w, r)
			return
		}
		s.handleLoginPage(w, r)
	})
	mux.HandleFunc("/api/login", s.handleLoginPost)
	// 首次使用：还没设密码时，本机访问会被引到这里自己设一个
	mux.HandleFunc("/setup", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			s.handleSetupPost(w, r)
			return
		}
		s.handleSetupPage(w, r)
	})
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/api/logout", s.handleLogout)

	// 其余一律要登录
	mux.HandleFunc("/api/state", s.authWrap(s.handleState))
	mux.HandleFunc("/api/regions", s.authWrap(s.handleRegions))
	mux.HandleFunc("/api/switch", s.authWrap(s.handleSwitch))
	mux.HandleFunc("/api/stop", s.authWrap(s.handleStop))
	mux.HandleFunc("/api/start", s.authWrap(s.handleStart))
	mux.HandleFunc("/api/node", s.authWrap(s.handleNode))
	mux.HandleFunc("/api/qr", s.authWrap(s.handleQR))
	mux.HandleFunc("/api/push", s.authWrap(s.handlePush))
	mux.HandleFunc("/api/push-clash", s.authWrap(s.handlePushClash))
	mux.HandleFunc("/api/push-flclash", s.authWrap(s.handlePushFlClash))
	// 订阅地址用令牌鉴权，Clash 拉取时没有登录态
	mux.HandleFunc("/sub/", s.handleSub)
	mux.HandleFunc("/api/logs", s.authWrap(s.handleLogs))
	mux.HandleFunc("/api/shutdown", s.authWrap(s.handleShutdown))
	mux.HandleFunc("/api/host", s.authWrap(s.handlePublicHost))
	mux.HandleFunc("/api/token", s.authWrap(s.handleToken))
	mux.HandleFunc("/api/login-cred", s.authWrap(s.handleLoginCred))
	mux.HandleFunc("/api/auth-code", s.authWrap(s.handleAuthCode))
	// 获取授权码的示意图（编译进二进制，离线也能看）
	mux.HandleFunc("/help/authcode.png", s.authWrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(helpImage())
	}))
	// 悬浮球用的动图
	mux.HandleFunc("/help/volt.gif", s.authWrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(voltGIF)
	}))
	mux.HandleFunc("/", s.authWrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 页面内嵌在二进制里，别让浏览器缓存旧版本
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		w.Write([]byte(page))
	}))

	srv := &http.Server{Addr: listenAddr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// 自启管理、路由探测、打开链接等平台相关实现见 platform_*.go

const page = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>URnetwork 控制台</title>
<style>
:root{
  --bg:#141518; --side:#0e0f12; --card:#202227; --card2:#282b31; --line:rgba(255,255,255,.07);
  --text:#e9eaec; --muted:#8b8e95; --dim:#5f636b;
  --accent:#3b82f6; --ok:#34c759; --warn:#f5a524; --danger:#ef4444;
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);
  font:14px/1.55 -apple-system,BlinkMacSystemFont,"PingFang SC",Helvetica,sans-serif;
  -webkit-font-smoothing:antialiased}
.app{display:flex;min-height:100vh}
.side{width:210px;flex:0 0 210px;background:var(--side);border-right:1px solid var(--line);
  display:flex;flex-direction:column;padding:16px 12px;position:sticky;top:0;height:100vh}
.brand{display:flex;align-items:center;gap:9px;padding:2px 6px 18px}
.brand img{width:32px;height:32px;object-fit:contain}
.brand b{font-size:14px;font-weight:600}
.brand span{color:var(--dim);font-size:11px}
.nav{display:flex;align-items:center;gap:10px;padding:9px 10px;border-radius:10px;cursor:pointer;
  color:var(--muted);font-size:13.5px;user-select:none;margin-bottom:2px}
.nav:hover{background:rgba(255,255,255,.04);color:var(--text)}
.nav.on{background:var(--card2);color:var(--text);font-weight:500}
.nav svg{width:17px;height:17px;flex:0 0 17px}
.sidefoot{margin-top:auto;display:grid;gap:8px}
.refchip{font-family:ui-monospace,Menlo,monospace;font-size:11.5px;color:#9ac0fb;
  background:rgba(59,130,246,.12);border:1px solid rgba(59,130,246,.3);border-radius:8px;
  padding:6px 8px;cursor:pointer;text-align:center}
.refchip:hover{background:rgba(59,130,246,.2)}
main{flex:1;min-width:0;padding:18px 22px 96px}
.top{display:flex;align-items:center;gap:12px;margin-bottom:16px}
.top h1{font-size:17px;font-weight:600;margin:0}
.status{display:inline-flex;align-items:center;gap:7px;margin-left:auto;font-size:12.5px;
  color:var(--muted);background:var(--card);border:1px solid var(--line);border-radius:999px;padding:5px 12px}
.dot{width:7px;height:7px;border-radius:50%;background:var(--dim);display:inline-block}
.dot.on{background:var(--ok);box-shadow:0 0 0 3px rgba(52,199,89,.15)}
.dot.busy{background:var(--warn);box-shadow:0 0 0 3px rgba(245,165,36,.15)}
.dot.err{background:var(--danger);box-shadow:0 0 0 3px rgba(239,68,68,.15)}
.view{display:none}
.view.on{display:block}
</style>
<style>
.card{background:var(--card);border:1px solid var(--line);border-radius:16px;padding:16px}
.grid{display:grid;gap:14px;margin-bottom:14px}
.g3{grid-template-columns:1.7fr 1fr 1fr}
.g2{grid-template-columns:1.2fr 1fr}
@media(max-width:980px){.g3,.g2{grid-template-columns:1fr}}
.hd{display:flex;align-items:center;gap:8px;font-size:12.5px;color:var(--muted);margin-bottom:12px}
.hd .right{margin-left:auto;display:flex;gap:8px;align-items:center}
.big{font-size:26px;font-weight:600;letter-spacing:-.4px;line-height:1.25}
.sub{color:var(--muted);font-size:12px;margin-top:3px}
.chip{display:inline-flex;align-items:center;gap:5px;font-size:11.5px;padding:2px 9px;border-radius:999px;
  background:rgba(255,255,255,.06);color:var(--muted)}
.chip.ok{background:rgba(52,199,89,.14);color:#7ee2a0}
.chip.warn{background:rgba(245,165,36,.14);color:#f7c46b}
.chip.bad{background:rgba(239,68,68,.14);color:#ff8f8f}
.kv{display:flex;justify-content:space-between;gap:12px;font-size:12.5px;padding:5px 0;color:var(--muted)}
.kv b{color:var(--text);font-weight:500;max-width:64%;text-align:right;overflow:hidden;
  text-overflow:ellipsis;white-space:nowrap}
.bar{height:6px;border-radius:99px;background:rgba(255,255,255,.08);overflow:hidden;margin-top:12px}
.bar i{display:block;height:100%;width:0;border-radius:99px;
  background:linear-gradient(90deg,#3b82f6,#60a5fa);transition:width .4s}
.orb{width:72px;height:72px;border-radius:50%;border:1px solid rgba(255,255,255,.12);
  background:rgba(255,255,255,.05);display:grid;place-items:center;cursor:pointer;
  transition:.18s;flex:0 0 72px}
.orb:hover{background:rgba(255,255,255,.1);border-color:rgba(255,255,255,.2)}
.orb svg{width:26px;height:26px;fill:#e9eaec}
.orb.on{border-color:rgba(59,130,246,.5);background:rgba(59,130,246,.14)}
.orb.on svg{fill:#9ac0fb}
.btn{background:var(--card2);border:1px solid var(--line);color:var(--text);border-radius:10px;
  padding:8px 12px;font-size:12.5px;cursor:pointer;white-space:nowrap}
.btn:hover{border-color:rgba(255,255,255,.18);background:#2f3238}
.btn.p{background:var(--accent);border-color:var(--accent);color:#fff}
.btn.p:hover{background:#4b8ef7}
.btn.d{background:rgba(239,68,68,.12);border-color:rgba(239,68,68,.3);color:#ff9d9d}
.btn:disabled{opacity:.55;cursor:default}
input,select{font:inherit;font-size:13px;color:var(--text);background:#1a1c20;
  border:1px solid var(--line);border-radius:10px;padding:9px 11px;outline:none;min-width:0}
input:focus,select:focus{border-color:rgba(59,130,246,.55)}
input::placeholder{color:var(--dim)}
.row{display:flex;gap:8px;align-items:center;flex-wrap:wrap}
.row>input,.row>select{flex:1}
.rlist{display:grid;grid-template-columns:1fr 1fr;gap:4px;max-height:62vh;overflow:auto}
@media(max-width:980px){.rlist{grid-template-columns:1fr}}
.ritem{display:flex;align-items:center;gap:10px;padding:9px 12px;border-radius:10px;
  cursor:pointer;font-size:13px}
.ritem:hover{background:rgba(255,255,255,.05)}
.ritem.on{background:rgba(59,130,246,.16);color:#bcd7ff}
.ritem i{width:6px;height:6px;border-radius:50%;background:var(--dim);flex:0 0 6px}
.ritem.on i{background:var(--accent)}
.ritem span{flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.ritem b{font-weight:400;color:var(--dim);font-size:11.5px}
.ritem.on b{color:#8fb9f7}
.uri{font-family:ui-monospace,Menlo,monospace;font-size:11.5px;background:#1a1c20;
  border:1px solid var(--line);border-radius:10px;padding:10px 12px;word-break:break-all;color:#c9ccd2}
#qr{display:block;width:166px;height:166px;background:#fff;border-radius:12px;padding:6px;margin:4px auto 8px}
pre{margin:0;background:#16181c;border:1px solid var(--line);border-radius:12px;padding:10px;
  font-size:11.5px;line-height:1.5;max-height:250px;overflow:auto;white-space:pre-wrap;
  word-break:break-all;color:#9aa0a9}
.err{color:#ff9d9d;font-size:12px;min-height:16px;margin-top:8px}
.muted{color:var(--muted);font-size:12px}
.field label{display:block;font-size:11.5px;color:var(--muted);margin-bottom:6px}
.cols2{display:grid;grid-template-columns:1fr 1fr;gap:8px}
@media(max-width:560px){.cols2{grid-template-columns:1fr}}
.chk{display:flex;align-items:center;gap:7px;font-size:12px;color:var(--muted);cursor:pointer}
.chk input{width:auto;padding:0;margin:0;accent-color:var(--accent)}
details.adv{margin-top:14px;border-top:1px solid var(--line);padding-top:12px}
details.adv summary{cursor:pointer;list-style:none;font-size:12.5px;color:var(--muted);
  display:flex;align-items:center;gap:7px}
details.adv summary::-webkit-details-marker{display:none}
details.adv summary::before{content:"\203A";font-size:15px;color:var(--dim);transition:.15s}
details.adv[open] summary::before{transform:rotate(90deg)}
details.adv summary:hover{color:var(--text)}
details.adv .advbody{display:grid;gap:14px;margin-top:14px}
details.howto{margin-top:10px}
details.howto summary{cursor:pointer;list-style:none;font-size:12px;color:#7fb0f7;
  display:inline-flex;align-items:center;gap:6px}
details.howto summary::-webkit-details-marker{display:none}
details.howto summary::before{content:"?";display:inline-grid;place-items:center;width:15px;height:15px;
  border:1px solid rgba(59,130,246,.5);border-radius:50%;font-size:10px}
details.howto ol{margin:10px 0 8px;padding-left:20px;font-size:12px;color:var(--muted);line-height:1.8}
details.howto ol b{color:var(--text);font-weight:500}
details.howto a{color:#7fb0f7}
details.howto img{display:block;width:100%;max-width:290px;border-radius:10px;
  border:1px solid var(--line);margin-top:6px}
.guide{display:flex;gap:14px;align-items:flex-start}
.guide .steps{display:grid;gap:9px;flex:1}
.guide .st{display:flex;gap:9px;font-size:13px;line-height:1.55;align-items:baseline}
.guide .n{width:19px;height:19px;flex:0 0 19px;border-radius:50%;background:rgba(59,130,246,.18);
  color:#9ac0fb;font-size:11px;display:inline-grid;place-items:center;position:relative;top:2px}
.guide a{color:#7fb0f7;text-decoration:none}
.guide a:hover{text-decoration:underline}
.guide code{font-family:ui-monospace,Menlo,monospace;background:rgba(59,130,246,.14);
  border-radius:5px;padding:1px 6px;color:#9ac0fb}
.guide .x{background:none;border:none;color:var(--dim);cursor:pointer;font-size:12px;padding:2px 4px}
.guide .x:hover{color:var(--text)}
.ball{position:fixed;right:24px;bottom:24px;width:78px;height:78px;border-radius:50%;cursor:grab;
  z-index:60;touch-action:none;user-select:none;filter:drop-shadow(0 10px 24px rgba(0,0,0,.55));
  transition:transform .16s}
.ball:active{cursor:grabbing}
.ball img{width:100%;height:100%;object-fit:contain;pointer-events:none}
.ball:hover{transform:scale(1.05)}
.ballmenu{position:fixed;right:26px;bottom:114px;width:236px;background:rgba(32,34,39,.98);
  border:1px solid var(--line);border-radius:14px;padding:12px;z-index:61;
  box-shadow:0 18px 40px rgba(0,0,0,.55);display:none}
.ballmenu.on{display:block}
.ballmenu .t{font-size:12px;color:var(--muted)}
.ballmenu .row{margin-top:8px}
</style>
</head>
<body>
<div class="app">
  <aside class="side">
    <div class="brand">
      <img src="/help/volt.gif" alt="">
      <div><b>URnetwork</b><br><span>控制台</span></div>
    </div>
    <div class="nav on" data-view="dash">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6"><rect x="3" y="3" width="7" height="7" rx="1.6"/><rect x="14" y="3" width="7" height="7" rx="1.6"/><rect x="3" y="14" width="7" height="7" rx="1.6"/><rect x="14" y="14" width="7" height="7" rx="1.6"/></svg>
      仪表盘
    </div>
    <div class="nav" data-view="regions">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6"><circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.5 2.6 2.5 15.4 0 18M12 3c-2.5 2.6-2.5 15.4 0 18"/></svg>
      地区
    </div>
    <div class="nav" data-view="node">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6"><rect x="4" y="4" width="6" height="6" rx="1.4"/><rect x="14" y="14" width="6" height="6" rx="1.4"/><path d="M10 7h4a3 3 0 0 1 3 3v4"/></svg>
      节点
    </div>
    <div class="nav" data-view="settings">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6"><circle cx="12" cy="12" r="3"/><path d="M20 12h1M3 12h1M12 3v1M12 20v1M17.7 6.3l.7-.7M5.6 18.4l.7-.7M17.7 17.7l.7.7M5.6 5.6l.7.7"/></svg>
      设置
    </div>
    <div class="sidefoot">
      <div class="refchip" id="refchip" title="点击复制推荐码">推荐码 5CAI7C</div>
      <button class="btn" id="logout">退出登录</button>
    </div>
  </aside>

  <main>
    <div class="top">
      <h1 id="page-title">仪表盘</h1>
      <span class="status"><i class="dot" id="dot"></i><span id="stext">连接中…</span></span>
    </div>
    <section class="view on" id="view-dash">
      <div class="card guide" id="guide" style="margin-bottom:14px">
        <div class="steps">
          <div class="st"><span class="n">1</span><span>打开 <a href="https://ur.io/" target="_blank" rel="noopener">ur.io</a> 登录（用 Google 账号），点右上角头像 → ACCOUNT TOOLS → <b>Copy Auth Code</b>；还没账号先注册，推荐码 <code>5CAI7C</code>（双方有奖励）</span></div>
          <div class="st"><span class="n">2</span><span>把授权码粘到「节点」页的「换凭据」里，60 秒内点「用授权码登录」<span class="muted">（码只能用一次）</span></span></div>
          <div class="st"><span class="n">3</span><span>装 <a href="https://apps.apple.com/us/app/shadowrocket/id932747118" target="_blank" rel="noopener">Shadowrocket</a> <span class="muted">（付费，需美区账号）</span>，扫节点二维码导入，分流规则在客户端里配</span></div>
        </div>
        <button class="x" id="guide-x">收起</button>
      </div>

      <div class="grid g3">
        <div class="card">
          <div class="hd"><span>状态</span><span class="chip" id="c-status">未运行</span></div>
          <div style="display:flex;align-items:center;gap:18px">
            <div style="min-width:0;flex:1">
              <div class="big" id="s-country">—</div>
              <div class="sub" id="s-exit">出口 IP</div>
              <div style="display:flex;gap:22px;margin-top:14px">
                <div><div class="muted" style="font-size:11px">运行时长</div><div id="s-up" style="font-size:14px">—</div></div>
                <div><div class="muted" style="font-size:11px">端口 / PID</div><div id="s-pid" style="font-size:14px">—</div></div>
              </div>
            </div>
            <div class="orb" id="orb" title="启动 / 停止"><svg viewBox="0 0 24 24"><path d="M8 5.5v13l11-6.5z"/></svg></div>
          </div>
        </div>

        <div class="card">
          <div class="hd"><span>探测耗时</span></div>
          <div class="big sm" id="s-lat">—</div>
          <div class="sub">走代理请求 ipinfo，含建连</div>
        </div>

        <div class="card">
          <div class="hd"><span>凭据</span><span class="chip" id="c-jwt">—</span></div>
          <div class="big sm" id="s-jwt">—</div>
          <div class="sub">到期后重新拿授权码</div>
          <div class="bar"><i id="jwtbar"></i></div>
        </div>
      </div>

      <div class="grid g2">
        <div class="card">
          <div class="hd"><span>快速切换出口</span><div class="right"><span class="muted" id="seg-now">—</span></div></div>
          <div class="row" id="quick"></div>
          <div class="muted" style="margin-top:12px">点一下即切换出口，失败会自动回滚到原来的地区；完整列表在「地区」页。</div>
        </div>
        <div class="card">
          <div class="hd"><span>本地入口</span></div>
          <div class="kv"><span>代理端口</span><b>socks5 · 9999</b></div>
          <div class="kv"><span>面板地址</span><b id="s-panel">—</b></div>
          <div class="row" style="margin-top:10px">
            <button class="btn" id="copy2">复制节点地址</button>
            <button class="btn" id="goto-node">去节点页</button>
          </div>
        </div>
      </div>

      <div class="card">
        <div class="hd"><span>实时日志</span><div class="right"><span class="muted">每 2.5 秒刷新</span></div></div>
        <pre id="logs"></pre>
      </div>
    </section>
    <section class="view" id="view-regions">
      <div class="card">
        <div class="hd"><span>出口地区</span><div class="right"><span class="muted" id="region-count"></span></div></div>
        <div class="row" style="margin-bottom:12px">
          <input id="q" placeholder="搜索地区，点一下即切换">
          <button class="btn" id="refresh">刷新列表</button>
          <button class="btn" id="start">启动</button>
          <button class="btn d" id="stop">停止</button>
        </div>
        <div class="rlist" id="regions"></div>
      </div>
    </section>

    <section class="view" id="view-node">
      <div class="grid g2">
        <div class="card">
          <div class="hd"><span>节点</span></div>
          <div class="row" style="margin-bottom:10px">
            <select id="host"></select>
            <button class="btn" id="copy">复制 URI</button>
          </div>
          <div class="row" style="margin-bottom:10px">
            <button class="btn p" id="push">导入 Shadowrocket</button>
            <button class="btn" id="pushclash">导入 Clash Verge</button>
            <button class="btn" id="pushflclash">导入 FlClash</button>
          </div>
          <div class="uri" id="uri">—</div>
          <div class="err" id="err"></div>

          <div class="field" style="margin-top:16px">
            <label>换凭据：粘贴授权码（App → Account Tools → Copy Auth Code，1 分钟有效）</label>
            <div class="row">
              <input id="authcode" placeholder="粘贴一次性授权码，越快越好">
              <button class="btn p" id="docode">用授权码登录</button>
            </div>
            <details class="howto">
              <summary>怎么获取授权码？</summary>
              <ol>
                <li>浏览器打开 <a href="https://ur.io/" target="_blank" rel="noopener">ur.io</a> 并登录（Google 账号即可）。</li>
                <li>点右上角头像（显示你的账户名），展开账户面板。</li>
                <li>往下拉到 <b>ACCOUNT TOOLS</b>，点 <b>Copy Auth Code</b>。</li>
                <li>回到这里粘贴，60 秒内点「用授权码登录」。码只能用一次，过期就再复制一个。</li>
              </ol>
              <img src="/help/authcode.png" alt="ACCOUNT TOOLS 里 Copy Auth Code 按钮的位置">
            </details>
          </div>
        </div>

        <div class="card">
          <div class="hd"><span>扫码导入</span></div>
          <img id="qr" alt="节点二维码">
          <div class="muted" style="text-align:center">手机：用 Shadowrocket 扫这张图</div>
        </div>
      </div>
    </section>

    <section class="view" id="view-settings">
      <div class="card">
        <div class="hd"><span>凭据与地址</span><div class="right"><span class="muted" id="tokeninfo"></span></div></div>
        <div class="advbody" style="display:grid;gap:14px">
          <div class="field">
            <label>对外地址（手机 / 远端要用哪个地址）</label>
            <div class="row"><input id="pubhost" placeholder="IP / 域名 / auto"><button class="btn" id="savehost">保存</button></div>
          </div>
          <div class="field">
            <label>凭据 JWT</label>
            <div class="row"><input id="token" placeholder="粘贴 JWT 或 .auth_state 内容"><button class="btn" id="savetoken">更新</button></div>
          </div>
          <div class="field">
            <label>或者用账号密码向官方接口换凭据</label>
            <div class="cols2"><input id="uauser" placeholder="账号（邮箱/用户 ID）"><input id="uapass" type="password" placeholder="密码" autocomplete="off"></div>
            <div class="row" style="margin-top:8px">
              <label class="chk"><input type="checkbox" id="remember"><span>记住，快过期时自动续期</span></label>
              <button class="btn" id="dologin" style="margin-left:auto">账号登录</button>
            </div>
          </div>
          <div class="field" style="border-top:1px solid var(--line);padding-top:14px">
            <label>进程</label>
            <div class="row"><button class="btn d" id="shutdown">停止服务并退出</button></div>
            <div class="muted" style="margin-top:6px">装过开机自启的话停掉会被拉起；要彻底停先执行 urnetd uninstall</div>
          </div>
        </div>
      </div>
    </section>
  </main>
</div>

<div class="ball" id="ball"><img src="/help/volt.gif" alt="Volt"></div>
<div class="ballmenu" id="ballmenu">
  <div class="t">快捷操作</div>
  <div class="row"><span class="chip" id="bm-status">—</span><span class="muted" id="bm-region">—</span></div>
  <div class="row">
    <button class="btn p" id="bm-toggle" style="flex:1">启动</button>
    <button class="btn" id="bm-node" style="flex:1">节点</button>
  </div>
</div>
<script>
var S=null, HOST='', REG=[], lastNodeKey='';
function $(id){return document.getElementById(id)}
function setText(id,v){var e=$(id); if(e) e.textContent=v}
function setHTML(id,v){var e=$(id); if(e) e.innerHTML=v}
async function get(u){var r=await fetch(u); if(r.status===401){location.href='/login';throw new Error('401')} return r.json()}
async function post(u,b){var r=await fetch(u,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b||{})}); if(r.status===401){location.href='/login';throw new Error('401')} return r.json()}
function fmtUp(s){if(s==null||s<0) return '—'; var h=Math.floor(s/3600),m=Math.floor(s%3600/60); return (h?h+'h ':'')+m+'m'}
function esc(s){return String(s==null?'':s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;')}

var TITLES={dash:'仪表盘',regions:'地区',node:'节点',settings:'设置'};
function showView(v){
  var i, vs=document.querySelectorAll('.view');
  for(i=0;i<vs.length;i++) vs[i].classList.remove('on');
  var t=$('view-'+v); if(t) t.classList.add('on');
  var ns=document.querySelectorAll('.nav');
  for(i=0;i<ns.length;i++) ns[i].classList.toggle('on', ns[i].getAttribute('data-view')===v);
  setText('page-title', TITLES[v]||'仪表盘');
  try{ localStorage.setItem('urnetdView', v); }catch(e){}
  if(v==='node'){ lastNodeKey=''; paintNode(); }
  if(v==='dash'){ paintQuick(); }
}

var QUICK=['Japan','United States','Singapore','Germany'];
function paintQuick(){
  if(!S) return;
  var html='', i, c;
  for(i=0;i<QUICK.length;i++){
    c=QUICK[i];
    html+='<button class="btn '+(c===S.country?'p':'')+'" data-c="'+esc(c)+'">'+esc(c)+'</button>';
  }
  setHTML('quick', html);
  setText('seg-now', S.country?('当前 '+S.country):'—');
}

function paint(){
  if(!S) return;
  var d=$('dot'), t=$('stext');
  var online=S.running&&S.ready;
  if(d){ d.className='dot'+(S.busy?' busy':(S.lastErr&&!S.running?' err':(online?' on':''))); }
  setText('stext', S.busy?'切换中…':(S.lastErr&&!S.running?'异常':(online?'运行中':(S.running?'启动中…':'已停止'))));
  var cs=$('c-status');
  if(cs){ cs.className='chip '+(S.busy?'warn':(online?'ok':'bad')); cs.textContent=S.busy?'切换中':(online?'运行中':(S.running?'启动中':'未运行')); }
  setText('s-country', S.country||'—');
  setText('s-exit', S.exitIp?(S.exitIp+(S.exitCity?' · '+S.exitCity:'')):'还没有探测到出口');
  setText('s-up', S.uptimeSec!=null?fmtUp(S.uptimeSec):'—');
  setText('s-pid', (S.port||'—')+' / '+(S.pid||'—'));
  setText('s-lat', S.latencyMs?S.latencyMs+' ms':'—');
  setText('s-jwt', S.jwtDays>=0?S.jwtDays+' 天':'未知');
  setText('s-panel', 'http://127.0.0.1:'+(location.port||'8877'));
  var cj=$('c-jwt');
  if(cj){ cj.className='chip '+(S.jwtDays<0?'':(S.jwtDays<7?'warn':'ok')); cj.textContent=S.jwtDays<0?'未知':(S.jwtDays<7?'即将过期':'有效'); }
  var b=$('jwtbar'); if(b){ var p=Math.max(0,Math.min(100,Math.round((S.jwtDays/30)*100))); b.style.width=(S.jwtDays<0?0:p)+'%'; }
  var orb=$('orb'); if(orb){ orb.classList.toggle('on', !!online); }
  setText('err', S.lastErr||'');
  var push=$('push'); if(push){ push.style.display=(S.canPush===false)?'none':'inline-block'; }
  paintQuick();
  buildHosts(); paintRegions(); paintNode();
  var bm=$('bm-status'); if(bm){ bm.className='chip '+(online?'ok':'bad'); bm.textContent=online?'运行中':'未运行'; }
  setText('bm-region', S.country||'');
  setText('bm-toggle', online?'停止':'启动');
  setText('region-count', S.regionCount?('共 '+S.regionCount+' 个地区'):'');
}
function buildHosts(){
  if(!S.addrs) return;
  var sel=$('host'); if(!sel) return;
  var list=S.addrs.slice();
  if(S.nodeHost && list.indexOf(S.nodeHost)<0) list.unshift(S.nodeHost);
  if(!HOST) HOST=S.nodeHost||S.primary||list[0];
  if(list.indexOf(HOST)<0) list.unshift(HOST);
  var want=list.join(',');
  if(sel.getAttribute('data-list')!==want){
    sel.innerHTML='';
    for(var i=0;i<list.length;i++){
      var a=list[i], o=document.createElement('option');
      o.value=a;
      o.textContent=a+(a===S.nodeHost?'（对外地址）':(a===S.primary?'（主网卡）':''));
      sel.appendChild(o);
    }
    sel.setAttribute('data-list',want);
  }
  sel.value=HOST;
  var ph=$('pubhost'); if(ph && document.activeElement!==ph) ph.value=S.publicHost||'';
}

async function paintNode(){
  if(!S) return;
  var key=(S.country||'')+'|'+HOST;
  if(key===lastNodeKey) return;
  lastNodeKey=key;
  try{
    var r=await get('/api/node?host='+encodeURIComponent(HOST));
    setText('uri', r.uri);
    var q=$('qr');
    if(q){ q.src='/api/qr?host='+encodeURIComponent(HOST)+'&t='+Date.now(); q.style.display='block'; }
  }catch(e){}
}

function paintRegions(){
  var box=$('regions'); if(!box) return;
  var q=($('q')&&$('q').value||'').toLowerCase();
  var cur=S?S.country:'';
  var html='';
  for(var i=0;i<REG.length;i++){
    var r=REG[i];
    if(q && r.name.toLowerCase().indexOf(q)<0) continue;
    html+='<div class="ritem'+(r.name===cur?' on':'')+'" data-n="'+esc(r.name)+'"><i></i><span>'+esc(r.name)+'</span><b>'+r.count+'</b></div>';
  }
  box.innerHTML=html||'<div class="muted" style="padding:10px">没有匹配的地区</div>';
}

async function loadRegions(force){
  try{ REG=await get('/api/regions'+(force?'?refresh=1':'')); paintRegions(); }catch(e){}
}

async function poll(){
  try{ S=await get('/api/state'); paint(); }
  catch(e){ setText('stext','后端无响应'); }
}

var lock=false;
async function switchTo(name){
  if(lock) return;
  lock=true;
  setText('stext','切换到 '+name+'…');
  try{ await post('/api/switch',{country:name}); }catch(e){}
  setTimeout(function(){lock=false},2000);
}

async function logs(){
  try{
    var L=await get('/api/logs?tail=120');
    var el=$('logs'); if(!el) return;
    var bottom=(el.scrollTop+el.clientHeight>=el.scrollHeight-24);
    el.textContent=(L||[]).join('\n');
    if(bottom) el.scrollTop=el.scrollHeight;
  }catch(e){}
}

async function loadToken(){
  try{
    var t=await get('/api/token');
    if(t.userAuth){ if($('uauser')) $('uauser').value=t.userAuth; if($('remember')) $('remember').checked=true; }
    var auto=t.hasCred?'　· 已存账号，将自动续期':'';
    var box=$('tokeninfo');
    if(box){
      if(t.error){ box.textContent='凭据: 读不到 —— '+t.error; box.style.color='#ff9d9d'; }
      else if(t.valid===false){ box.textContent='凭据已过期（'+t.expiresAt+'），请更新'+auto; box.style.color='#ff9d9d'; }
      else { box.textContent='有效期至 '+t.expiresAt+'（剩 '+t.daysLeft+' 天）· '+t.network+auto; box.style.color=''; }
    }
  }catch(e){}
}
function bindPush(id, endpoint, label, confirmText){
  var b=$(id); if(!b) return;
  b.addEventListener('click',async function(){
    if(confirmText && !confirm(confirmText)) return;
    b.disabled=true; b.textContent='发送中…';
    try{
      var r=await post(endpoint+'?host='+encodeURIComponent(HOST));
      if(r.ok){ b.textContent='已发送，去弹窗确认'; setText('err','订阅地址: '+r.subUrl); }
      else { b.textContent='导入失败'; setText('err', (r.error||'open 失败')); }
    }catch(e){ b.textContent='导入失败'; }
    setTimeout(function(){b.textContent=label;b.disabled=false},2500);
  });
}

function on(id, ev, fn){ var e=$(id); if(e) e.addEventListener(ev, fn); }

on('regions','click',function(e){
  var n=e.target.closest('.ritem');
  if(n && n.getAttribute('data-n')) switchTo(n.getAttribute('data-n'));
});
on('quick','click',function(e){
  var b=e.target.closest('button');
  if(b && b.getAttribute('data-c')) switchTo(b.getAttribute('data-c'));
});
on('q','input',paintRegions);
on('refresh','click',function(){loadRegions(true)});
on('host','change',function(){HOST=this.value;lastNodeKey='';paintNode()});
on('copy','click',function(){ copyText($('uri').textContent, this, '复制 URI'); });
on('copy2','click',function(){ copyText($('uri').textContent, this, '复制节点地址'); });
function copyText(t, btn, label){
  if(!t) return;
  if(navigator.clipboard) navigator.clipboard.writeText(t);
  btn.textContent='已复制';
  setTimeout(function(){btn.textContent=label},1200);
}
on('goto-node','click',function(){ showView('node'); });
on('orb','click',async function(){
  if(!S) return;
  if(S.running) await fetch('/api/stop',{method:'POST'});
  else await fetch('/api/start',{method:'POST'});
  poll();
});
on('stop','click',async function(){ await fetch('/api/stop',{method:'POST'}); poll(); });
on('start','click',async function(){ await fetch('/api/start',{method:'POST'}); poll(); });
on('logout','click',async function(){ try{ await fetch('/api/logout',{method:'POST'}); }catch(e){} location.href='/login'; });
on('refchip','click',function(){
  var b=this; if(navigator.clipboard) navigator.clipboard.writeText('5CAI7C');
  b.textContent='已复制 5CAI7C';
  setTimeout(function(){b.textContent='推荐码 5CAI7C'},1400);
});
on('guide-x','click',function(){ $('guide').style.display='none'; try{localStorage.setItem('urnetdGuide','0')}catch(e){} });
on('savehost','click',async function(){
  var b=this, old='保存';
  b.textContent='保存中…';
  try{
    var r=await post('/api/host',{host:$('pubhost').value.trim()});
    if(r.error){ setText('err', r.error); b.textContent='失败'; }
    else { b.textContent='已保存'; HOST=''; lastNodeKey=''; poll(); }
  }catch(e){ b.textContent='失败'; }
  setTimeout(function(){b.textContent=old},1400);
});
on('savetoken','click',async function(){
  var b=this, old='更新', t=$('token').value.trim();
  if(!t){ setText('err','先粘贴 token'); return; }
  b.textContent='校验中…';
  try{
    var r=await post('/api/token',{token:t});
    if(r.error){ setText('err', r.error); b.textContent='失败'; }
    else { b.textContent='已更新'; $('token').value=''; setText('tokeninfo','已更新至 '+r.expiresAt); loadToken(); setTimeout(poll,1500); }
  }catch(e){ b.textContent='失败'; }
  setTimeout(function(){b.textContent=old},1600);
});
on('docode','click',async function(){
  var b=this, old='用授权码登录', c=$('authcode').value.trim();
  if(!c){ setText('err','先粘贴授权码（1 分钟内有效）'); return; }
  b.textContent='登录中…';
  try{
    var r=await post('/api/auth-code',{code:c});
    if(r.error){ setText('err','授权码登录失败: '+r.error); b.textContent='失败'; }
    else { b.textContent='已登录'; $('authcode').value=''; setText('err','登录成功，凭据至 '+r.expiresAt+'，代理重启中…'); loadToken(); setTimeout(poll,1500); }
  }catch(e){ b.textContent='失败'; }
  setTimeout(function(){b.textContent=old},1800);
});
on('dologin','click',async function(){
  var b=this, old='账号登录';
  var ua=$('uauser').value.trim(), pw=$('uapass').value;
  if(!ua||!pw){ setText('err','账号和密码都要填'); return; }
  b.textContent='登录中…'; setText('err','');
  try{
    var r=await post('/api/login-cred',{userAuth:ua,password:pw,remember:$('remember').checked});
    if(r.error){ setText('err','登录失败: '+r.error); b.textContent='失败'; }
    else { b.textContent='已登录'; $('uapass').value=''; setText('err','登录成功，凭据至 '+r.expiresAt); loadToken(); setTimeout(poll,1500); }
  }catch(e){ b.textContent='失败'; }
  setTimeout(function(){b.textContent=old},1800);
});
on('shutdown','click',async function(){
  if(!confirm('停止代理并退出 urnetd？之后需要重新打开 app 才会恢复。')) return;
  try{ await fetch('/api/shutdown',{method:'POST'}); }catch(e){}
  document.body.innerHTML='<div style="padding:80px 20px;text-align:center;color:#8b8e95;font-size:14px">服务已停止，可以关闭这个页面了。<br><span style="font-size:12px">重新打开控制台：双击 URnetwork Console.app</span></div>';
});
bindPush('push','/api/push','导入 Shadowrocket','');
bindPush('pushclash','/api/push-clash','导入 Clash Verge','会把「URnetwork」订阅加进 Clash Verge 并切换为当前配置。\n原配置不会被删除，之后可以切回去。继续？');
bindPush('pushflclash','/api/push-flclash','导入 FlClash','会把「URnetwork」订阅加进 FlClash 并切换为当前配置。\n原配置不会被删除，之后可以切回去。继续？');

var navs=document.querySelectorAll('.nav');
for(var i=0;i<navs.length;i++){
  navs[i].addEventListener('click',function(){ showView(this.getAttribute('data-view')); });
}

(function(){
  var ball=$('ball'), menu=$('ballmenu');
  if(!ball||!menu) return;
  var pos=null;
  try{ pos=JSON.parse(localStorage.getItem('urnetdBallPos')||'null'); }catch(e){}
  if(pos && typeof pos.r==='number' && typeof pos.b==='number'){
    ball.style.right=pos.r+'px'; ball.style.bottom=pos.b+'px';
    menu.style.right=(pos.r+2)+'px'; menu.style.bottom=(pos.b+90)+'px';
  }
  var dragging=false, moved=false, sx=0, sy=0, sr=0, sb=0;
  ball.addEventListener('pointerdown',function(e){
    dragging=true; moved=false;
    sx=e.clientX; sy=e.clientY;
    sr=parseInt(getComputedStyle(ball).right,10)||24;
    sb=parseInt(getComputedStyle(ball).bottom,10)||24;
    ball.setPointerCapture(e.pointerId);
  });
  ball.addEventListener('pointermove',function(e){
    if(!dragging) return;
    var dx=sx-e.clientX, dy=sy-e.clientY;
    if(Math.abs(dx)>6||Math.abs(dy)>6) moved=true;
    if(!moved) return;
    var r=Math.max(8,Math.min(window.innerWidth-90, sr+dx));
    var b=Math.max(8,Math.min(window.innerHeight-90, sb+dy));
    ball.style.right=r+'px'; ball.style.bottom=b+'px';
    menu.style.right=(r+2)+'px'; menu.style.bottom=(b+90)+'px';
  });
  ball.addEventListener('pointerup',function(e){
    if(!dragging) return;
    dragging=false;
    if(moved){
      try{
        localStorage.setItem('urnetdBallPos', JSON.stringify({
          r:parseInt(ball.style.right,10), b:parseInt(ball.style.bottom,10)
        }));
      }catch(err){}
      return;
    }
    menu.classList.toggle('on');
  });
  on('bm-toggle','click',async function(){
    if(!S) return;
    if(S.running) await fetch('/api/stop',{method:'POST'});
    else await fetch('/api/start',{method:'POST'});
    poll();
  });
  on('bm-node','click',function(){ showView('node'); menu.classList.remove('on'); });
  document.addEventListener('click',function(e){
    if(!menu.classList.contains('on')) return;
    if(menu.contains(e.target)||ball.contains(e.target)) return;
    menu.classList.remove('on');
  });
})();

(function(){
  var hideGuide=false;
  try{ hideGuide=localStorage.getItem('urnetdGuide')==='0'; }catch(e){}
  if(hideGuide && $('guide')) $('guide').style.display='none';
  var v='dash';
  try{ v=localStorage.getItem('urnetdView')||'dash'; }catch(e){}
  if(!TITLES[v]) v='dash';
  showView(v);
})();

poll(); loadRegions(false); logs(); loadToken();
setInterval(poll,2000);
setInterval(logs,2500);
</script>
</body>
</html>
`
