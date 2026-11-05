package main

// 面板的口令保护。
//
// 设计取舍：
//   - 不引外部依赖，用标准库做加盐 sha256 迭代（10 万轮），挡住随手爆破足够
//   - 会话无状态：cookie = "过期时间.HMAC"，密钥由口令哈希派生。
//     改密码即让所有旧会话失效，服务重启也不会把已登录的人踢下去
//   - 没设密码时只在回环监听的情况下放行；监听 0.0.0.0 又没密码则一律拒绝

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	sessionCookie = "urnetd_session"
	sessionTTL    = 30 * 24 * time.Hour
	hashRounds    = 100000
)

func newSalt() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 极端情况下退化成时间戳，只是为了别 panic
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func hashPassword(pw, saltHex string) string {
	salt, _ := hex.DecodeString(saltHex)
	buf := make([]byte, 0, len(salt)+len(pw))
	buf = append(buf, salt...)
	buf = append(buf, pw...)
	sum := sha256.Sum256(buf)
	cur := sum[:]
	for i := 0; i < hashRounds; i++ {
		h := sha256.New()
		h.Write(cur)
		h.Write(salt)
		cur = h.Sum(nil)
	}
	return hex.EncodeToString(cur)
}

func checkPassword(pw string, c savedConf) bool {
	if c.PassHash == "" || c.Salt == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashPassword(pw, c.Salt)), []byte(c.PassHash)) == 1
}

// setPassword 生成新盐并写盘，同时让旧会话全部失效
func setPassword(pw string) error {
	c := loadConf()
	c.Salt = newSalt()
	c.PassHash = hashPassword(pw, c.Salt)
	return saveConf(c)
}

// ---------- 无状态会话 ----------

func sessionKey(c savedConf) []byte {
	k := sha256.Sum256([]byte("urnetd-session-v1|" + c.Salt + "|" + c.PassHash))
	return k[:]
}

func signSession(exp int64, key []byte) string {
	payload := fmt.Sprintf("%d", exp)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(m.Sum(nil))
}

