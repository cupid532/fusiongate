# FusionGate

根目录 [`VERSION`](VERSION) 是发布入口，并且必须与 [`internal/fusiongate/version.go`](internal/fusiongate/version.go) 完全一致。所有 Agent 和贡献者在更新前必须遵循 [AGENTS.md](AGENTS.md) 中的版本递增规则。

面向个人和小型可信团队的**自托管 HTTP AI 聚合网关**。通过一把下游 API Key 访问多个上游，管理鉴权、模型映射、IP 出口、故障转移、用量与请求元数据账本。渠道支持客户端协议时优先透传；需要转换时按渠道接口方式与现有协议桥接能力处理，不保证所有请求控制都可跨协议转换。

> **V3.18 控制台整理**：上游渠道列表可直接勾选并批量设置默认出口、优先级、分组、启停及归档。渠道编辑使用固定导航，渠道参数、Key、模型分别明确保存；Key 的独立出口不会被批量默认出口覆盖。当前行为以前述新版本说明为准，V3.12 / V3.13 的透传与计费限制是历史行为。

> **V3.14 行为变化**：四个入口（Chat Completions、Responses、Messages、Gemini generateContent）对任意渠道可用。渠道支持客户端协议时仍原样透传；渠道返回“不支持该接口”（404/405/501 或 invalid URL/unsupported 类 400）时，同一渠道自动改用流式协议转换重试一次，并记住 30 分钟，后续请求直接转换（例如 Codex 走只支持 Chat 的 AIAPI）。Token（输入/缓存/输出/推理）与费用恢复统计，模型映射与别名恢复（只改写顶层 model 字段），渠道可设置接口方式（自适应或固定）。
>
> **V3.13 行为变化**：公开推理入口继续原样透传，但把**身份适配**与**协议转换**分开——Codex/Claude/Grok 授权渠道重新参与推理，只用它们自己的原生端点，并且只替换凭据与身份头，客户端正文一字不改。尚无经过验证实现的专用类型保留配置与启用状态、但不被选中，账本会记录原因。路由只有一种策略（优先级固定），旧策略值会被拒绝且不再驱动路由。同一任务会粘在故障转移后到达的渠道上，不再回环到更高优先级。新请求不解析 token/usage，费用为未知，金额预算和余额不能准确约束新消费。

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
| 原样转发 | Chat、Responses、Messages、Images、Audio、Embeddings 等已有推理入口；不增加任意 URL 代理。 |
| 多渠道故障转移 | 单一优先级顺序（渠道优先级 → 全局位置 → 渠道编号，渠道内再按映射优先级/顺序/编号）；每个渠道有独立重试预算，用尽才前进；保留冷却、熔断与半开恢复。 |
| 渠道多 Key | Key 独立启停、模型权限与 IP 出口；优先尝试其他渠道，再尝试未用备用 Key。 |
| 请求元数据账本 | 渠道、Key 脱敏标识、策略、候选、尝试序列、停止原因及延迟；不保存 prompt/completion。 |
| 账号与历史管理 | 保留 OAuth 授权/导入/导出及历史用量；Codex/Claude/Grok 授权账号以身份适配方式参与推理，无验证实现的专用类型仍只保留配置。 |
| 安全默认值 | AES-256-GCM 凭据加密、网关 Key 权限、限流、CSRF、SSRF 防护与固定网络出口。 |

## 详细能力

