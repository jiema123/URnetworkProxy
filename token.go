package main

// 凭据（JWT）的读取、校验、写入与命令行工具。
//
// 一个必然的限制：token 只能由官方登录接口签发，本程序无法凭空造出来。
// 所以这里的定位是「搬运与更新」——
//   - 在有 App 的机器上导出（urnetd token show）
//   - 在服务器上导入（urnetd token import，或面板上粘贴）
//   - 校验有效期，提前发现过期

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type jwtInfo struct {
	ExpiresAt   time.Time
	IssuedAt    time.Time
	DaysLeft    int
	Expired     bool
	NetworkName string
	NetworkID   string
	ClientID    string
	UserID      string
	Subject     string
	Issuer      string
}

// decodeJwt 解出 payload（不验签：验签需要服务端公钥，这里只做本地自检）
func decodeJwt(tok string) (*jwtInfo, error) {
	tok = strings.TrimSpace(tok)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errors.New("不是合法的 JWT（应为三段用 . 分隔）")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("payload 不是合法 base64url: %w", err)
	}
	var c struct {
		Exp         int64  `json:"exp"`
		Iat         int64  `json:"iat"`
		NetworkName string `json:"network_name"`
		NetworkID   string `json:"network_id"`
		ClientID    string `json:"client_id"`
		UserID      string `json:"user_id"`
		Sub         string `json:"sub"`
		Iss         string `json:"iss"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("payload 不是 JSON: %w", err)
	}
	if c.Exp == 0 {
		return nil, errors.New("JWT 里没有 exp，无法判断有效期")
	}
	info := &jwtInfo{
		ExpiresAt:   time.Unix(c.Exp, 0),
		NetworkName: c.NetworkName,
		NetworkID:   c.NetworkID,
		ClientID:    c.ClientID,
		UserID:      c.UserID,
		Subject:     c.Sub,
		Issuer:      c.Iss,
	}
	if c.Iat > 0 {
		info.IssuedAt = time.Unix(c.Iat, 0)
	}
	info.DaysLeft = int(time.Until(info.ExpiresAt).Hours() / 24)
	info.Expired = time.Now().After(info.ExpiresAt)
	return info, nil
}

// readByJwt 从 .auth_state 里取 by_jwt
func readByJwt(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if tok, ok := extractJwt(string(b)); ok {
		return tok, nil
	}
	return "", errors.New("文件里没有找到 JWT")
}

// extractJwt 兼容两种输入：完整 .auth_state JSON，或直接一段 JWT 文本
func extractJwt(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "{") {
		var st struct {
			ByJwt       string `json:"by_jwt"`
			ByClientJwt string `json:"by_client_jwt"`
		}
		if json.Unmarshal([]byte(raw), &st) == nil {
			if st.ByJwt != "" {
				return st.ByJwt, true
			}
			if st.ByClientJwt != "" {
				return st.ByClientJwt, true
			}
		}
		return "", false
	}
	// 纯 JWT：去掉可能的 "Bearer " 前缀和引号
	raw = strings.TrimPrefix(raw, "Bearer ")
	raw = strings.Trim(raw, "\"' ")
	if strings.Count(raw, ".") == 2 {
		return raw, true
	}
	return "", false
}

// writeByJwt 把 token 写进 .auth_state。
// 原文件存在时保留其余字段（version/generation/instance_id 等），只替换两个 jwt 字段，
// 这样 macOS 上不会把 App 写的结构弄坏。
func writeByJwt(path, jwt string) error {
	if _, err := decodeJwt(jwt); err != nil {
		return err
	}
	obj := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &obj) // 解析失败就当空对象，下面重建
	}
	if obj == nil {
		obj = map[string]any{}
	}
	obj["by_jwt"] = jwt
	obj["by_client_jwt"] = jwt
	if _, ok := obj["version"]; !ok {
		obj["version"] = 1
	}
	if _, ok := obj["generation"]; !ok {
		obj["generation"] = 1
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// 留一份旧文件，新 token 万一不好用可以手动回滚
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		os.WriteFile(path+".bak", b, 0o600)
	}
	// 先写临时文件再改名，避免读方拿到半个文件
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- 命令行 ----------

func tokenStatus() (*jwtInfo, string, error) {
	tok, err := readByJwt(authState)
	if err != nil {
		return nil, "", err
	}
	info, err := decodeJwt(tok)
	return info, tok, err
}

func doToken(args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "", "status":
		info, tok, err := tokenStatus()
		if err != nil {
			fmt.Println("凭据文件:", authState)
			fmt.Println("状态: 读不到 ——", err)
			fmt.Println()
			fmt.Println("导入方法:")
			fmt.Println("  在已有 App 的机器上: urnetd token show   （复制输出的那串）")
			fmt.Println("  在本机:             urnetd token import <文件或 ->")
			fmt.Println("  或者在面板「凭据」输入框里直接粘贴")
			os.Exit(1)
		}
		fmt.Println("凭据文件:", authState)
		fmt.Println("网络:", info.NetworkName, "("+info.NetworkID+")")
		fmt.Println("签发:", info.IssuedAt.Format("2006-01-02 15:04"))
		fmt.Println("过期:", info.ExpiresAt.Format("2006-01-02 15:04"))
		if info.Expired {
			fmt.Println("状态: 已过期，请更新（面板里粘贴新的，或 urnetd token import）")
		} else {
			fmt.Printf("状态: 有效，还剩 %d 天\n", info.DaysLeft)
		}
		fmt.Println("指纹:", fingerprint(tok))

	case "show":
		_, tok, err := tokenStatus()
		if err != nil {
			fmt.Fprintln(os.Stderr, "读不到凭据:", err)
			os.Exit(1)
		}
		// 只输出 token 本身，方便管道复制
		fmt.Println(tok)

	case "import", "set":
		src := ""
		if len(args) > 1 {
			src = args[1]
		}
		if src == "" {
			fmt.Fprintln(os.Stderr, "用法: urnetd token import <文件路径|->   （- 表示从标准输入读）")
			os.Exit(1)
		}
		var raw []byte
		var err error
		if src == "-" {
			raw, err = readAllStdin()
		} else {
			raw, err = os.ReadFile(src)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "读取失败:", err)
			os.Exit(1)
		}
		jwt, ok := extractJwt(string(raw))
		if !ok {
			fmt.Fprintln(os.Stderr, "输入里没有识别到 JWT")
			os.Exit(1)
		}
		info, err := decodeJwt(jwt)
		if err != nil {
			fmt.Fprintln(os.Stderr, "JWT 校验失败:", err)
			os.Exit(1)
		}
		if err := writeByJwt(authState, jwt); err != nil {
			fmt.Fprintln(os.Stderr, "写入失败:", err)
			os.Exit(1)
		}
		fmt.Println("已写入:", authState)
		fmt.Println("过期:", info.ExpiresAt.Format("2006-01-02 15:04"), fmt.Sprintf("（剩 %d 天）", info.DaysLeft))
		if info.Expired {
			fmt.Println("注意: 这个 token 已经过期了")
		}
		fmt.Println("代理需要重启才会用上新凭据；面板上点「重启」，或等待自动检测（30 秒内）")

	case "code", "auth-code":
		code := ""
		if len(args) > 1 {
			code = args[1]
		}
		if code == "-" {
			raw, _ := readAllStdin()
			code = strings.TrimSpace(string(raw))
		}
		if code == "" {
			fmt.Fprint(os.Stderr, "授权码（App → Account Tools → Copy Auth Code，1 分钟内有效）: ")
			code = readLine()
		}
		jwt, err := loginWithAuthCode(code)
		if err != nil {
			fmt.Fprintln(os.Stderr, "授权码登录失败:", err)
			os.Exit(1)
		}
		info, err := decodeJwt(jwt)
		if err != nil {
			fmt.Fprintln(os.Stderr, "拿到的 token 有问题:", err)
			os.Exit(1)
		}
		if err := writeByJwt(authState, jwt); err != nil {
			fmt.Fprintln(os.Stderr, "写入失败:", err)
			os.Exit(1)
		}
		fmt.Println("授权码登录成功，凭据已写入:", authState)
		fmt.Println("网络:", info.NetworkName)
		fmt.Println("过期:", info.ExpiresAt.Format("2006-01-02 15:04"), fmt.Sprintf("（剩 %d 天）", info.DaysLeft))

	case "login":
		// 用法: token login <账号> <密码|->   （- 表示从标准输入读密码）
		rest := args[1:]
		ua := ""
		if len(rest) > 0 && rest[0] != "-" {
			ua = rest[0]
		}
		if ua == "" {
			fmt.Fprint(os.Stderr, "账号（user-auth，通常是邮箱或用户 ID）: ")
			ua = readLine()
		}
		pw := ""
		if len(rest) > 1 {
			if rest[1] == "-" {
				raw, _ := readAllStdin()
				pw = strings.TrimSpace(string(raw))
			} else {
				pw = rest[1]
			}
		}
		if pw == "" {
			fmt.Fprint(os.Stderr, "密码（明文回显；建议用 echo 密码 | urnetd token login 账号 -）: ")
			pw = readLine()
		}
		if ua == "" || pw == "" {
			fmt.Fprintln(os.Stderr, "账号和密码都要有")
			os.Exit(1)
		}
		jwt, network, err := loginWithPassword(ua, pw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "登录失败:", err)
			os.Exit(1)
		}
		info, err := decodeJwt(jwt)
		if err != nil {
			fmt.Fprintln(os.Stderr, "拿到的 token 有问题:", err)
			os.Exit(1)
		}
		if err := writeByJwt(authState, jwt); err != nil {
			fmt.Fprintln(os.Stderr, "写入失败:", err)
			os.Exit(1)
		}
		fmt.Println("登录成功，凭据已写入:", authState)
		fmt.Println("网络:", network)
		fmt.Println("过期:", info.ExpiresAt.Format("2006-01-02 15:04"), fmt.Sprintf("（剩 %d 天）", info.DaysLeft))

	default:
		fmt.Fprintln(os.Stderr, "未知子命令:", sub)
		fmt.Fprintln(os.Stderr, "可用:")
		fmt.Fprintln(os.Stderr, "  urnetd token                      查看有效期")
		fmt.Fprintln(os.Stderr, "  urnetd token show                 打印完整 JWT")
		fmt.Fprintln(os.Stderr, "  urnetd token import <文件|->      从 .auth_state 或 JWT 文本导入")
		fmt.Fprintln(os.Stderr, "  urnetd token code <授权码|->     用 App 的一次性授权码换 token（推荐）")
		fmt.Fprintln(os.Stderr, "  urnetd token login <账号> <密码|-> 用账号密码向官方接口换 token")
		os.Exit(1)
	}
}

func fingerprint(tok string) string {
	if len(tok) < 24 {
		return "(too short)"
	}
	return tok[:12] + "…" + tok[len(tok)-8:]
}

// loginWithAuthCode 用 App 里「Copy Auth Code」生成的一次性授权码换 token。
// 授权码 1 分钟有效、只能用一次（App 里 duration_minutes=1, uses=1），
// 但换到的 JWT 是完整有效期的，所以这条路比拷 .auth_state 更省事。
//
//	POST /auth/code-login {"auth_code":"..."}  →  {"by_jwt":"..."}
func loginWithAuthCode(code string) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "", errors.New("授权码不能为空")
	}
	payload, _ := json.Marshal(map[string]string{"auth_code": code})
	req, err := http.NewRequest("POST", apiBaseURL()+"/auth/code-login", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		ByJwt string `json:"by_jwt"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("接口返回无法解析（HTTP %d）", resp.StatusCode)
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", errors.New(out.Error.Message)
	}
	if out.ByJwt == "" {
		return "", fmt.Errorf("接口没返回 token（HTTP %d）", resp.StatusCode)
	}
	return out.ByJwt, nil
}