func verifySession(v string, key []byte) bool {
	i := strings.LastIndex(v, ".")
	if i <= 0 {
		return false
	}
	payload, sigHex := v[:i], v[i+1:]
	var exp int64
	if _, err := fmt.Sscanf(payload, "%d", &exp); err != nil {
		return false
	}
	if time.Now().Unix() > exp {
		return false
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return hmac.Equal(sig, m.Sum(nil))
}

// ---------- 中间件 ----------

// isLoopbackListen 判断面板是不是只绑在本机
func isLoopbackListen() bool {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) setSession(w http.ResponseWriter, c savedConf) {
	exp := time.Now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    signSession(exp, sessionKey(c)),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// authWrap 保护面板与 API
func (s *Server) authWrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := loadConf()
		if c.PassHash == "" {
			if !isLoopbackListen() {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, "未设置面板密码，且监听在非回环地址，已拒绝访问。\n请先在终端执行: urnetd set-password <你的密码>")
				return
			}
			// 本机访问且还没设密码：引导去 /setup 自己设一个
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "还没设置面板密码", "setup": "/setup"})
				return
			}
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		if ck, err := r.Cookie(sessionCookie); err == nil && verifySession(ck.Value, sessionKey(c)) {
			next(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "未登录或会话已过期"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

// ---------- 登录 / 登出 ----------

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	c := loadConf()
	if ck, err := r.Cookie(sessionCookie); err == nil && verifySession(ck.Value, sessionKey(c)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(loginPage))
}

func (s *Server) failLogin(w http.ResponseWriter, r *http.Request, msg string, wantsJSON bool) {
	time.Sleep(800 * time.Millisecond) // 拖一下，别让人高频试
	if wantsJSON {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
		return
	}
	http.Redirect(w, r, "/login?e=1", http.StatusFound)
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	c := loadConf()
	pw := ""
	jsonReq := strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
	if jsonReq {
		var body struct {
			Password string `json:"password"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		pw = body.Password
	} else {
		r.ParseForm()
		pw = r.FormValue("password")
	}
	if c.PassHash == "" {
		s.failLogin(w, r, "面板还没设置密码", jsonReq)
		return
	}
	if !checkPassword(pw, c) {
		s.failLogin(w, r, "密码不对", jsonReq)
		return
	}
	s.setSession(w, c)
	if jsonReq {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ---------- 首次启动生成随机密码 ----------

// ensurePassword 只在「部署方通过环境变量指定了密码」时自动写入。
// 其余情况不生成随机密码——因为 .app 由 LaunchServices 启动，stdout 看不到，
// 随机密码等于把人锁在门外。改成首次访问时由用户自己在 /setup 里设。
func ensurePassword() bool {
	c := loadConf()
	if c.PassHash != "" {
		return false
	}
	pw := os.Getenv("URNETD_PASSWORD")
	if pw == "" {
		return false
	}
	c.Salt = newSalt()
	c.PassHash = hashPassword(pw, c.Salt)
	if err := saveConf(c); err != nil {
		fmt.Fprintln(os.Stderr, "写入密码失败:", err)
		return false
	}
	return true
}

func randomPassword(n int) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	out := make([]byte, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			out[i] = 'x'
			continue
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out)
}

// subToken 给订阅地址用的固定令牌：由面板密码派生，改密码即失效。
// 这样 Clash 来拉 YAML 时不用带登录 cookie。
func subToken() string {
	c := loadConf()
	m := hmac.New(sha256.New, sessionKey(c))
	m.Write([]byte("urnetd-clash-sub-v1"))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

func checkSubToken(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(subToken())) == 1
}

// ---------- 首次使用：由用户自己设置面板密码 ----------

func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	if loadConf().PassHash != "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(setupPage))
}

func (s *Server) handleSetupPost(w http.ResponseWriter, r *http.Request) {
	if loadConf().PassHash != "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	jsonReq := strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
	pw, pw2 := "", ""
	if jsonReq {
		var body struct {
			Password string `json:"password"`
			Confirm  string `json:"confirm"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		pw, pw2 = body.Password, body.Confirm
	} else {
		r.ParseForm()
		pw, pw2 = r.FormValue("password"), r.FormValue("confirm")
	}
	fail := func(msg string) {
		if jsonReq {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": msg})
			return
		}
		http.Redirect(w, r, "/setup?e="+url.QueryEscape(msg), http.StatusFound)
	}
	if len([]rune(pw)) < 6 {
		fail("密码至少 6 位")
		return
	}
	if pw != pw2 {
		fail("两次输入不一致")
		return
	}
	if err := setPassword(pw); err != nil {
		fail("写入失败: " + err.Error())
		return
	}
	s.setSession(w, loadConf())
	s.note("[urnetd] 面板密码已设置")
	if jsonReq {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

const setupPage = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>首次使用 · URnetwork 控制台</title>
<style>
:root{color-scheme:dark}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:#1b1b1a;color:#e8e6e0;font:14px/1.6 -apple-system,BlinkMacSystemFont,"PingFang SC",Helvetica,sans-serif}
.card{width:340px;background:#232322;border:1px solid #33322f;border-radius:14px;padding:22px}
h1{font-size:15px;font-weight:500;margin:0 0 4px}
p{color:#8b8980;font-size:12px;margin:0 0 14px;line-height:1.6}
label{display:block;font-size:11px;color:#8b8980;margin:10px 0 4px}
input{width:100%;font:inherit;color:inherit;background:#2c2c2a;border:1px solid #3d3c39;border-radius:8px;padding:9px 11px;outline:none}
input:focus{border-color:#5f5e5a}
button{width:100%;margin-top:16px;font:inherit;cursor:pointer;background:#185fa5;border:1px solid #185fa5;color:#fff;border-radius:8px;padding:9px}
button:hover{background:#1d6fbf}
.err{color:#f09595;font-size:12px;margin-top:10px;min-height:16px}
.tip{margin-top:14px;padding-top:12px;border-top:1px solid #33322f;color:#5f5e5a;font-size:11px;line-height:1.6}
</style>
</head>
<body>
<form class="card" method="post" action="/setup">
  <h1>先设一个管理密码</h1>
  <p>这是打开本机控制台的密码，跟你的 URnetwork 账号无关。<br>设好之后进面板才能操作。</p>
  <label>密码（至少 6 位）</label>
  <input type="password" name="password" autofocus autocomplete="new-password">
  <label>再输一遍</label>
  <input type="password" name="confirm" autocomplete="new-password">
  <button type="submit">设置并进入</button>
  <div class="err" id="err"></div>
  <div class="tip">忘了也没关系：终端执行 urnetd set-password 新密码 即可重置。</div>
</form>
<script>
var m=new URLSearchParams(location.search).get('e');
if(m) document.getElementById('err').textContent=m;
</script>
</body>
</html>`

const loginPage = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>登录 · URnetwork 控制台</title>
<style>
:root{color-scheme:dark}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:#1b1b1a;color:#e8e6e0;font:14px/1.6 -apple-system,BlinkMacSystemFont,"PingFang SC",Helvetica,sans-serif}
.card{width:320px;background:#232322;border:1px solid #33322f;border-radius:14px;padding:22px}
h1{font-size:15px;font-weight:500;margin:0 0 4px}
p{color:#8b8980;font-size:12px;margin:0 0 16px}
input{width:100%;font:inherit;color:inherit;background:#2c2c2a;border:1px solid #3d3c39;border-radius:8px;padding:9px 11px;outline:none}
input:focus{border-color:#5f5e5a}
button{width:100%;margin-top:12px;font:inherit;cursor:pointer;background:#185fa5;border:1px solid #185fa5;color:#fff;border-radius:8px;padding:9px}
button:hover{background:#1d6fbf}
.err{color:#f09595;font-size:12px;margin-top:10px;min-height:16px}
</style>
</head>
<body>
<form class="card" method="post" action="/login">
  <h1>URnetwork 控制台</h1>
  <p>输入面板密码</p>
  <input type="password" name="password" autofocus autocomplete="current-password" placeholder="密码">
  <button type="submit">登录</button>
  <div class="err" id="err"></div>
</form>
<script>
if(location.search.indexOf('e=1')>-1) document.getElementById('err').textContent='密码不对，再试一次';
</script>
</body>
</html>`
