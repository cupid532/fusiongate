# FusionGate

面向个人和小型可信团队的**自托管 AI 账号与 API 聚合网关**。它将多个上游渠道映射成统一模型名，并通过一把下游 API Key 提供 OpenAI 兼容访问和完整请求账本。

已实现 API Key 渠道与基础协议适配，并支持 Codex、Claude 与 Grok 的官方 OAuth 授权及 CLIProxyAPI / sub2api OAuth JSON 迁移。FusionGate 只接收用户主动完成的官方授权或用户主动导出的凭据文件，不保存账号密码、不抓取 Cookie，也不绕过服务商访问控制。

## 已实现

- Go 单二进制 + SQLite（WAL、busy timeout），无 Redis 依赖。
- 管理员会话、CSRF 校验、安全响应头；管理员密码以 PBKDF2-HMAC-SHA256 哈希存储。
- 上游凭据采用 **AES-256-GCM 字段加密**；下游 API Key 使用 SHA-256 哈希鉴权，同时保存 AES-256-GCM 加密副本，管理员可在控制台按需再次复制（升级前创建的旧 Key 仍不可恢复）。
- Provider 管理：OpenAI、OpenRouter、任意 OpenAI Compatible、Anthropic、Gemini，以及 Codex / Claude / Grok OAuth；普通 API 渠道保存后自动读取上游模型候选，OAuth 渠道可在授权或导入后手动识别，均由管理员勾选后批量创建路由；公开模型名与保存的上游模型 ID 统一规范为小写。
- 授权接入：支持 Codex / Claude 官方浏览器 OAuth（PKCE）、Grok 设备授权，以及 CLIProxyAPI、sub2api 导出的 Codex / Claude / Grok OAuth JSON。JSON 可一次选择多个文件，必须先识别再勾选，默认不选择账号；重复账号可跳过或只更新凭据。认证文件支持按厂商筛选、批量选择和敏感凭据 JSON 导出。
- 公共模型 / 别名与多条候选路由；渠道可通过直观开关整体开启或关闭，并设置默认 `1` 的渠道优先级。数字越大越优先，同级按渠道添加顺序自动故障转移；可在渠道页全局选择优先级故障转移、顺序轮询或智能选择。
- 被动健康感知：可配置最大并发、单次请求超时、失败阈值和冷却时间；支持熔断、单探针半开恢复、指数冷却、`Retry-After`。
- 安全故障转移：连接/超时、429、部分路由错误与 5xx 可切换备用；空流或首字节前断流可切换，首字节发出后绝不拼接第二家响应；图片传输结果不确定时不自动重放。
- 健康状态只处罚可归因于上游的失败；下游客户端主动取消不会污染 Provider 健康度。带 `Retry-After` 的 429 会立即进入冷却，避免继续冲击已限流渠道。
- `/v1/models`、`/v1/chat/completions`、`/v1/responses`、`/v1/messages`、`/v1/images/generations`。
  - OpenAI Compatible：Chat、Responses、Images；Chat / Responses 支持安全流式转发。
  - Provider 可选择“标准适配”或“原样透明转发”。透明模式不改写 JSON 正文，保留真实 User-Agent 与允许的端到端头部，只替换上游凭据并过滤 hop-by-hop、Cookie、转发链和网关内部头。
  - Anthropic / Gemini：OpenAI Chat 的文本消息非流式转换；Anthropic Messages 原生代理。
- API Key 可从实时可用模型中勾选白名单/拒绝规则，并支持 RPM 限流、图片权限与安全再次复制；删除会物理移除密钥记录，同时保留已脱敏的历史请求账本。
- 请求账本实时显示进行中请求、动态运行时间、每次故障转移尝试及上游首字节耗时。
- 独立 Token 用量中心：支持近 7/30/90 天和近一年范围，按日期、下游 Key、渠道、公开模型与实际上游模型统计请求数、尝试次数、输入、输出、缓存、推理和总 Token，包含趋势图、排行、筛选、分页与 usage 采集覆盖率。请求和 Token 明细自动保留一年。
- 标准 OpenAI Chat/Responses 与 Anthropic Messages 的非透明响应会被动读取 usage（包含流式末尾事件），Gemini 转换响应同步采集 usage；透明转发保持原样，不读取或修改响应载荷，控制台会明确标记为未采集而不是伪造为 0。
- 请求尝试账本按 `gateway_request_id` 聚合，记录 attempt、Provider、重试来源、状态、Token 与延迟，不记录 prompt / completion 正文。
- SSRF 默认保护：只接受 HTTPS 上游；解析并校验全部 DNS 地址，阻止 localhost、私网、链路本地、未指定和组播地址，限制重定向且禁止跨主机携带凭据。
- 默认白色管理主题，并支持一键切换深色主题；主题偏好保存在浏览器本地。
- Docker Compose 与非 root 容器配置。

