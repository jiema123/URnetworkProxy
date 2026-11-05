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
	"syscall"
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

// 优先用 app 包里自带的 socksproxy（打包成 .app 时放在 Contents/Resources），
// 找不到再退回开发机上的路径。
func defaultBinPath() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "..", "Resources", "socksproxy")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
		}
	}
	return filepath.Join(home(), "Dev/personal/tools/bin/socksproxy")
}

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
	// 端口被脚本或旧进程占着时，按可执行文件路径杀掉
	out, err := exec.Command("pgrep", "-f", binPath).Output()
	if err != nil {
		return
	}
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	time.Sleep(600 * time.Millisecond)
}

func (s *Server) startLocked(country string) error {
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("找不到 socksproxy: %s", binPath)
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
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
	syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	if done != nil {
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
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
	fmt.Printf("urnetd 监听 http://%s\n", listenAddr)
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
:root{color-scheme:dark}
*{box-sizing:border-box}
body{margin:0;background:#1b1b1a;color:#e8e6e0;font:14px/1.6 -apple-system,BlinkMacSystemFont,"PingFang SC",Helvetica,sans-serif}
header{display:flex;align-items:center;gap:10px;padding:12px 20px;border-bottom:1px solid #33322f;position:sticky;top:0;background:#1b1b1a;z-index:5}
h1{font-size:15px;font-weight:500;margin:0}
#dot{width:8px;height:8px;border-radius:50%;background:#5f5e5a}
#dot.on{background:#97c459}
#dot.busy{background:#ef9f27}
#dot.err{background:#f09595}
#stext{font-size:13px;color:#a8a69e}
main{padding:14px 20px 24px;max-width:1180px;margin:0 auto}
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(128px,1fr));gap:10px;margin-bottom:14px}
.stat{background:#232322;border:1px solid #33322f;border-radius:10px;padding:9px 12px}
.stat .k{font-size:11px;color:#8b8980}
.stat .v{font-size:15px;font-weight:500;margin-top:2px;word-break:break-all}
.cols{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1.1fr);gap:14px}
@media(max-width:820px){.cols{grid-template-columns:1fr}}
.card{background:#232322;border:1px solid #33322f;border-radius:12px;padding:12px 14px}
.card h2{font-size:13px;font-weight:500;margin:0 0 10px;color:#a8a69e}
input,select,button{font:inherit;color:inherit;background:#2c2c2a;border:1px solid #3d3c39;border-radius:8px;padding:6px 10px;outline:none}
button{cursor:pointer}
button:hover{border-color:#5f5e5a}
button.p{background:#185fa5;border-color:#185fa5;color:#fff}
button.d{background:#4a1b0c;border-color:#993c1d;color:#f5c4b3}
.tools{display:flex;gap:8px;margin-bottom:10px;flex-wrap:wrap;align-items:center}
#regions{list-style:none;margin:0;padding:0;max-height:430px;overflow:auto}
#regions li{display:flex;justify-content:space-between;gap:10px;padding:6px 10px;border-radius:8px;cursor:pointer;font-size:13px}
#regions li:hover{background:#2c2c2a}
#regions li.cur{background:#0c447c;color:#b5d4f4}
#regions li b{font-weight:400;color:#8b8980;font-size:12px}
#regions li.cur b{color:#85b7eb}
.uri{font-family:ui-monospace,Menlo,monospace;font-size:11px;line-height:1.5;background:#1b1b1a;border:1px solid #33322f;border-radius:8px;padding:8px;word-break:break-all;margin-bottom:8px}
pre{margin:0;background:#1b1b1a;border:1px solid #33322f;border-radius:8px;padding:9px;font-size:11px;line-height:1.45;max-height:200px;overflow:auto;white-space:pre-wrap;word-break:break-all;color:#a8a69e}
#qr{display:block;width:176px;height:176px;background:#fff;border-radius:8px;padding:6px;margin:4px auto 10px}
.err{color:#f09595;font-size:12px;min-height:16px}
.refchip{font-family:ui-monospace,Menlo,monospace;font-size:12px;color:#97c459;background:#173404;border:1px solid #3b6d11;border-radius:6px;padding:3px 9px;cursor:pointer;white-space:nowrap}
.refchip:hover{border-color:#639922}
.guide{background:#232322;border:1px solid #33322f;border-radius:12px;padding:12px 14px;margin-bottom:14px;display:flex;gap:14px;align-items:flex-start}
.guide .steps{display:grid;gap:9px;flex:1}
.guide .st{display:flex;gap:9px;font-size:13px;line-height:1.5;align-items:baseline}
.guide .n{width:18px;height:18px;flex:0 0 18px;border-radius:50%;background:#0c447c;color:#b5d4f4;font-size:11px;display:inline-flex;align-items:center;justify-content:center;position:relative;top:2px}
.guide a{color:#85b7eb;text-decoration:none;border-bottom:1px solid #185fa5}
.guide a:hover{color:#b5d4f4;border-color:#85b7eb}
.guide .muted{color:#8b8980;font-size:12px}
.guide code{font-family:ui-monospace,Menlo,monospace;background:#1b1b1a;border:1px solid #3d3c39;border-radius:6px;padding:1px 7px;color:#97c459;letter-spacing:1px}
.guide .x{background:none;border:none;color:#8b8980;cursor:pointer;font-size:12px;padding:2px 6px;white-space:nowrap}
.guide .x:hover{color:#e8e6e0;border:none}
details.adv{margin-top:14px;border-top:1px solid #33322f;padding-top:10px}
details.adv summary{cursor:pointer;list-style:none;font-size:12px;color:#a8a69e;display:flex;align-items:center;gap:6px;padding:4px 0}
details.adv summary:hover{color:#e8e6e0}
details.adv summary::-webkit-details-marker{display:none}
details.adv summary::before{content:"▸";font-size:10px;color:#5f5e5a}
details.adv[open] summary::before{content:"▾"}
details.adv[open] summary{color:#a8a69e;margin-bottom:12px}
details.adv .advbody{display:grid;gap:12px}
details.adv .field label{display:block;font-size:11px;color:#8b8980;margin-bottom:5px}
.cols2{display:grid;grid-template-columns:1fr 1fr;gap:8px}
.cols2 input{min-width:0}
.chk{display:flex;align-items:center;gap:6px;font-size:12px;color:#8b8980;cursor:pointer}
.chk input{width:auto;padding:0;margin:0}
@media(max-width:520px){.cols2{grid-template-columns:1fr}}
details.howto{margin-top:8px}
details.howto summary{cursor:pointer;list-style:none;font-size:12px;color:#85b7eb;display:inline-flex;align-items:center}
details.howto summary:hover{color:#b5d4f4}
details.howto summary::-webkit-details-marker{display:none}
details.howto summary::before{content:"?";display:inline-flex;align-items:center;justify-content:center;width:14px;height:14px;border:1px solid #185fa5;border-radius:50%;font-size:10px;margin-right:5px;color:#85b7eb}
details.howto ol{margin:8px 0 6px;padding-left:20px;font-size:12px;color:#a8a69e;line-height:1.75}
details.howto ol b{color:#e8e6e0;font-weight:500}
details.howto a{color:#85b7eb}
details.howto img{display:block;width:100%;max-width:300px;border-radius:8px;border:1px solid #33322f;margin-top:4px}
</style>
</head>
<body>
<header>
  <h1>URnetwork 控制台</h1>
  <span id="dot"></span><span id="stext">连接中…</span>
  <span style="flex:1"></span>
  <span class="refchip" id="refchip" title="点击复制推荐码">推荐码 5CAI7C</span>
  <button id="guide-tog" style="display:none">使用引导</button>
  <button id="logout" title="退出登录">退出</button>
</header>
<main>
  <div class="guide" id="guide">
    <div class="steps">
      <div class="st"><span class="n">1</span><span>打开 <a href="https://ur.io/" target="_blank" rel="noopener">ur.io</a> 登录（用 Google 账号），点右上角头像 → ACCOUNT TOOLS → <b>Copy Auth Code</b>；还没账号先注册，推荐码 <code>5CAI7C</code>（双方有奖励）</span></div>
      <div class="st"><span class="n">2</span><span>把授权码粘到下面的「换凭据」里，60 秒内点「用授权码登录」<span class="muted">（码只能用一次）</span></span></div>
      <div class="st"><span class="n">3</span><span>装 <a href="https://apps.apple.com/us/app/shadowrocket/id932747118" target="_blank" rel="noopener">Shadowrocket</a> <span class="muted">（付费，需美区账号）</span>，扫右侧二维码导入节点，分流规则在小火箭里配</span></div>
    </div>
    <button class="x" id="guide-x">收起</button>
  </div>
  <div class="stats">
    <div class="stat"><div class="k">当前地区</div><div class="v" id="s-country">—</div></div>
    <div class="stat"><div class="k">出口 IP</div><div class="v" id="s-exit">—</div></div>
    <div class="stat"><div class="k">延迟</div><div class="v" id="s-lat">—</div></div>
    <div class="stat"><div class="k">端口 / PID</div><div class="v" id="s-pid">—</div></div>
    <div class="stat"><div class="k">凭据剩余</div><div class="v" id="s-jwt">—</div></div>
    <div class="stat"><div class="k">已运行</div><div class="v" id="s-up">—</div></div>
  </div>
  <div class="cols">
    <div class="card">
      <h2>地区（点一下即切换）</h2>
      <div class="tools">
        <input id="q" placeholder="搜索地区" style="flex:1;min-width:110px">
        <button id="refresh">刷新</button>
        <button class="d" id="stop">停止</button>
        <button class="p" id="start">启动</button>
      </div>
      <ul id="regions"></ul>
    </div>
    <div class="card">
      <h2>节点</h2>
      <div class="tools">
        <select id="host" style="flex:1"></select>
      </div>
      <div class="tools">
        <button id="copy">复制 URI</button>
        <button class="p" id="push">导入 Shadowrocket</button>
        <button id="pushclash">导入 Clash Verge</button>
        <button id="pushflclash">导入 FlClash</button>
      </div>
      <div class="uri" id="uri">—</div>
      <img id="qr" alt="节点二维码">
      <div class="muted" style="text-align:center;margin-top:-4px">手机：用 Shadowrocket 扫这张图</div>
      <div class="err" id="err"></div>
      <div class="field" style="margin-top:12px">
        <label>换凭据：粘贴授权码（App → Account Tools → Copy Auth Code，1 分钟有效）</label>
        <div class="tools" style="margin:0">
          <input id="authcode" placeholder="粘贴一次性授权码，越快越好" style="flex:1">
          <button class="p" id="docode">用授权码登录</button>
        </div>
        <details class="howto">
          <summary>怎么获取授权码？</summary>
          <ol>
            <li>浏览器打开 <a href="https://ur.io/" target="_blank" rel="noopener">ur.io</a> 并登录（用你注册时的 Google 账号即可）。</li>
            <li>点右上角头像（显示你的账户名），展开账户面板。</li>
            <li>往下拉到 <b>ACCOUNT TOOLS</b>，点 <b>Copy Auth Code</b>。</li>
            <li>回到这里粘贴，60 秒内点「用授权码登录」。码只能用一次，过期就再复制一个。</li>
          </ol>
          <img src="/help/authcode.png" alt="ACCOUNT TOOLS 里 Copy Auth Code 按钮的位置">
        </details>
      </div>
      <details class="adv" id="adv">
        <summary><span id="advsum">更多设置</span></summary>
        <div class="advbody">
          <div class="field">
            <label>对外地址（手机/远端要用哪个地址）</label>
            <div class="tools" style="margin:0">
              <input id="pubhost" placeholder="IP / 域名 / auto" style="flex:1">
              <button id="savehost">保存</button>
            </div>
          </div>
          <div class="field">
            <label>凭据 JWT</label>
            <div class="tools" style="margin:0">
              <input id="token" placeholder="粘贴 JWT 或 .auth_state 内容" style="flex:1">
              <button id="savetoken">更新</button>
            </div>
          </div>
          <div class="field">
            <label>或者用账号密码向官方接口换凭据</label>
            <div class="cols2">
              <input id="uauser" placeholder="账号（邮箱/用户 ID）">
              <input id="uapass" type="password" placeholder="密码" autocomplete="off">
            </div>
            <div class="tools" style="margin:8px 0 0">
              <label class="chk"><input type="checkbox" id="remember"><span>记住，快过期时自动续期</span></label>
              <button id="dologin" style="margin-left:auto">账号登录</button>
            </div>
          </div>
          <div class="muted" id="tokeninfo"></div>
          <div class="field" style="border-top:1px solid #33322f;padding-top:10px">
            <label>进程</label>
            <div class="tools" style="margin:0">
              <button class="d" id="shutdown">停止服务并退出</button>
            </div>
            <div class="muted" style="margin-top:6px">如果装了开机自启，停掉后会被自动拉起；要彻底停先执行 urnetd uninstall</div>
          </div>
        </div>
      </details>
      <h2 style="margin-top:12px">日志</h2>
      <pre id="logs"></pre>
    </div>
  </div>
</main>
<script>
var S=null, HOST='', REG=[], lastNodeKey='';
function $(id){return document.getElementById(id)}
async function get(u){var r=await fetch(u); if(r.status===401){location.href='/login';throw new Error('401')} return r.json()}
async function post(u,b){var r=await fetch(u,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b||{})}); if(r.status===401){location.href='/login';throw new Error('401')} return r.json()}
function fmtUp(s){if(!s&&s!==0) return '—'; var h=Math.floor(s/3600),m=Math.floor(s%3600/60); return (h?h+'h ':'')+m+'m'}

function paint(){
  if(!S) return;
  var d=$('dot'), t=$('stext');
  if(S.busy){d.className='busy';t.textContent='切换中…'}
  else if(S.lastErr&&!S.running){d.className='err';t.textContent='异常'}
  else if(S.running&&S.ready){d.className='on';t.textContent='运行中'}
  else if(S.running){d.className='busy';t.textContent='启动中…'}
  else {d.className='';t.textContent='已停止'}
  $('s-country').textContent=S.country||'—';
  $('s-exit').textContent=S.exitIp?(S.exitIp+(S.exitCity?' · '+S.exitCity:'')):'—';
  $('s-lat').textContent=S.latencyMs?S.latencyMs+' ms':'—';
  $('s-pid').textContent=S.port+' / '+(S.pid||'—');
  $('s-jwt').textContent=S.jwtDays>=0?S.jwtDays+' 天':'未知';
  $('s-jwt').style.color=(S.jwtDays>=0&&S.jwtDays<7)?'#f09595':'';
  $('s-up').textContent=S.uptimeSec?fmtUp(S.uptimeSec):'—';
  $('err').textContent=S.lastErr||'';
  $('push').style.display=(S.canPush===false)?'none':'inline-block';
  buildHosts(); paintRegions(); paintNode();
}

function buildHosts(){
  if(!S.addrs) return;
  var sel=$('host');
  var list=S.addrs.slice();
  if(S.nodeHost && list.indexOf(S.nodeHost)<0) list.unshift(S.nodeHost);
  if(!HOST) HOST=S.nodeHost||S.primary||list[0];
  if(list.indexOf(HOST)<0){ list.unshift(HOST); }
  var want=list.join(',');
  if(sel.getAttribute('data-list')!==want){
    sel.innerHTML='';
    list.forEach(function(a){
      var o=document.createElement('option');
      o.value=a;
      o.textContent=a+(a===S.nodeHost?'（对外地址）':(a===S.primary?'（主网卡）':''));
      sel.appendChild(o);
    });
    sel.setAttribute('data-list',want);
  }
  sel.value=HOST;
  var ph=$('pubhost');
  if(document.activeElement!==ph) ph.value=S.publicHost||'';
}

async function paintNode(){
  var key=(S?S.country:'')+'|'+HOST;
  if(key===lastNodeKey) return;
  lastNodeKey=key;
  try{
    var r=await get('/api/node?host='+encodeURIComponent(HOST));
    $('uri').textContent=r.uri;
    $('qr').src='/api/qr?host='+encodeURIComponent(HOST)+'&t='+Date.now();
    $('qr').onerror=function(){this.style.display='none'};
    $('qr').onload=function(){this.style.display='block'};
  }catch(e){}
}

function paintRegions(){
  var q=($('q').value||'').toLowerCase();
  var cur=S?S.country:'';
  var html='';
  REG.forEach(function(r){
    if(q&&r.name.toLowerCase().indexOf(q)<0) return;
    var cls=(r.name===cur)?' class="cur"':'';
    html+='<li'+cls+' data-n="'+r.name+'"><span>'+r.name+'</span><b>'+r.count+'</b></li>';
  });
  $('regions').innerHTML=html||'<li style="cursor:default;color:#8b8980">没有匹配的地区</li>';
}

async function loadRegions(force){
  try{ REG=await get('/api/regions'+(force?'?refresh=1':'')); paintRegions(); }catch(e){}
}

async function poll(){
  try{ S=await get('/api/state'); paint(); }
  catch(e){ $('stext').textContent='后端无响应' }
}

var lock=false;
async function switchTo(name){
  if(lock) return;
  lock=true;
  $('stext').textContent='切换到 '+name+'…';
  try{ await post('/api/switch',{country:name}); }catch(e){}
  setTimeout(function(){lock=false},2000);
}

$('regions').addEventListener('click',function(e){
  var li=e.target.closest('li');
  if(li&&li.getAttribute('data-n')) switchTo(li.getAttribute('data-n'));
});
$('q').addEventListener('input',paintRegions);
$('refresh').addEventListener('click',function(){loadRegions(true)});
$('host').addEventListener('change',function(){HOST=this.value;lastNodeKey='';paintNode()});
$('copy').addEventListener('click',function(){
  var t=$('uri').textContent, b=this;
  if(navigator.clipboard) navigator.clipboard.writeText(t);
  b.textContent='已复制';
  setTimeout(function(){b.textContent='复制 URI'},1200);
});
$('push').addEventListener('click',async function(){
  var b=this, old='导入 Shadowrocket';
  b.disabled=true;
  b.textContent='发送中…';
  try{
    var r=await fetch('/api/push?host='+encodeURIComponent(HOST),{method:'POST'});
    var j=await r.json();
    if(j.ok){ b.textContent='已发送，去弹窗确认'; $('err').textContent=''; }
    else { b.textContent='导入失败'; $('err').textContent=(j.error||'open 失败'); }
  }catch(e){ b.textContent='导入失败'; $('err').textContent=String(e); }
  setTimeout(function(){b.textContent=old;b.disabled=false},2500);
});
$('stop').addEventListener('click',async function(){await fetch('/api/stop',{method:'POST'});poll()});
$('start').addEventListener('click',async function(){await fetch('/api/start',{method:'POST'});poll()});
$('logout').addEventListener('click',async function(){
  try{ await fetch('/api/logout',{method:'POST'}); }catch(e){}
  location.href='/login';
});
$('shutdown').addEventListener('click',async function(){
  if(!confirm('停止代理并退出 urnetd？之后需要重新打开 app（或执行 urnetd）才会恢复。')) return;
  try{ await fetch('/api/shutdown',{method:'POST'}); }catch(e){}
  document.body.innerHTML='<div style="padding:60px 20px;text-align:center;color:#a8a69e;font-size:14px">服务已停止，可以关闭这个页面了。<br><span style="font-size:12px">重新打开控制台：双击 URnetwork Console.app</span></div>';
});
$('savehost').addEventListener('click',async function(){
  var b=this, old='保存';
  b.textContent='保存中…';
  try{
    var r=await post('/api/host',{host:$('pubhost').value.trim()});
    if(r.error){ $('err').textContent=r.error; b.textContent='失败'; }
    else { b.textContent='已保存'; HOST=''; lastNodeKey=''; poll(); }
  }catch(e){ b.textContent='失败'; }
  setTimeout(function(){b.textContent=old},1500);
});
$('savetoken').addEventListener('click',async function(){
  var b=this, old='更新凭据', t=$('token').value.trim();
  if(!t){ $('err').textContent='先粘贴 token'; return; }
  b.textContent='校验中…';
  try{
    var r=await post('/api/token',{token:t});
    if(r.error){ $('err').textContent=r.error; b.textContent='失败'; }
    else {
      b.textContent='已更新'; $('token').value='';
      $('err').textContent='凭据已更新至 '+r.expiresAt+'（剩 '+r.daysLeft+' 天），代理正在重启…';
      loadToken(); setTimeout(poll,1500);
    }
  }catch(e){ b.textContent='失败'; }
  setTimeout(function(){b.textContent=old},1800);
});
async function loadToken(){
  try{
    var t=await get('/api/token');
    if(t.userAuth){ $('uauser').value=t.userAuth; $('remember').checked=true; }
    var auto=t.hasCred?'　· 已存账号，将自动续期':'';
    if(t.daysLeft>=0&&t.valid!==false){ $('advsum').textContent='更多设置 · 凭据剩 '+t.daysLeft+' 天'; }
    else { $('advsum').textContent='更多设置 · 凭据需要更新'; $('adv').open=true; }
    if(t.valid===false&&t.error){ $('tokeninfo').textContent='凭据: 读不到 —— '+t.error; $('tokeninfo').style.color='#f09595'; }
    else if(t.valid===false){ $('tokeninfo').textContent='凭据已过期（'+t.expiresAt+'），请更新'+auto; $('tokeninfo').style.color='#f09595'; }
    else { $('tokeninfo').textContent='凭据有效期至 '+t.expiresAt+'（剩 '+t.daysLeft+' 天）· '+t.network+auto; $('tokeninfo').style.color=''; }
  }catch(e){}
}
function bindPush(id, endpoint, label, confirmText){
  $(id).addEventListener('click',async function(){
    var b=this;
    if(confirmText && !confirm(confirmText)) return;
    b.disabled=true; b.textContent='发送中…';
    try{
      var r=await post(endpoint+'?host='+encodeURIComponent(HOST));
      if(r.ok){ b.textContent='已发送，去弹窗确认'; $('err').textContent='订阅地址: '+r.subUrl; }
      else { b.textContent='导入失败'; $('err').textContent=(r.error||'open 失败'); }
    }catch(e){ b.textContent='导入失败'; $('err').textContent=String(e); }
    setTimeout(function(){b.textContent=label;b.disabled=false},2500);
  });
}
bindPush('pushclash','/api/push-clash','导入 Clash Verge','会把「URnetwork」订阅加进 Clash Verge 并切换为当前配置。\n原配置不会被删除，之后可以切回去。继续？');
bindPush('pushflclash','/api/push-flclash','导入 FlClash','会把「URnetwork」订阅加进 FlClash 并切换为当前配置。\n原配置不会被删除，之后可以切回去。继续？');
$('docode').addEventListener('click',async function(){
  var b=this, old='用授权码登录', c=$('authcode').value.trim();
  if(!c){ $('err').textContent='先粘贴授权码（1 分钟内有效）'; return; }
  b.textContent='登录中…'; $('err').textContent='';
  try{
    var r=await post('/api/auth-code',{code:c});
    if(r.error){ $('err').textContent='授权码登录失败: '+r.error; b.textContent='失败'; }
    else {
      b.textContent='已登录'; $('authcode').value='';
      $('err').textContent='授权码登录成功，凭据至 '+r.expiresAt+'（剩 '+r.daysLeft+' 天），代理正在重启…';
      loadToken(); setTimeout(poll,1500);
    }
  }catch(e){ b.textContent='失败'; }
  setTimeout(function(){b.textContent=old},1800);
});
$('dologin').addEventListener('click',async function(){
  var b=this, old='账号登录';
  var ua=$('uauser').value.trim(), pw=$('uapass').value;
  if(!ua||!pw){ $('err').textContent='账号和密码都要填'; return; }
  b.textContent='登录中…'; $('err').textContent='';
  try{
    var r=await post('/api/login-cred',{userAuth:ua,password:pw,remember:$('remember').checked});
    if(r.error){ $('err').textContent='登录失败: '+r.error; b.textContent='失败'; }
    else {
      b.textContent='已登录'; $('uapass').value='';
      $('err').textContent='登录成功（'+r.network+'），凭据有效期至 '+r.expiresAt+'，代理正在重启…';
      loadToken(); setTimeout(poll,1500);
    }
  }catch(e){ b.textContent='失败'; }
  setTimeout(function(){b.textContent=old},1800);
});

async function logs(){
  try{
    var L=await get('/api/logs?tail=120');
    var el=$('logs');
    var bottom=(el.scrollTop+el.clientHeight>=el.scrollHeight-24);
    el.textContent=(L||[]).join('\n');
    if(bottom) el.scrollTop=el.scrollHeight;
  }catch(e){}
}

function syncGuide(){
  var hid=(localStorage.getItem('urnetdGuide')==='0');
  $('guide').style.display=hid?'none':'flex';
  $('guide-tog').style.display=hid?'inline-block':'none';
}
$('guide-x').addEventListener('click',function(){localStorage.setItem('urnetdGuide','0');syncGuide()});
$('guide-tog').addEventListener('click',function(){localStorage.setItem('urnetdGuide','1');syncGuide()});
$('refchip').addEventListener('click',function(){
  var b=this, old='推荐码 5CAI7C';
  if(navigator.clipboard) navigator.clipboard.writeText('5CAI7C');
  b.textContent='已复制 5CAI7C';
  setTimeout(function(){b.textContent=old},1400);
});
syncGuide();

poll(); loadRegions(false); logs(); loadToken();
setInterval(poll,2000);
setInterval(logs,2500);
</script>
</body>
</html>`
