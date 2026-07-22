# FusionGate

面向个人和小型可信团队的**自托管 AI 账号与 API 聚合网关**。它将多个上游渠道映射成统一模型名，并通过一把下游 API Key 提供 OpenAI 兼容访问和完整请求账本。

已实现 API Key 渠道与基础协议适配；Codex / Claude Code / Gemini CLI 官方 OAuth 目前仅保留 Provider 类型和扩展边界，尚未实现 OAuth 授权或网页自动化逻辑。项目不会保存账号密码、抓取 Cookie 或绕过服务商限制。

## 已实现

- Go 单二进制 + SQLite（WAL、busy timeout），无 Redis 依赖。
- 管理员会话、CSRF 校验、安全响应头；管理员密码以 PBKDF2-HMAC-SHA256 哈希存储。
- 上游凭据采用 **AES-256-GCM 字段加密**；下游 API Key 只保存 SHA-256 哈希，创建时仅显示一次。
- Provider 管理：OpenAI、OpenRouter、任意 OpenAI Compatible、Anthropic、Gemini；保存渠道时自动读取上游模型列表并创建同名路由，也可随时手动重新识别；OAuth 类型明确标为未接入适配器。
- 公共模型 / 别名与多条候选路由；优先级分层、平滑加权轮询、EWMA 延迟修正和最少并发修正。
- 被动健康感知：可配置最大并发、单次请求超时、失败阈值和冷却时间；支持熔断、单探针半开恢复、指数冷却、`Retry-After`。
- 安全故障转移：连接/超时、429、部分路由错误与 5xx 可切换备用；空流或首字节前断流可切换，首字节发出后绝不拼接第二家响应；图片传输结果不确定时不自动重放。
- 健康状态只处罚可归因于上游的失败；下游客户端主动取消不会污染 Provider 健康度。带 `Retry-After` 的 429 会立即进入冷却，避免继续冲击已限流渠道。
- `/v1/models`、`/v1/chat/completions`、`/v1/responses`、`/v1/messages`、`/v1/images/generations`。
  - OpenAI Compatible：Chat、Responses、Images；Chat / Responses 支持安全流式转发。
  - Provider 可选择“标准适配”或“原样透明转发”。透明模式不改写 JSON 正文，保留真实 User-Agent 与允许的端到端头部，只替换上游凭据并过滤 hop-by-hop、Cookie、转发链和网关内部头。
  - Anthropic / Gemini：OpenAI Chat 的文本消息非流式转换；Anthropic Messages 原生代理。
- API Key 模型白名单/拒绝规则、RPM 限流、撤销、图片权限。
- 请求尝试账本按 `gateway_request_id` 聚合，记录 attempt、Provider、重试来源、状态、Token、延迟与费用，不记录 prompt / completion 正文；费用标记为 `actual`、`estimated` 或 `unknown`。
- SSRF 默认保护：只接受 HTTPS 上游；解析并校验全部 DNS 地址，阻止 localhost、私网、链路本地、未指定和组播地址，限制重定向且禁止跨主机携带凭据。
- Docker Compose 与非 root 容器配置。

## 本机启动

```bash
cp .env.example .env
# 编辑 .env：填入 openssl rand -base64 32 的输出和一个高熵管理员密码
set -a; source .env; set +a
go run ./cmd/fusiongate
```

打开 `http://127.0.0.1:8787`，登录后依次：

1. 添加 Provider（例如 `OpenAI`、`https://api.openai.com` 与 API Key），系统会自动识别模型并建立同名路由。
2. 按需创建额外别名，例如公开名 `smart` → 上游模型 `gpt-4.1`。
3. 创建下游 API Key，并立即复制一次性显示的 Key。
4. 在任意 OpenAI SDK / 客户端中使用：

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer fg_..." -H "Content-Type: application/json" \
  -d '{"model":"smart","messages":[{"role":"user","content":"你好"}]}'
```

## 负载均衡与透明模式

同一公开模型可以配置多个候选。FusionGate 先选择最低的路由优先级和 Provider 优先级层，再在该层执行健康修正的平滑加权轮询；熔断中、半开探测占用或达到并发上限的渠道会被排除。每一次故障转移都会写入独立 attempt，并保留上一跳失败原因。

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

本 MVP 故意不包含支付、充值、用户注册、兑换码或商业计费模块。完整官方 OAuth / CLI 流程、图像编辑、复杂工具调用/结构化输出、原生协议的完整流式转换、PostgreSQL、定时模型同步和备份 UI 仍需后续阶段实现。不要将订阅账号的等价 API 价值误称为实际上游扣费。
