# URnetwork 面板（urnetd）

把 [URnetwork](https://ur.io/) 的去中心化出口桥接成本地 SOCKS5，并给你一个网页面板来切换地区、生成节点、导入 Shadowrocket。

![工作流](docs/img/flow.png)

> 一个本地入口，背后是几万个随机出口；分流规则仍然在你自己的客户端里配。

## 下载

安装包在 [Releases](https://github.com/jiema123/URnetworkProxy/releases/latest) 里（不放进 git 历史，避免仓库被几十 MB 的二进制撑大）：

| 平台 | 文件 |
| --- | --- |
| macOS（Intel + Apple Silicon 通用） | `urnetd-x.y.z.dmg` |
| Linux amd64 | `urnetd-linux-amd64.tar.gz` |
| Linux arm64 | `urnetd-linux-arm64.tar.gz` |

---

## 为什么需要它

URnetwork 是 P2P 网络：出口是你家宽带上的**真人设备**，地址随机、随时变化，
所以它给不出「节点列表」——你没法把 `IP:端口 + 密码` 填进 Shadowrocket / Clash / sing-box。

可行做法只有一个：在本机跑一个桥接进程，让它**对外只暴露一个 SOCKS5 端口**，
出口由平台按你选的地区实时分配。这个面板就是管这个桥接进程的。

| 常见误解 | 实际情况 |
| --- | --- |
| 面板里是一堆可选节点 | 只是一个入口 + 一个地区条件 |
| 切换地区 = 换个节点 | 是重启桥接进程，让平台重新挑出口 |
| 换 IP 要改配置 | 每次建连都可能是新出口，端口和账号不变 |

## 架构

![架构](docs/img/arch.png)

- **出口层**：URnetwork 平台按地区挑出真实设备做出口，随机且会变
- **桥接层**：`socksproxy` 子进程（官方 `urnetwork/proxy` 的 socks 实现），负责把 SOCKS5 流量送进 P2P 网络
- **客户端层**：Shadowrocket / Clash 把本机端口当成普通 SOCKS5 节点用，分流规则在这里配

面板（`urnetd`）本身不转发流量，它只做四件事：管进程、切地区、探真实出口、发节点。

## 功能

- **网页面板**：点地区即切换，失败自动回滚到上一个地区
- **真实出口探测**：内置极简 SOCKS5 客户端（RFC1929），走代理查 `ipinfo.io`，显示出口 IP / 城市 / 耗时
- **节点分发**：生成节点 URI 和二维码；macOS 上可以把节点一键推进 Shadowrocket，也可以推进 Clash Verge（走订阅地址）
- **授权码登录**：App 里点一下 Copy Auth Code，粘到面板就换到新凭据，不用找密码、不用拷文件
- **面板密码**：加盐 SHA-256 ×10 万轮存储，会话用 HMAC 签名，改密码即让旧会话失效
- **开机自启**：macOS 用 LaunchAgent，Linux 用 systemd user unit，一个命令装/卸
- **真·单文件**：Go 标准库 + 一个纯 Go 二维码库，前端内嵌，无打包步骤；macOS 出通用二进制（Intel + Apple Silicon）

## 快速开始

### macOS（dmg）

1. 从 [Releases](https://github.com/jiema123/URnetworkProxy/releases/latest) 下载 `urnetd-x.y.z.dmg`，把 **URnetwork Console.app** 拖进「应用程序」
2. 首次打开要绕过 Gatekeeper（无 Apple 开发者签名）：右键 app →「打开」→ 再点「打开」
3. 它会起服务并打开浏览器（默认 `http://127.0.0.1:8877`），**第一次会引导你设一个面板密码**
4. 到面板里粘授权码、选地区、扫码导入节点

app 是纯后台服务（`LSUIElement`），没有 Dock 图标也没有菜单栏图标。想再打开面板：

- 双击 dmg 里的 **打开控制台.command**
- 或者浏览器收藏 `http://127.0.0.1:8877`
- 或者再双击一次 app —— 检测到已在运行会直接打开面板

### Linux（tar.gz）

从 [Releases](https://github.com/jiema123/URnetworkProxy/releases/latest) 下载对应架构的包：

```bash
sudo mkdir -p /opt/urnetd
sudo tar -xzf urnetd-linux-arm64.tar.gz -C /opt/urnetd
sudo chmod +x /opt/urnetd/urnetd /opt/urnetd/socksproxy

/opt/urnetd/urnetd              # 前台跑，默认 http://127.0.0.1:8877
/opt/urnetd/urnetd install      # 装 systemd user unit 并立即启动
```

服务器场景（面板要监听对外地址时）必须先设密码，因为网页端不能自助设置：

```bash
/opt/urnetd/urnetd set-password '你的密码'
URNETD_LISTEN=0.0.0.0:8877 /opt/urnetd/urnetd
sudo loginctl enable-linger $USER   # 没登录会话也让服务常驻
```

### 从源码构建

```bash
go build -o urnetd .          # 需要 Go 1.27+
./urnetd
```

打包（产物在 `dist/`、`linux/`）：

```bash
./package-macos.sh   # 出 .app + dmg，两个二进制都是 arm64+x86_64
./package-linux.sh   # 出 amd64 / arm64 tar.gz
```

## 第一次用：拿授权码

面板要的是 URnetwork 的凭据（JWT），它不是密码，只能由官方登录签发。
最省事的做法是拿一次性授权码换：

![授权码位置](help/authcode.png)

1. 浏览器打开 <https://ur.io/> 登录（Google 账号即可）；还没账号先注册，注册时填推荐码 `5CAI7C`（双方有奖励）
2. 点右上角头像，展开账户面板
3. 拉到 **ACCOUNT TOOLS**，点 **Copy Auth Code**（码 1 分钟内有效、只能用一次）
4. 回面板「换凭据」粘上，60 秒内点「用授权码登录」

也可以换别的路：

```bash
urnetd token code            # 粘贴授权码
urnetd token login 账号 密码  # 账号有密码时（走官方 /auth/login-with-password）
urnetd token import 文件      # 从别的机器拷 .auth_state 过来
urnetd token                 # 看还剩几天
```

## 面板长什么样

![控制台](docs/img/panel.png)

左侧地区列表（带在线出口数，可搜索），右侧节点卡片：

- 顶部状态条：当前地区、出口 IP、探测耗时、端口/PID、凭据剩余天数、已运行时长
- 节点卡片：地址下拉、复制 URI、导入 Shadowrocket、导入 Clash Verge、二维码
- 「换凭据」：授权码输入框 + 「怎么获取授权码？」折叠说明
- 「更多设置」：对外地址、JWT 直填、账号密码登录、停止服务
- 底部日志：桥接进程的实时输出

点「导入 Shadowrocket」之后，节点会直接出现在小火箭里（SOCKS5 / UDP）：

![Shadowrocket 里导入后的节点](docs/img/shadowrocket.png)

分流规则在小火箭的「配置」里配 —— 哪些域名 / IP 走这个节点、哪些直连，都由它决定，
本工具只负责把出口稳定地挂在一个本地 SOCKS5 端口上。

### 用 Clash / mihomo（含 Clash Verge）

- **Clash Verge**：面板点「导入 Clash Verge」，会通过 `clash://install-config` 深链把一个订阅加进去并切换为当前配置。
  原来那份配置不会被删，之后可以在 Clash Verge 里切回去。弹窗里点确认即可。
- **其他客户端**（mihomo / sing-box / openclash 等）：把订阅地址填进去就行。地址在点「导入 Clash Verge」失败时会显示，
  也可以自己拼：`http://127.0.0.1:8877/sub/<token>.yaml?host=<对外地址>`。
  token 由面板密码派生（改密码即失效），所以订阅地址不需要也不带登录态。
- 订阅里只有一个 `socks5` 节点 + 一个 `select` 组，**不含规则**：分流仍然在客户端里配。

> 生成的配置故意不写 `mixed-port` 之类端口字段，避免和客户端自己的端口设置打架。
> 已用 `verge-mihomo -t` 校验过可以正常加载。

## 命令行

```
urnetd                          前台运行（默认 127.0.0.1:8877）
urnetd app-start                同上，供 launchd / systemd 调用
urnetd install | uninstall      装/卸开机自启
urnetd set-password [密码]      设/改面板密码，不带参数则随机生成
urnetd token [status]           看凭据有效期、网络名、指纹
urnetd token show               打印完整 JWT
urnetd token import <文件|->     导入 JWT 或 .auth_state
urnetd token code <授权码|->     用一次性授权码换凭据
urnetd token login <账号> <密码|->  用账号密码换凭据
```

## HTTP API

面板全部功能都是这套接口，方便自己接脚本。除 `/login`、`/setup`、`/api/login*` 外都需要登录会话。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/state` | 运行状态、出口、凭据天数、可用地址 |
| GET | `/api/regions` | 地区列表（`?refresh=1` 强制刷新） |
| POST | `/api/switch` | `{"country":"Germany"}` 切换地区 |
| POST | `/api/start` `/api/stop` | 启停桥接进程 |
| GET | `/api/node?host=` | 节点 URI |
| GET | `/api/qr?host=` | 节点二维码 PNG |
| POST | `/api/push?host=` | 把节点推给本机客户端（macOS，Shadowrocket） |
| POST | `/api/push-clash?host=` | 把订阅推给 Clash Verge（macOS，走 `clash://` 深链） |
| GET | `/sub/<token>.yaml?host=` | Clash 订阅（令牌鉴权，不需要登录态） |
| GET | `/api/logs?tail=` | 最近日志 |
| GET/POST | `/api/host` | 读写对外地址（`auto` = 自动查公网 IP） |
| GET/POST | `/api/token` | 看/写 JWT |
| POST | `/api/auth-code` | `{"code":"..."}` 授权码登录 |
| POST | `/api/login-cred` | `{"userAuth","password","remember"}` 账号登录 |
| POST | `/api/shutdown` | 停服务并退出 |

## 配置与环境变量

配置文件：macOS `~/Library/Application Support/urnetd/urnetd.json`，
Linux `~/.config/urnetd/urnetd.json`（0600，含密码哈希）。

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `URNETD_LISTEN` | `127.0.0.1:8877` | 面板监听地址 |
| `URNET_PORT` | `9999` | 代理端口 |
| `URNET_BIN` | 包内 `Resources/socksproxy` | 桥接程序路径 |
| `URNET_AUTH_STATE` | 见平台默认 | 凭据文件路径 |
| `URNET_SOCKS_USER` / `URNET_SOCKS_PASS` | `urnet` / 首次运行随机生成 | SOCKS5 账号密码；改它等于换节点凭据，要重新扫码 |
| `URNET_PUBLIC_HOST` | 空 | 节点对外地址，`auto` 自动查公网 IP |
| `URNETD_PASSWORD` | 空 | 启动时直接写入面板密码（适合自动化部署） |
| `URNET_API_URL` | `https://api.bringyour.com` | 官方 API |
| `URNET_HELP_IMG` | 空 | 替换面板里那张授权码示意图 |

## 安全说明

- 面板默认**只监听 127.0.0.1**，局域网访问不到；改监听地址前请先设密码
- 面板密码不明文存储，只存盐和哈希；会话是无状态 HMAC，改密码立即失效
- **SOCKS5 的账号密码是明文协议**。代理端口默认监听 `0.0.0.0`（方便手机连），
  密码首次运行随机生成并存在本地配置里；但仍不要把这个端口直接暴露到公网，
  远程使用建议只开面板，或用 SSH 隧道 / WireGuard 转发 9999
- 凭据文件等价于账号权限，权限位 0600，别提交到仓库、别贴进聊天工具

## 已知限制

- 凭据约 30 天过期。有账号密码可以自动续期，否则到期重新拿一次授权码
- 同一账号并发多个桥接实例时，平台可能限流（429），建议只跑一个
- 依赖官方未公开文档的接口，平台侧改动可能让某些功能失效
- 目前只支持 macOS / Linux；Windows 没做（Go 侧代码基本通用，主要差自启和「一键导入」）

## 致谢

- [urnetwork/proxy](https://github.com/urnetwork/proxy) — SOCKS5 桥接实现
- [skip2/go-qrcode](https://github.com/skip2/go-qrcode) — 纯 Go 二维码

## 许可

MIT