## Codex / Claude / Grok 授权与迁移

- **浏览器授权**：管理台生成带 PKCE 的官方授权链接。授权结束后，将浏览器地址栏中的完整 `localhost` 回调地址粘贴回 FusionGate；回调只用于提取一次性授权码和校验 state，FusionGate 不要求服务器监听本机回调端口。
- **JSON 迁移**：可粘贴或批量上传 CLIProxyAPI / sub2api 导出的 Codex、Claude、Grok OAuth JSON。支持单对象、数组、连续 JSON，以及常见的 `accounts` / `data.accounts` / `credentials` / `token_data` 包装。单文件最大 2 MiB、单次总量最大 8 MiB；非 OAuth 账号和不支持的平台会被忽略。
- **批量导出**：可按厂商筛选并勾选最多 200 份认证文件，二次确认后下载 CLIProxyAPI 风格兼容 JSON。导出文件包含完整 Token，仅用于管理员主动迁移，不会写入页面、浏览器存储或应用日志。
- **安全保存**：Access Token、Refresh Token 与 ID Token 作为一个凭据对象使用 AES-256-GCM 加密后写入 SQLite；预览、管理 API、页面和错误信息均不回显 Token。
- **自动续期**：有 Refresh Token 时会在到期前自动刷新并保存轮换后的 Refresh Token；同一实例内的并发刷新会合并。刷新失败只标记授权状态并允许故障转移，不删除渠道。
- **路由**：Codex OAuth 支持 OpenAI Responses 路径适配；Claude OAuth 支持 Anthropic Messages 所需授权头。模型识别仍需管理员确认，系统不会在导入账号后自动创建模型路由。

请只导入你本人或你有权管理的账号凭据，并遵守对应服务商条款。FusionGate 不提供 Cookie 抓取、会话劫持或访问控制规避功能。

## 本机启动

```bash
cp .env.example .env
# 编辑 .env：填入 openssl rand -base64 32 的输出和一个高熵管理员密码
set -a; source .env; set +a
go run ./cmd/fusiongate
```

打开 `http://127.0.0.1:8787`，登录后依次：

1. 添加普通 API Provider（例如 `OpenAI`、`https://api.openai.com` 与 API Key），或在“授权接入”中完成 Codex / Claude 浏览器授权、导入 CLIProxyAPI / sub2api OAuth JSON。系统只识别候选模型，不会直接添加；在候选弹窗中勾选需要的模型并确认导入。公开模型名与保存的上游模型 ID 会统一转为小写。
2. 按需创建额外别名，例如公开名 `smart` → 上游模型 `gpt-4.1`。
3. 创建下游 API Key，从实时模型列表勾选允许/拒绝权限；完整 Key 可在管理员控制台再次复制。
4. 在任意 OpenAI SDK / 客户端中使用：

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer fg_..." -H "Content-Type: application/json" \
  -d '{"model":"smart","messages":[{"role":"user","content":"你好"}]}'