- Go 单二进制 + SQLite；React 管理台资源内嵌。普通 API 渠道可编辑 Base URL、凭据、调度与出口，不必重建渠道。
- 原始请求体不重新序列化：模型名、工具字段、未知字段和 `stream` 原样保留。只读提取路由所需元数据；multipart 和二进制输出不做协议转换。
- 原样保留端到端响应头、状态、压缩正文和 SSE 字节；不隐式解压或跟随 3xx，不伪造结束事件，不把未知事件或空 2xx 判成网关 502。
- 必要代理例外：目标 Host、上游鉴权替换、hop-by-hop/Connection 指定头过滤、传输分帧及网关安全控制。不会向上游泄露网关 Key、管理员 Cookie。应用层字节一致不等于 TCP 分包、HTTP 头大小写完全一致。
- `/v1/models` 是可参与纯透传路由的聚合目录，而非任何一家上游目录的原样副本。不能把“目录存在”视为任意接口都可用。
- 模型映射与别名可将客户端公开名映射到上游模型名；需要时只改写顶层 `model` 字段。渠道接口方式决定自适应或固定上游协议，实际透传/转换结果以请求账本记录为准。
- 管理员显式检活可能产生上游费用；检活结果只描述其测试路径，不改变真实请求协议。不会自动开启用户关闭的渠道。
- 下游 Key 保留模型权限、图片/音频权限、RPM、过期撤销与安全复制。V3.14 起恢复 Token、估算费用与预算统计；上游未报告的用量和未配置的模型价格不能当成实际账单，最终扣费仍以服务商为准。
- 未采集的 token/usage 与未能估算的费用应视为未知；历史统计与价格管理保留，不能把未采集费用解释为免费或零消费。
- IP 池支持现有 sing-box 出口；节点失败不静默回落直连。SSRF 与加密凭据保护继续有效。

## 跨协议桥接与字段策略

- **惰性桥接**：客户端协议与渠道原生协议一致时按字节透传；渠道明确表示“不支持该接口”时才转换（404/405/501、invalid URL/unsupported 类 400，或网关自己的 `gateway_provider_protocol_unavailable`）。这次判定按渠道、模型与协议记住 30 分钟，并写入 `channel_protocol_capabilities`，重启后继续生效——否则每次重启的第一次请求都会再去撞一次已知不存在的端点。
- **字段三分类**：桥接对每个字段只有三种处置，全部声明在 `internal/fusiongate/bridge_field_policy.go` 一张表里。**保留**（转换器能表达）；**丢弃**（只影响答案形态：采样惩罚、缓存时长、归属信息、冗长度等，丢掉仍是可用答案）；**拒绝**（会改变客户端拿到什么：结构化输出契约、多选、会话状态、能联网的托管工具、无法表达的自定义格式）。表里没有的字段一律拒绝，不会静默消失。
- **拒绝会写明字段**：错误与账本会给出字段名（`capability_not_supported: <field> <reason>`），控制台“字段兼容”页按渠道汇总每条渠道被哪个字段拦住过、丢过什么、各发生多少次。
- **按请求同意降级**：需要 JSON 模式却只有 Anthropic 类渠道时，调用方可带 `X-FusionGate-Accept-Lost-Contract: response_format` 表示本次请求可接受契约丢失（该渠道无法兑现时会被丢弃而不是拒绝）。默认不开，名单外字段一律忽略，工具 schema 契约（`tools.function.strict`）即使用此头也不放宽。
- **声明只排序、不排除**：渠道声明的 `protocol:<name>` 能力决定转换目标**优先尝试顺序**，绝不会移除渠道类型本身支持的其他协议——声明是运营者"追加一个可选项"（例如给 Anthropic 类渠道额外声明原生 Responses 端点），不是替换。真正排除某个目标只有一种依据：**有过期时间的学习事实**，且任一成功即提前清除。
- **学习只认端点级证据**：只有 404 / 405 / 415 / 501 会被当作"该端点不存在"；400 / 422 需要读正文才能区分"没有这个端点"和"请求有问题"，交由 `protocolUnsupportedSignal` 判断；5xx 与超时属于**健康**证据，由熔断器负责，不会被记成协议事实——否则一次 500 就会把一个只是不舒服的端点压掉。

## Codex / Claude / Grok 授权与迁移