// ---------- 账号密码登录（官方 /auth/login-with-password） ----------

func apiBaseURL() string {
	if v := os.Getenv("URNET_API_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://api.bringyour.com"
}

// loginWithPassword 用官方接口换一个 JWT。
// 注意：这个 token 是「网络级」的，拿到后仍然写进 auth_state，
// 代理侧只认文件，不关心它是 App 给的还是这里换的。
func loginWithPassword(userAuth, password string) (jwt string, network string, err error) {
	payload, _ := json.Marshal(map[string]string{
		"user_auth": userAuth,
		"password":  password,
	})
	url := apiBaseURL() + "/auth/login-with-password"
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		VerificationRequired *struct {
			UserAuth string `json:"user_auth"`
		} `json:"verification_required"`
		Network *struct {
			ByJwt string `json:"by_jwt"`
			Name  string `json:"name"`
		} `json:"network"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", fmt.Errorf("接口返回无法解析（HTTP %d）", resp.StatusCode)
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", "", errors.New(out.Error.Message)
	}
	if out.VerificationRequired != nil {
		return "", "", errors.New("该账号需要验证码验证，请改用 token 方式导入")
	}
	if out.Network == nil || out.Network.ByJwt == "" {
		return "", "", fmt.Errorf("接口没返回 token（HTTP %d）", resp.StatusCode)
	}
	return out.Network.ByJwt, out.Network.Name, nil
}

func readLine() string {
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

func readAllStdin() ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if n == 0 {
				return buf, nil
			}
			break
		}
	}
	return buf, nil
}