```

## 渠道优先级、故障转移与透明模式

故障转移只需要在“上游渠道”管理，不需要为每个模型重复设置：

- 每个渠道都有一个开启/关闭开关。关闭后，该渠道下的所有模型立即停止参与新请求；重新开启即可恢复。
- 添加渠道时优先级默认是 `1`，之后可直接修改。数字越大越优先；相同优先级按渠道添加顺序使用。
- 在渠道页选择一种全局故障转移模式：**优先级故障转移**（默认）、**顺序轮询**或**智能选择**。
- 当前渠道连接失败、超时、限流、返回可重试错误、触发熔断或达到最大并发时，会自动尝试下一个可用渠道。

模型路由页面只负责维护公开模型名与上游真实模型之间的映射。每一次故障转移都会写入独立 attempt，并保留上一跳失败原因。熔断中的渠道、正在执行半开探针的渠道，以及达到最大并发的渠道会被自动跳过。

透明模式用于上游要求原生协议字段或未知扩展字段的场景：请求正文按原始字节转发，不修改字段顺序、模型名、`user` 或 `stream_options`。因此透明路由要求公开模型名与上游模型名完全一致。它**不会伪造 Codex / Claude Code 身份，也不会隐藏真实客户端来绕过上游限制**；`client_policy` 只会检查真实传入的 User-Agent，并可将某个 Provider 限定为真实 Codex 或 Claude Code 请求。

## Docker Compose

```bash
cp .env.example .env
# 编辑 .env 后：
docker compose up -d --build
```

Compose 默认绑定 `127.0.0.1:8787`；请使用 Tailscale/WireGuard 或配置了 TLS 与访问控制的反向代理，而不是直接将后台暴露到公网。

## 服务器一键部署

生产部署支持 Debian 12 和 Ubuntu 22.04/24.04。提前将域名的 A/AAAA 记录指向服务器，并开放 TCP 80、TCP/UDP 443，然后运行：

```bash
curl -fsSL https://raw.githubusercontent.com/cupid532/fusiongate/main/deploy/install.sh | sudo bash
```

安装程序会：

- 从 Docker 官方 apt 仓库安装 Docker Engine 和 Compose 插件；
- 下载并在服务器本地构建 FusionGate；
- 生成独立的 256 位主密钥；
- 通过 Docker secrets 挂载主密钥和管理员密码；
- 配置 Caddy 自动申请和续期 HTTPS 证书；
- 启用非 root 容器、只读根文件系统、能力裁剪和健康检查；
- 安装 `fusiongatectl` 运维命令。

常用操作：

```bash
sudo fusiongatectl status
sudo fusiongatectl logs
sudo fusiongatectl update
sudo fusiongatectl backup
fusiongatectl health
```

建议先下载并审阅脚本，再执行：

```bash
curl -fsSLo install.sh https://raw.githubusercontent.com/cupid532/fusiongate/main/deploy/install.sh
less install.sh
sudo bash install.sh
```

完整上线检查见 [`DEPLOYMENT.md`](DEPLOYMENT.md)。

## 运行配置

| 变量 | 说明 |
|---|---|
| `FUSIONGATE_MASTER_KEY` | 必填，base64 编码的随机 32 字节主密钥。丢失后无法解密既有上游凭据。 |
| `FUSIONGATE_ADMIN_PASSWORD` | 必填，首次运行初始化管理员密码；之后必须保持一致。 |
| `FUSIONGATE_MASTER_KEY_FILE` | 可选，读取主密钥的文件路径；生产 Compose 使用该方式挂载 secret。 |
| `FUSIONGATE_ADMIN_PASSWORD_FILE` | 可选，读取管理员密码的文件路径；生产 Compose 使用该方式挂载 secret。 |
| `FUSIONGATE_ADDR` | 监听地址，默认 `127.0.0.1:8787`。 |
| `FUSIONGATE_DATA_DIR` | SQLite 数据目录，默认 `./data`。 |
| `FUSIONGATE_ALLOW_INSECURE_UPSTREAMS` | 仅可信开发环境可设 `true`，允许 HTTP。 |
| `FUSIONGATE_ALLOW_PRIVATE_UPSTREAMS` | 仅可信开发环境可设 `true`，允许私有网络上游。 |

## 备份与恢复

停止服务后，备份数据目录中的 `fusiongate.db`（以及 WAL / SHM 文件，如存在）和 `FUSIONGATE_MASTER_KEY`。恢复时同时恢复数据库并使用**相同主密钥**。建议对备份进行加密。

## 已知范围和后续工作

本 MVP 故意不包含支付、充值、用户注册、兑换码或商业计费模块。Gemini CLI OAuth、图像编辑、复杂工具调用/结构化输出、原生协议的完整流式转换、PostgreSQL、定时模型同步和备份 UI 仍需后续阶段实现。不要将订阅账号的等价 API 价值误称为实际上游扣费。