- **浏览器授权**：管理台生成带 PKCE 的官方授权链接。授权结束后，将浏览器地址栏中的完整 `localhost` 回调地址粘贴回 FusionGate；回调只用于提取一次性授权码和校验 state，FusionGate 不要求服务器监听本机回调端口。
- **JSON 迁移**：可粘贴或批量上传常见结构的 Codex、Claude、Grok OAuth JSON。支持单对象、数组、连续 JSON，以及常见的 `accounts` / `data.accounts` / `credentials` / `token_data` 包装。单文件最大 2 MiB、单次总量最大 8 MiB；非 OAuth 账号和不支持的平台会被忽略。
- **批量导出**：可按厂商筛选并勾选最多 200 份认证文件，二次确认后下载兼容迁移 JSON。导出文件包含完整 Token，仅用于管理员主动迁移，不会写入页面、浏览器存储或应用日志。
- **安全保存**：Access Token、Refresh Token 与 ID Token 作为一个凭据对象使用 AES-256-GCM 加密后写入 SQLite；预览、管理 API、页面和错误信息均不回显 Token。
- **自动续期**：有 Refresh Token 时会在到期前自动刷新并保存轮换后的 Refresh Token；同一实例内的并发刷新会合并。刷新失败只标记授权状态并允许故障转移，不删除渠道。
- **V3.13 推理边界**：Codex、Claude、Grok 授权渠道以身份适配方式参与推理——请求只发往该 API 自己暴露的端点（例如 Codex 只有 `/v1/responses`），凭据、账号头与 `User-Agent` 由网关按渠道设置，正文不转换。没有经过验证实现的专用类型（Gemini、Antigravity、Qwen、iFlow、Grok 网页版等）配置与启用状态照旧保留，但不会被选中，账本的候选排除原因会写明 `specialized_adapter_unsupported`。导入或续期成功只说明凭据可用，通道能否服务某个端点仍以该通道类型实际支持的原生端点为限。

请只导入你本人或你有权管理的账号凭据，并遵守对应服务商条款。FusionGate 不提供 Cookie 抓取、会话劫持或访问控制规避功能。

## 五分钟上手

### 控制台管理流程

- 渠道列表常驻复选框，选中后显示批量工具栏；筛选不会静默清除已选渠道，隐藏选项会明确计数。默认展示配置位置，也可切换到调度优先级顺序；拖动只调整同优先级的全局位置。
- “指定出口”只修改渠道默认节点。Key 可继承渠道、强制直连或指定独立节点；修改默认出口不会覆盖后两种配置。没有节点时可明确选择本机直连；停用节点不允许新选。
- 本次保持后端不变。现有单 Key PATCH 的出口字段绑定存在错位，故 Key 独立出口仅展示、不提交修改，防止错误改变模型策略。既有独立出口继续保留；继承渠道出口的 Key 可通过渠道默认出口调整。修复此接口需要另行授权后端变更。
- 渠道参数使用差量 PATCH；选择本机直连发送 `ip_pool_node_id: 0`。Key 每行需点击“保存 Key”，模型需点击“保存模型配置”；测试、识别模型与删除是独立执行动作。检活和测试可能产生上游费用。
- 优先级、分组和归档的批量操作通过现有单渠道接口逐项执行，不具备整体原子性。结果区分成功、失败、待核对、未执行；仅重试失败项。网络中断后先读取当前配置，不能假定请求没有执行。
- OAuth 专属授权与导入仍在认证文件页，默认出口选择与普通 API 渠道共用组件。模型路由中的渠道链接可直达模型配置，渠道列表可跳转到带渠道筛选的请求账本。
- 前端开发：在 `web/` 运行 `npm run dev`，默认代理到 `http://127.0.0.1:8787`；需要其他实例时设置 `FUSIONGATE_DEV_UPSTREAM`。发布运行 `deploy/build-web.sh`，脚本先按锁文件执行 `npm ci`，再构建并同步内嵌资源。

### 1. 本机启动

```bash
cp .env.example .env
# 编辑 .env：填入 openssl rand -base64 32 的输出和一个高熵管理员密码
set -a; source .env; set +a
go run ./cmd/fusiongate
```

### 2. 完成基础配置

打开 `http://127.0.0.1:8787`，登录后依次：

1. 添加普通 API 渠道（例如 OpenAI、`https://api.openai.com` 与首张上游 API Key），随后进入模型管理。可以识别并勾选上游模型，也可以手动或批量添加；上游未提供模型列表接口不影响手动配置。需要固定出口时先在 IP 池添加节点，再指定渠道出口。
2. 配置请求模型名到上游模型名的路由，例如 `my-coding → 渠道 A / vendor/model-v2`，并按需加入备用渠道。请求名、上游名可以不同，调用别名共享同一模型组；接口兼容性仍由渠道能力与现有协议转换支持决定。检查可用候选数量，只有一个候选时没有后备。
3. 创建下游访问 Key 并设置模型权限、RPM 与到期时间。模型保存不自动发起生成检活，上游消费限制与实际账单仍以服务商为准。

