# FusionGate

当前版本以 [`internal/fusiongate/version.go`](internal/fusiongate/version.go) 为准。所有 Agent 和贡献者在更新前必须遵循 [AGENTS.md](AGENTS.md) 中的版本递增规则。

面向个人和小型可信团队的**自托管 AI 账号与 API 聚合网关**。它将多个上游渠道映射成统一模型名，并通过一把下游 API Key 提供 OpenAI 兼容访问和完整请求账本。

已实现 API Key 渠道与基础协议适配，并支持 Codex、Claude 与 Grok 的官方 OAuth 授权及常见 OAuth JSON 迁移。FusionGate 只接收用户主动完成的官方授权或用户主动导出的凭据文件，不保存账号密码、不抓取 Cookie，也不绕过服务商访问控制。

## 快速导航

- [核心能力](#核心能力)
- [五分钟上手](#五分钟上手)
- [渠道优先级、故障转移与透明模式](#渠道优先级故障转移与透明模式)
- [Codex / Claude / Grok 授权与迁移](#codex--claude--grok-授权与迁移)
- [IP 池与渠道网络出口](#ip-池与渠道网络出口)
- [服务器一键部署](#服务器一键部署)
- [运行配置](#运行配置)
- [备份与恢复](#备份与恢复)
- [生产上线检查](DEPLOYMENT.md)

## 核心能力

| 能力 | 说明 |
|---|---|
| 统一模型入口 | 对外提供 OpenAI Chat、Responses、Images 与 Anthropic Messages 接口。 |
| 多渠道故障转移 | 支持优先级、轮询、智能轮询和自适应调度；熔断后每 30 秒自动探测恢复，手动关闭的渠道不会被自动开启。 |
| 渠道多 Key | 每张 Key 独立识别和勾选模型、检活、排序、启停并配置网络出口；Key × 模型检活结果逐项展示。 |
| OAuth 账号接入 | 支持 Codex、Claude 官方浏览器授权，Grok 设备授权和兼容 OAuth JSON 迁移。 |
| 固定网络出口 | 支持常见代理分享链接与 sing-box outbound JSON；节点失败时严格故障转移，不静默回落直连。 |
| 请求与费用可观测 | 实时请求账本支持精确到秒的开始/结束时间、状态、渠道、关键词和条数筛选，并统计 Token、延迟和估算费用。 |
| 安全默认值 | SQLite 单机部署、字段级 AES-256-GCM 加密、CSRF、安全响应头、SSRF 防护和非 root 只读容器。 |

```text
OpenCode / SDK / 应用
          │  OpenAI / Anthropic API
          ▼
     FusionGate
       ├── 模型别名与权限
       ├── Key 选择与固定出口
       ├── 熔断、恢复与故障转移
       └── 请求、Token 与费用账本
          │
          ├── API Key Provider
          ├── Codex / Claude / Grok OAuth
          └── OpenAI Compatible / OpenRouter / Gemini
```

## 详细能力

- Go 单二进制 + SQLite（WAL、busy timeout），无 Redis 依赖。
- 管理员会话、CSRF 校验、安全响应头；管理员密码以 PBKDF2-HMAC-SHA256 哈希存储。
- 上游凭据采用 **AES-256-GCM 字段加密**；下游 API Key 使用 SHA-256 哈希鉴权，同时保存 AES-256-GCM 加密副本，管理员可在控制台按需再次复制（升级前创建的旧 Key 仍不可恢复）。
- Provider 管理：OpenAI 官方 Key、Grok / xAI 官方 Key（默认 `https://api.x.ai`）、OpenRouter、任意 OpenAI Compatible、Anthropic、Gemini，以及 Codex / Claude / Grok OAuth；官方 API Key 与 OAuth 认证文件是独立渠道类型。普通 API 渠道可随时编辑名称、类型、Base URL、API Key 与调度设置，更换 Key 无需删除渠道或重建模型路由；保存后自动读取上游模型候选，由管理员勾选后批量创建路由；OAuth 认证文件在授权或 JSON 导入完成后会自动识别并默认添加全部可用模型，之后仍可手动编辑或删除路由；公开模型名与保存的上游模型 ID 统一规范为小写。
- IP 池与固定出口：默认所有渠道使用服务器本机直连；管理员可粘贴 SOCKS4/5、HTTP(S)、Shadowsocks、Trojan、VLESS（含 Reality）、VMess、Hysteria/Hysteria2、TUIC、AnyTLS 分享链接，或单个受支持的 sing-box outbound JSON，并为普通 API 渠道或 OAuth 认证文件指定节点。转发、模型识别、检活、OAuth 续签和额度查询使用同一渠道出口；节点故障时严格失败并交给现有渠道故障转移，不会静默泄漏到本机直连。
- 授权接入：支持 Codex / Claude 官方浏览器 OAuth（PKCE）、Grok 设备授权，以及常见工具导出的 Codex / Claude / Grok OAuth JSON。JSON 可一次选择多个文件，必须先识别再勾选，默认不选择账号；重复账号可跳过或只更新凭据。认证文件支持按厂商筛选、批量选择和敏感凭据 JSON 导出。
- 安全检活：认证文件可自由勾选后一键检活；默认“基础检活”只读取模型列表、不会生成内容，明确选择“真实生成测速”时才发送最多 1 token 的请求并记录首字节/总耗时。任务采用低并发、单项超时、重复探测互斥、可取消和逐项结果展示，不会因一次手动检活自动启停或删除账号。
- 公共模型 / 别名与多条候选路由；既可删除单条渠道映射，也可从模型页一次删除某个公开模型的全部映射。“上游渠道”列表支持拖拽或上下按钮调整全局渠道位置，刷新后保留，并统一用于 API 渠道与 OAuth 认证渠道调度。渠道可通过直观开关整体开启或关闭，并设置默认 `1` 的渠道优先级；可在渠道页全局选择优先级、逐个轮询、智能轮询或智能选择。
- 渠道支持归档：归档用于“余额耗尽但是优秀的站点”。归档渠道只出现在“归档”列表，不会出现在“全部渠道”“已开启”或“熔断冷却”列表，也不会参与新请求调度。
- 被动健康感知：可配置最大并发、单次请求超时、失败阈值和冷却时间；支持熔断、每 30 秒主动恢复探测、单探针半开恢复、指数冷却和 `Retry-After`。自动熔断不会改写管理员开关，手动关闭的渠道不会自动开启。429 会显示为“限流”并立即进入至少 5 分钟冷却，不会因短耗时错误响应污染自适应延迟统计。
- 安全故障转移：连接/超时、429、部分路由错误与 5xx 可切换备用；空流或首字节前断流可切换，首字节发出后绝不拼接第二家响应；图片请求在尚未向客户端写出响应前也会自动切换备用渠道（例如 input 超时/5xx 后无缝落到 Codex Plus），不会固定某一家。
- 健康状态只处罚可归因于上游的失败；下游客户端主动取消不会污染 Provider 健康度。智能选择优先依据成功请求的首字节 EWMA，而不是被输出长度放大的完整响应耗时；请求账本会对 2 万/4 万以上输入 Token 标记“大上下文/超大上下文”。
- `/v1/models`、`/v1/chat/completions`、`/v1/responses`、`/v1/messages`、`/v1/images/generations`。
  - 所有 `/v1/*` 网关接口支持浏览器跨域调用、无需鉴权的 `OPTIONS` 预检和常用 SDK 自定义请求头；管理后台接口不开放跨域。
  - OpenAI Compatible：Chat、Responses、Images；Chat / Responses 支持安全流式转发。
  - Codex OAuth Plus 生图兼容：发现到 `gpt-5.5` 时自动提供 `gpt-image-1` 与 `gpt-image-2` 图像别名；标准 `POST /v1/images/generations` 会转换成 Codex Responses 的 `image_generation` 内置工具调用，并把 SSE 中的真实图片结果转换回 OpenAI `b64_json` 响应。Codex OAuth 路径每次只支持 `n=1`（ChatGPT 账号侧工具一次只出一张，且并发 fan-out 易被限流/拖垮）；需要多图时请对 OpenAI Compatible 生图渠道传 `n`，或对 Codex 路径发起多次请求。支持上游接受的 `size`、`output_format`、`output_compression`、`background`、`moderation` 与 `partial_images` 参数；不伪造 URL 或透明背景能力。Codex 生图默认至少 180s 超时下限。
  - Provider 可选择“标准适配”或“原样透明转发”。透明模式不改写 JSON 正文，保留真实 User-Agent 与允许的端到端头部，只替换上游凭据并过滤 hop-by-hop、Cookie、转发链和网关内部头。
  - Anthropic / Gemini：OpenAI Chat 的文本消息非流式转换；Anthropic Messages 支持原生代理，也可安全转换到 OpenAI / OpenRouter / OpenAI Compatible / Grok Chat，覆盖文本、图片、工具调用、工具结果以及 Anthropic SSE 流。
- 下游 API Key 可从实时可用模型中勾选白名单/拒绝规则，并支持 RPM 限流、图片权限、到期时间、USD 费用预算与安全再次复制；到期或累计估算费用达到预算后会停止接受新请求。费用在上游返回 usage 后结算，因此最后一个并发或在途请求可能产生少量超额；删除会物理移除密钥记录，同时保留已脱敏的历史请求账本。
- 请求账本实时显示进行中请求、动态运行时间、每次故障转移尝试及上游首字节耗时。
- 请求账本支持精确到秒的本地日期时间范围、状态、渠道、模型/协议/请求 ID/错误关键词与 50/100/200 条返回数量组合筛选；筛选在服务端执行，实时轮询保持当前条件。
- 官方价格同步：默认每 1 小时读取 OpenAI、xAI、Gemini、Claude 官方定价页面与 OpenRouter 兜底目录，并按公开模型和上游模型 ID 更新非手动价格路由；管理台可随时手动同步。可通过 `FUSIONGATE_PRICING_SYNC_INTERVAL` 调整周期，设为 `0`、`off` 或 `false` 可关闭后台自动同步。支持缓存输入价格和长上下文分档。费用是基于上游 usage 的估算值，最终账单仍以上游服务商为准。
- 独立用量与费用中心：支持近 7/30/90 天和近一年范围，按日期、下游 Key、渠道、公开模型与实际上游模型统计请求数、尝试次数、输入、输出、缓存、推理、总 Token 和估算费用，包含趋势图、排行、筛选、分页、usage 采集覆盖率与官方价格覆盖率。请求、Token 和费用明细自动保留一年。
- 标准 OpenAI Chat/Responses 与 Anthropic Messages 的非透明响应会被动读取 usage（包含流式末尾事件），Gemini 转换响应同步采集 usage；透明转发保持原样，不读取或修改响应载荷，控制台会明确标记为未采集而不是伪造为 0。
- 请求尝试账本按 `gateway_request_id` 聚合，记录 attempt、Provider、重试来源、状态、Token 与延迟，不记录 prompt / completion 正文。
- Codex Chat → Responses 桥接保留 OpenCode / OpenAI Compatible 的 `reasoning_effort`，并转换为 `reasoning.effort`，避免推理强度回落为上游默认值。
- SSRF 默认保护：只接受 HTTPS 上游；解析并校验全部 DNS 地址，阻止 localhost、私网、链路本地、未指定和组播地址，限制重定向且禁止跨主机携带凭据。
- 默认白色管理主题，并支持一键切换深色主题；主题偏好保存在浏览器本地。
- Docker Compose 与非 root 容器配置。

## Codex / Claude / Grok 授权与迁移

- **浏览器授权**：管理台生成带 PKCE 的官方授权链接。授权结束后，将浏览器地址栏中的完整 `localhost` 回调地址粘贴回 FusionGate；回调只用于提取一次性授权码和校验 state，FusionGate 不要求服务器监听本机回调端口。
- **JSON 迁移**：可粘贴或批量上传常见结构的 Codex、Claude、Grok OAuth JSON。支持单对象、数组、连续 JSON，以及常见的 `accounts` / `data.accounts` / `credentials` / `token_data` 包装。单文件最大 2 MiB、单次总量最大 8 MiB；非 OAuth 账号和不支持的平台会被忽略。
- **批量导出**：可按厂商筛选并勾选最多 200 份认证文件，二次确认后下载兼容迁移 JSON。导出文件包含完整 Token，仅用于管理员主动迁移，不会写入页面、浏览器存储或应用日志。
- **安全保存**：Access Token、Refresh Token 与 ID Token 作为一个凭据对象使用 AES-256-GCM 加密后写入 SQLite；预览、管理 API、页面和错误信息均不回显 Token。
- **自动续期**：有 Refresh Token 时会在到期前自动刷新并保存轮换后的 Refresh Token；同一实例内的并发刷新会合并。刷新失败只标记授权状态并允许故障转移，不删除渠道。
- **路由**：Codex OAuth 支持 OpenAI Responses 路径适配；Claude OAuth 支持 Anthropic Messages 所需授权头。模型识别仍需管理员确认，系统不会在导入账号后自动创建模型路由。

请只导入你本人或你有权管理的账号凭据，并遵守对应服务商条款。FusionGate 不提供 Cookie 抓取、会话劫持或访问控制规避功能。

## 五分钟上手

### 1. 本机启动

```bash
cp .env.example .env
# 编辑 .env：填入 openssl rand -base64 32 的输出和一个高熵管理员密码
set -a; source .env; set +a
go run ./cmd/fusiongate
```

### 2. 完成基础配置

打开 `http://127.0.0.1:8787`，登录后依次：

1. 添加普通 API Provider（例如 `OpenAI`、`https://api.openai.com` 与 API Key），或在“授权接入”中完成 Codex / Claude 浏览器授权、导入兼容 OAuth JSON。系统只识别候选模型，不会直接添加；在候选弹窗中勾选需要的模型并确认导入。公开模型名与保存的上游模型 ID 会统一转为小写。需要固定出口时，可先在“IP 池”添加节点，再在渠道的“网络出口”中选择；不选择即保持本机直连。
2. 按需创建额外别名，例如公开名 `smart` → 上游模型 `gpt-4.1`。
3. 创建下游 API Key，从实时模型列表勾选允许/拒绝权限；完整 Key 可在管理员控制台再次复制。
### 3. 发起第一个请求

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer fg_..." -H "Content-Type: application/json" \
  -d '{"model":"smart","messages":[{"role":"user","content":"你好"}]}'
```

## 渠道优先级、故障转移与透明模式

故障转移只需要在“上游渠道”管理，不需要为每个模型重复设置：

- 每个渠道都有一个开启/关闭开关。关闭后，该渠道下的所有模型立即停止参与新请求；重新开启即可恢复。
- 添加渠道时优先级默认是 `1`，之后可直接修改。数字越大越优先；相同优先级按“上游渠道”列表可拖拽调整的全局位置使用。
- 在渠道页选择一种全局故障转移模式：**优先级**（默认）、**逐个轮询**、**智能轮询**或**智能选择**。逐个轮询固定从排序最前的可用渠道开始，仅失败时继续下一渠道；智能轮询让每个新请求主动移动到下一可用渠道；智能选择则根据成功请求的首字节延迟、失败历史和当前并发动态分配，同时保留当前请求内的无缝故障转移。
- 当前渠道连接失败、超时、限流、返回可重试错误、触发熔断或达到最大并发时，会自动尝试下一个可用渠道。

“上游渠道”页面的拖拽手柄和上下按钮会即时保存全局渠道位置；逐个轮询和智能轮询直接使用该顺序，优先级模式则先比较渠道优先级，再使用该顺序。模型路由页面继续维护公开模型名与上游真实模型映射，并可设置同一渠道内多条映射的次级顺序。每一次故障转移都会写入独立 attempt，并保留上一跳失败原因。熔断中的渠道、正在执行半开探针的渠道，以及达到最大并发的渠道会被自动跳过。

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
| `FUSIONGATE_PRICING_SYNC_INTERVAL` | 官方价格同步间隔，默认 `24h`，最低 `1h`；设为 `0`、`off` 或 `false` 可关闭。 |
| `FUSIONGATE_ALLOW_INSECURE_UPSTREAMS` | 仅可信开发环境可设 `true`，允许 HTTP。 |
| `FUSIONGATE_ALLOW_PRIVATE_UPSTREAMS` | 仅可信开发环境可设 `true`，允许私有网络上游。 |
| `FUSIONGATE_SING_BOX_PATH` | 可选，sing-box 可执行文件路径；官方 Docker 镜像已内置固定版本，本机运行仅在启用 IP 池节点时需要安装。 |

## IP 池与渠道网络出口

IP 池由 FusionGate 管理节点元数据与渠道绑定，实际多协议网络栈由镜像内固定版本的 [sing-box](https://sing-box.sagernet.org/) 提供：

- 分享链接和其中的密码、UUID、Reality 公钥等信息使用与上游凭据相同的 AES-256-GCM 主密钥加密后写入 SQLite，管理 API 和页面不回显原始链接。
- 运行配置只写入权限为 `0700/0600` 的临时目录；sing-box 读取并打开仅监听 `127.0.0.1` 的本地 SOCKS 入口后立即删除配置文件，不会把节点密钥明文持久化到 `/data`。
- FusionGate 仍在本地解析上游 API 域名并过滤私网、回环、链路本地、未指定与组播地址，使用代理不会绕过原有 SSRF 防护。
- 已绑定节点不可用或被暂停时，渠道会产生可重试网络失败并按现有模型路由切换到其他渠道；不会自动改用服务器真实出口。删除仍被渠道引用的节点会被拒绝。
- 分享 URI 无法完整表达的高级配置可粘贴单个 sing-box outbound JSON。仅允许代理型 outbound，禁止 `direct`、`block`、`selector` 等可改变隔离语义的类型。

源码本机运行且需要 IP 池时，请安装兼容的 sing-box，并按需设置 `FUSIONGATE_SING_BOX_PATH`。不创建或不启用任何节点时，FusionGate 不启动 sing-box，行为与升级前一致。

## 备份与恢复

停止服务后，备份数据目录中的 `fusiongate.db`（以及 WAL / SHM 文件，如存在）和 `FUSIONGATE_MASTER_KEY`。恢复时同时恢复数据库并使用**相同主密钥**。建议对备份进行加密。

## 已知范围和后续工作

FusionGate 的费用统计与密钥预算属于管理侧估算和访问限制，不包含支付、充值、用户注册、兑换码或商业计费模块。Gemini CLI OAuth、图像编辑、跨协议结构化输出的完整等价转换、PostgreSQL、定时模型同步和备份 UI 仍需后续阶段实现。不要将订阅账号的等价 API 价值误称为实际上游扣费，最终费用以上游账单为准。