#### V3.44 手动模型与路由编辑

- **渠道 → 模型管理 → 手动添加 / 批量添加模型**：每行一个上游模型名，选择明确的 Key 范围；可以填写显示名称与能力，并同时创建同名或自定义请求入口。不自动修改未选 Key 的权限，不改变 Key 自身启停。
- **编辑模型**：编辑当前 Key 的上游名称、显示名称和能力；改名时列出关联路由，可选择同步更新。若其他未选 Key 仍保留旧模型，需一并选中才能同步共享路由。复制已选模型可为本渠道其他 Key 配置相同模型权限。
- 后续识别保留手动元数据和启停状态。移除 Key 模型权限保留路由配置；没有支持该模型的 Key 时，该成员自然不可调度，不因保留配置而放宽权限。
- **模型路由**：新建请求模型或添加渠道成员；编辑完整映射，保存前预览支持的 Key 数与当前可用数。未知上游名称可自由输入，但创建路由不等于授予 Key 使用权限；可跳转对应渠道模型管理补齐清单。
- **重命名请求模型组**：整个组原子更新，可保留旧名作为别名；调用别名及下游访问权限一起检查。无法保持原权限边界时拒绝改名并要求先调整权限，不能因改名绕过禁止规则。
- 保存与检活分离：新模型未经验证不代表上游实际可用。手动检活或真实请求可能产生费用。

### 3. 发起第一个请求

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer fg_..." -H "Content-Type: application/json" \
  -d '{"model":"YOUR_NATIVE_MODEL_ID","messages":[{"role":"user","content":"你好"}]}'
```

## 渠道优先级、故障转移与透明模式

起始渠道策略全局配置，但每个原生模型必须有自己的有效候选渠道：

- 每个渠道都有一个开启/关闭开关。关闭后，该渠道下的所有模型立即停止参与新请求；重新开启即可恢复。
- 添加渠道时优先级默认是 `1`，之后可直接修改。数字越大越优先；相同优先级按“上游渠道”列表可拖拽调整的全局位置使用。
- 路由只有一种策略：**优先级固定**。每次请求按渠道优先级从高到低、再按全局位置、再按渠道编号选择起点；同一渠道内多条映射由映射级顺序决定先后。旧的轮换/自适应策略已移除，保存其他值会被明确拒绝，数据库里遗留的旧值也不会再驱动路由。
- 同一任务在故障转移到某个渠道后会**保持**在该渠道上（会话粘性，含空闲/最长保留与容量上限）：任务不会因为高优先级渠道恢复就回环过去，只会沿它已经走过的顺序继续往下。
- 当前渠道连接失败、超时、限流、返回可重试错误、触发熔断或达到最大并发时，会自动尝试下一个可用渠道。

“上游渠道”页面的拖拽手柄和上下按钮会即时保存全局渠道位置：先比较渠道优先级（数字越大越优先），优先级相同再按该位置，最后按渠道编号排序。每个渠道拥有一个由它的全部 Key 共享的小额重试预算；预算用尽后才前进到下一个渠道，而上游返回的 Retry-After 只属于发出它的那个渠道，不会被下一个渠道继承。模型路由页面按规范模型组展示公开模型名、调用别名与实际渠道成员，并可设置同一渠道内多条映射的次级顺序。每一次故障转移都会写入独立 attempt，并保留上一跳失败原因。熔断中的渠道、正在执行半开探针的渠道，以及达到最大并发的渠道会被自动跳过。

所有推理请求均原样透传。客户端请求 `/v1/chat/completions` 就转发同一路径，不会替换为 `/v1/responses`。上游不支持该原生请求时，只能尝试另一家支持它的渠道；不能靠切换网关策略创造协议兼容性。

默认可重试状态为 401/403/404/408/425/429 和 5xx，以及可重放请求的连接失败或超时。400/422 和 2xx 正文交给客户端解释，不根据错误文本猜测重试。所有候选失败时尽量返回最后一个上游原始 HTTP 错误；从未收到上游响应才使用网关本地错误。错误体与请求体处理受资源和超时限制，不无限缓冲。

**响应提交后不再切换渠道**，中途断流不能拼接另一家内容。客户端取消立即停止后续调用。生成重放不是 exactly-once：上游可能已经计算或计费，即使网关仅收到超时。

网关不会自动启用禁用渠道、增加未经确认的模型支持或将新消费算作零费用。`client_policy` 只检查真实 User-Agent，不伪造客户端身份。

## Docker Compose

```bash
cp .env.example .env
# 编辑 .env 后：
docker compose up -d --build
```

Compose 默认绑定 `127.0.0.1:8787`；请使用 Tailscale/WireGuard 或配置了 TLS 与访问控制的反向代理，而不是直接将后台暴露到公网。

正式发布使用 pull-only 的 `deploy/compose.release.yml`，只引用 `ghcr.io/cupid532/fusiongate:<tag>`，没有 `build:`。根目录 `VERSION` 去掉前缀 `V` 即对应 Git/GHCR tag（例如 `V3.06` 对应 `v3.06`）。tag workflow 成功后按以下方式精确拉取和更新：

```bash
export FUSIONGATE_REF=v<版本>   # 必须替换为真实发布 tag（例如 V3.06 → v3.06），不要使用 latest
docker compose -f deploy/compose.release.yml config --images
docker compose -f deploy/compose.release.yml pull fusiongate
docker compose -f deploy/compose.release.yml up -d --no-build fusiongate
curl -fsS http://127.0.0.1:8787/healthz
```

每次更新必须显式修改 `FUSIONGATE_REF` 并先检查 `config --images`。旧 shell 或 `.env` 中残留的 `FUSIONGATE_REF` 不会自动前进；它会让 `pull` 成功但仍拉取/运行旧 tag。不要用 `latest` 代替可复现的 release tag。

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

常用操作（安装器托管的部署）：

```bash
# 更新到安装时记录的 GitHub ref
sudo /home/myservices/fusiongate/app/deploy/install.sh --update

# 生成带校验和的受保护备份（默认写入 /home/myservices/fusiongate/backups）
sudo /home/myservices/fusiongate/app/deploy/install.sh --backup

# 校验并恢复备份；旧数据安全副本保存在 /home/myservices/fusiongate/pre-restore-<stamp>
sudo /home/myservices/fusiongate/app/deploy/install.sh --restore /home/myservices/fusiongate/backups/fusiongate-<stamp>.tar.gz

# 日常状态与日志
sudo docker compose --project-directory /home/myservices/fusiongate/app --env-file /home/myservices/fusiongate/config/compose.env -f /home/myservices/fusiongate/app/deploy/compose.production.yml ps
```

如果该主机不是由 `deploy/install.sh` 安装的（即没有
`/home/myservices/fusiongate/.fusiongate-install`，Compose 与 Caddy 由你自己维护），
安装器的 `--update` / `--backup` / `--restore` 都不适用。请改用
`deploy/deploy-from-origin.sh` 升级——它只允许部署已经推送到 `origin/main`
的提交，并在部署后校验 `/healthz` 上报的 `version` 与 `revision`，从而保证
服务器与 GitHub 始终一致。两种部署模式的差异与操作方式见
[DEPLOYMENT.md](DEPLOYMENT.md#two-deployment-models)。

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
| `FUSIONGATE_ADMIN_PASSWORD` | 首次运行必填（至少 8 字符），用于初始化管理员密码；已有数据库使用库内密码，页面改密后重启不会被此变量覆盖或阻止。 |
| `FUSIONGATE_MASTER_KEY_FILE` | 可选，读取主密钥的文件路径；生产 Compose 使用该方式挂载 secret。 |
| `FUSIONGATE_ADMIN_PASSWORD_FILE` | 可选，读取首次初始化密码的文件路径；生产 Compose 使用该方式挂载 secret，指定的文件需可读取。 |
| `FUSIONGATE_ADDR` | 监听地址，默认 `127.0.0.1:8787`。 |
| `FUSIONGATE_DATA_DIR` | SQLite 数据目录，默认 `./data`。 |
| `FUSIONGATE_MAX_FAILOVER_ATTEMPTS` | 可选保险丝：单次请求最多尝试的上游渠道数。默认不限（逐个试完请求内全部候选渠道后才返回失败），渠道越多尝试越多；设为 N（N≥1）时恢复固定上限，用于避免失效渠道造成重试风暴。 |
| `FUSIONGATE_MAX_CONCURRENT_REQUESTS` | 网关同时处理的 API 请求上限，默认 `64`；达到上限返回 `503` 并带 `Retry-After`。 |
| `FUSIONGATE_STREAM_START_TIMEOUT` | 流式响应等待上游开始响应的时间（不解析模型事件），未显式设置时继承渠道 `request_timeout_ms`（通常 120 秒）；渠道 `stream_start_timeout_ms` 优先于此环境变量。 |
| `FUSIONGATE_STREAM_IDLE_TIMEOUT` | 流式响应读取字节之间的最大空闲时间（不解析模型事件），默认 `5m`。 |
| `FUSIONGATE_CORS_ORIGINS` | 可选的逗号分隔浏览器 Origin 白名单；留空保持兼容的通配行为。 |
| `FUSIONGATE_PRICING_SYNC_INTERVAL` | 官方价格同步间隔，默认 `1h`，最低 `5m`；低于 `5m` 的值回退为 `1h`，设为 `0`、`off` 或 `false` 可关闭。 |
| `FUSIONGATE_HEALTH_CHECK_INTERVAL` | OAuth 后台模型列表连通性探测间隔，默认 `15m`；设为 `0`、`off` 或 `false` 可关闭后台任务，不影响真实业务请求和手动真实生成检活。 |
| `FUSIONGATE_HEALTH_CHECK_CONCURRENCY` | OAuth 后台连通性探测并发数，默认 `5`，最大 `20`；每个渠道仍由 `health_check_enabled` 单独控制。 |
| `FUSIONGATE_ALLOW_INSECURE_UPSTREAMS` | 仅可信开发环境可设 `true`，允许 HTTP。 |
| `FUSIONGATE_ALLOW_PRIVATE_UPSTREAMS` | 仅可信开发环境可设 `true`，允许私有网络上游。 |
| `FUSIONGATE_SING_BOX_PATH` | 可选，sing-box 可执行文件路径；官方 Docker 镜像已内置固定版本，本机运行仅在启用 IP 池节点时需要安装。 |

网关不改变客户端的 `stream`。上游按客户端请求返回 SSE 或普通响应，网关按原始字节转发；慢生成请合理配置渠道超时和外层反向代理时限。收到心跳只说明连接仍有字节，不代表模型已输出结果。

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

控制台“渠道备份”不是完整数据库恢复：它按渠道名称和上游 Key 指纹合并配置，保留文件之外的 Key、路由及别名，不包含 OAuth 凭据、下游访问密钥、管理员密码或出口节点。v2 保存归档状态、模型策略、白名单、模型清单和排除规则；匹配 Key 的显式空清单会清空库存，旧 v1 缺失字段则保留现有设置（新 Key 使用默认值）。出口节点按名称匹配，不存在时降级并返回警告。导出含上游密钥，请按秘密文件保管。

## 已知范围和后续工作

FusionGate 不包含支付、充值、用户注册、兑换码或商业计费模块。费用为按路由定价的估算，最终以上游账单为准（V3.12–V3.13 期间的请求没有用量记录）。跨协议转换只在渠道不支持客户端协议时使用；任意有状态资源 API、图像编辑、PostgreSQL 与备份 UI 不在本次范围内。

渠道 `stream_start_timeout_ms` / `stream_idle_timeout_ms` 可选，0 表示继承（PATCH 传 0 可重置）。优先级：渠道正值 > 显式环境变量 > 首包继承请求超时、空闲 300 秒。流中任意数据含心跳重置空闲时间，非流式保留请求总超时。

**凭据层边界**：认证文件导入面向迁移——可识别 CLIProxyAPI 与 sub2api 的导出结构（含 sub2api 的 `credentials` 包装），并把这类来源标记为**外部管理**：FusionGate 不轮换它们的 Refresh Token，令牌过期时只提示回源头更新。若这些工具已经在你本机提供了端点，更省事也更保真的做法是把它们的地址注册成普通渠道（`openai_compatible` / `anthropic_compatible`）走原生透传，而不是导入令牌后自行维护刷新——适配新客户端（Kiro、Cursor、Trae 等）不是本项目的目标。
