# ZCode Gateway (Go)

本地多账号 ZCode → Anthropic 兼容 API 网关，带 Web 管理台。

## 功能

- **添加账号**：浏览器打开授权链接，登录完成后自动写入；也支持粘贴 `zcode://oauth/callback?...` 或 CLI 回调 URL。
- **多账号调度池**：每个账号同时只允许 1 个上游请求；新会话随机分配，之后按会话 ID 粘性；瞬时限流只在本次请求内换号（绑定保留），401/403、额度耗尽、账号禁用/删除才解绑。
- **限流冷却**：429/1113 连续第 3 次才冻结 1 分钟，前两次只换号不冻结；账号列表可查看冷却原因并手动「清除冷却」。
- **活动中心**：账号列表点「活动」检测当前 marketing 活动与可领取套餐，弹窗内可直接领取；领取使用账号自己的 ZCode 凭据。
- **账号指纹**：每个账号独立生成一套自洽的桌面 SKU（平台/架构/系统版本/屏幕成套绑定，语言与时区成对），`device_mid` 为全新 UUIDv4 且跨账号不重复。**完全不读取宿主机的任何信息**，程序与数据库搬到别的机器上指纹不变。持久化在 SQLite `account_profiles`，同一账号跨请求、跨重启保持稳定。屏幕只取 Retina / 1440p 及以上的桌面分辨率。
- **单账号并发**：账号列表可为每个账号设置 1–5 个并发，默认 1；调度池按设置动态限流。
- **模型级冷却升级**：按「账号 + 模型」统计连续失败；3 次异常冷却 1 分钟，冷却后再次 3 次且无成功升级为 10 分钟，再 3 次禁用该模型的调度（页面显示），其他模型仍可继续使用；任意成功会重置该模型状态。
- **SQLite 自动迁移**：`account_profiles`、`model_states` 均通过代码内建表/补列自动升级，旧版本数据目录可直接启动。
- **统一上游代理**：启动时设置 `ZCODE_PROXY_URL`（或 `ZCODE_PROXY`）可让 OAuth、额度、活动、领取和消息请求全部走 HTTP/HTTPS/SOCKS5 代理；支持 `http://`、`https://`、`socks5://`、`socks5h://`。
- **额度刷新**：后台按设置间隔（默认 30s）调用 `billing/balance`，页面每 30s 刷新展示；接口失败时回退到本地日志统计。
- **日志落库 SQLite**：每次请求记录账号、会话、输入/缓存读/缓存写/输出/思考 token，以及首 Token、最后流、流时长和生成速度（输出 tokens，已包含思考 / 首末 Token 间隔）；失败记录上游状态与响应；支持详情抽屉和请求/响应预览。
- **面板登录**：未登录只能看到登录页，账号/额度/日志 API 全部 401。
- **账号列表**：额度按模型拆分、今日 tokens 按类型拆分（输入/输出/缓存读/缓存写/思考）、请求次数 / 成功率、最后使用时间（秒/分/小时）。
- **单位换算**：所有 token/额度按 `100` / `1K` / `1M` / `1B` 展示。
- **上游指纹**：
  - TLS/HTTP2：`tls-client` + Chrome 131 profile
  - 完整 ZCode headers：`X-ZCode-*`、`X-Request-Id`、`X-Session-Id`、`X-Query-Id` 等
  - 所有上游调用（OAuth、billing、活动、project token、消息）统一使用该账号的指纹，没有“登录一套、消息另一套”
  - Start Plan system prompt 指纹：自动前置官方 `system[0] + system[1]` 前缀
- **请求兼容**：Start Plan 前置指纹后，把客户端的多段 `system` 合并为一段（多段组合会触发上游 `3012`），并剥离上游不支持的 `context_management`、`thinking.display` 和 tool `strict`；SSE 流式透传。

## 运行

要求：Windows + Go 1.27（依赖 `fhttp` 要求；`GOTOOLCHAIN=auto` 会自动下载）。

```bat
cd zcode-gateway
go build -o zcode-gateway.exe .
start.cmd
```

打开 `http://127.0.0.1:8787`，先登录管理面板。

### 预编译包

Release 页提供六个平台的压缩包（仅含二进制与 README）：

| 平台 | 包名 |
|---|---|
| Windows x64 | `zcode-gateway_<version>_windows_amd64.zip` |
| Windows ARM64 | `zcode-gateway_<version>_windows_arm64.zip` |
| Linux x64 | `zcode-gateway_<version>_linux_amd64.tar.gz` |
| Linux ARM64 | `zcode-gateway_<version>_linux_arm64.tar.gz` |
| macOS Intel | `zcode-gateway_<version>_darwin_amd64.tar.gz` |
| macOS Apple Silicon | `zcode-gateway_<version>_darwin_arm64.tar.gz` |

同目录的 `SHA256SUMS.txt` 用于校验。发布由推 tag 自动触发：`git tag v1.2.3 && git push origin v1.2.3`，工作流依次执行 `go vet`、`go test`、六平台交叉编译，再创建对应版本的 Release。

## 登录

面板默认强制登录：未登录时只能看到登录页，`/api/*` 全部返回 401，看不到任何账号、额度或日志，也无法操作。

- 首次启动会自动生成管理密码并打印到控制台：`admin panel password ...`
- 用环境变量固定密码：`set ZCODE_ADMIN_PASSWORD=your-password`
- 密码以 bcrypt 哈希保存在 `data/accounts.json`，接口不会返回哈希
- 登录会话存 SQLite（`admin_sessions`），Cookie 为 `HttpOnly`，有效期 7 天
- 右上角“退出”会删除会话

首次启动会自动导入 `%USERPROFILE%\.zcode\v2\credentials.json` 里的本机 ZCode 账号；也可以在“账号”页点“导入本机账号”或“添加账号”。

## 客户端

```bat
set ANTHROPIC_BASE_URL=http://127.0.0.1:8787
set ANTHROPIC_API_KEY=local-proxy
```

如果设置了 Web 设置里的“代理 Token”，客户端需带：

```http
Authorization: Bearer <token>
```

或 `x-api-key: <token>`。

支持：

- `POST /v1/messages`（Anthropic Messages，流式/非流式）
- `GET /v1/models`
- `GET /health`

## 配置

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `ZCODE_GATEWAY_ADDR` | `127.0.0.1:8787` | 监听地址，不要改成 `0.0.0.0` |
| `ZCODE_GATEWAY_DATA` | `data` | 数据目录 |
| `ZCODE_GATEWAY_BASE_URL` | `http://<addr>` | UI 显示的 API Base |
| `ZCODE_APP_VERSION` | `3.16.3` | 请求头/签名版本 |
| `ZCODE_MODEL` | 空 | 强制覆盖请求模型（例如 `GLM-5.3-Flash`） |
| `ZCODE_SYSTEM_FINGERPRINT` | 开 | `0` 关闭 Start Plan system 前缀（会 3012） |
| `ZCODE_MERGE_SYSTEM` | 开 | `0` 关闭客户端多段 system 合并（多段组合可能 3012） |
| `ZCODE_UPSTREAM` | 空 | 覆盖上游 messages URL，仅用于联调/抓包 |
| `ZCODE_PASSTHROUGH_BETA` | 空 | `1` 转发客户端 anthropic-beta（默认丢弃） |
| `ZCODE_CAPTCHA_PARAM` | 空 | 手动提供 Aliyun captcha 参数 |
| `ZCODE_CAPTCHA_REGION` | `cn` | captcha region |
| `ZCODE_GATEWAY_LOG` | `1` | `0` 关闭请求日志 |
| `ZCODE_ADMIN_PASSWORD` | 空 | 固定面板登录密码；为空则首次启动生成并打印 |
| `ZCODE_STICKY_TTL_SEC` | `86400` | 会话粘性空闲过期秒数；每次成功请求后自动顺延（滑动 TTL） |
| `ZCODE_STICKY_WAIT_MS` | `3000` | 命中粘性账号时为其排队的最长毫秒数；超时改用空闲账号，但**不改写粘性绑定** |
| `ZCODE_REQUEST_TIMEOUT_SEC` | `600` | 单次上游请求总超时；超时自动释放账号槽位 |
| `ZCODE_MAX_HOLD_SEC` | `600` | 账号槽位最长占用；超时 watchdog 强制释放，防止断链/卡死永久占位 |

Web 设置页可改：代理 Token（支持一键随机生成 `sk-` 并复制）、是否保存请求/响应内容、是否启用 system 指纹、提示词伪装、UA 指纹、额度刷新间隔。

生成 Token 后点“保存设置”生效；之后未带 `Authorization: Bearer sk-...` 或 `x-api-key: sk-...` 的请求返回 401。

## 数据

- `data/master.key`：32 字节主密钥（0600），首次启动生成
- `data/accounts.json`：账号与额度；token 字段用 AES-256-GCM 加密
- `data/gateway.db`：SQLite；`logs` 存请求日志，`session_bindings` 存会话粘性绑定，`admin_sessions` 存面板登录会话
- 旧的 `data/logs.jsonl` 会在首次启动时导入 SQLite，并改名为 `logs.jsonl.imported`

## 多账号调度池

1. 每个账号一个容量为 1 的信号量：同一账号同时只跑一个上游请求；并发请求会排队。
2. 新会话：在健康账号中**随机**选一个（保证第一轮分散）。
3. 后续请求：按会话键优先使用绑定账号；绑定是**偏好**而不是租约：
   - 绑定账号空闲时直接使用；忙时最多等 `ZCODE_STICKY_WAIT_MS`，超时先用其他空闲账号跑完本次请求，绑定不变，账号恢复后会话自然回落；
   - 上游瞬时错误（`429/1113`、`5xx`、`3012`、网络错误）只影响本次请求和模型冷却，不会解绑会话；
   - 只有硬失败（`401/403`、额度耗尽、账号被禁用/删除）才清除该账号的会话绑定；
   - 粘性 TTL 默认 24 小时且滑动：每次成功请求后顺延，活跃会话不会过期。
4. 会话键来源（按顺序）：
   - `X-Session-Id`、`X-ZCode-Session-Id`、`X-OMP-Session-Id`、`X-Claude-Code-Session-Id`、`X-Conversation-Id`、`X-Codex-Session-Id`、`Session-Id`、`Conversation-Id`
   - body 顶层 `session_id` / `sessionId` / `conversation_id` / `conversationId`
   - body `metadata.session_id` / `metadata.sessionId` / `metadata.conversation_id`
   - `metadata.user_id` 里的 `session_<uuid>`（omp/Claude Code 风格）
   - 没有会话键：每个请求随机分配（不粘性）
5. 上游返回以下情况时标记账号/模型冷却并重试其他账号（会话绑定保留，除 `401/403` 等硬失败）：
   - `429` 或 `code=1113`：10 分钟
   - `401/403`：30 分钟（硬失败，清绑定）
   - `405 code=3012`：1 分钟
   - `5xx`：30 秒
6. 其他 `4xx` 直接返回客户端，不切换。
7. **异常释放**：下游断链、客户端取消、上游超时、panic 都会通过 `defer` 释放槽位；另有 watchdog 在 `ZCODE_MAX_HOLD_SEC` 后强制释放，保证不会被一个坏请求永久占住。
8. 下游断开时同时关闭上游连接，避免上游继续生成；`/health` 的 `pool.inFlight` 可观察当前占用数。


## 提示词与 UA 伪装

设置页默认：

- **提示词伪装 = append（默认）**：发送官方 ZCode system 前缀 + 客户端自己的 `system`，保留 omp 的策略；如需最强伪装可切到 `replace`（完全不发送客户端 system）。
- **UA / 身份指纹 = zcode**：`User-Agent: ZCode/3.16.3`、`X-Title: Z Code@electron`、完整 `X-ZCode-*` / `X-Request-Id` / `X-Session-Id` 头；TLS 使用 Chrome 131 profile。
- 客户端 `anthropic-beta` 默认丢弃（官方 ZCode 请求不带它）；需要时设 `ZCODE_PASSTHROUGH_BETA=1`。
- `metadata.user_id` 改写为账号的 ZCode user id。
- 客户端 tools 透传，但会去掉 `strict` 字段；只有 system/身份头被替换。

默认就是 **append**：保留 omp 的 system 策略，但上游能看到它。需要最强伪装时切到 **replace**（客户端 system 完全不发送）。

### 在 omp 中指向本网关

在 `~/.omp/agent/models.yml` 加一个 custom provider（不要覆盖已有 providers）：

```yaml
providers:
  zcode-local:
    baseUrl: http://127.0.0.1:8787
    api: anthropic-messages
    apiKey: local-proxy
    authHeader: true
    models:
      - id: GLM-5.3-Flash
        name: GLM-5.3-Flash (ZCode)
        contextWindow: 1000000
        maxTokens: 128000
```

然后：

```bat
omp -p "..." --model=zcode-local/GLM-5.3-Flash
```

注意：默认 `append` 会保留 omp 的 harness 指令；切到 `replace` 后 omp 的 system 不会发给模型，工具仍可用，但行为会更接近官方 ZCode。

## Start Plan system 指纹

`zcode-plan/anthropic` 要求请求的 `system` 以官方 ZCode 前缀开头，否则返回 `3012`：

```
block[0] = "You are ZCode, an interactive coding agent"
block[1] = 官方 system[1] 前 1213 字符
```

代码在 `fingerprint.go`；`upstream.go` 的 `withSystemFingerprint` 会自动前置，并保留客户端自己的 system 在后。客户端如果本来就是官方 ZCode system，不会重复插入。随后 `mergeClientSystem` 会把客户端的多段 system 合并为一段：分成多段转发时，上游会以 `3012` 拒绝 omp 这类多段系统提示。

关闭开关（会重新 `3012`）：Web 设置里关掉“Start Plan system 指纹”，或设置 `ZCODE_SYSTEM_FINGERPRINT=0`。

## 目录

```
main.go           入口、路由
handlers.go       Web API、代理、重试、日志
store.go          账号/日志持久化、加密、本机导入
oauth.go          OAuth init/poll/callback
quota.go          billing/balance、额度统计
upstream.go       tls-client、ZCode headers、system 指纹/合并、请求字段兼容、usage 解析
zcode.go          Z.ai/BigModel 业务接口、项目 key
fingerprint.go    Start Plan system 指纹
profile.go        每账号设备指纹生成/SKU 池/身份头
web/              HTML/CSS/JS（embed）
```

## 说明

- 这是本地单用户网关；不要绑定公网地址。
- `3012` 的另一半原因是 system 指纹；TLS/headers 只是补充。升级 ZCode 客户端后，如果重新出现 `3012`，从官方 `model-io-*.jsonl` 重新提取 `system[1]` 前缀更新 `fingerprint.go`。
- 账号 token 来自本机 ZCode 登录；Start Plan 的 `zcodejwttoken` 只能通过重新登录刷新。需要刷新时，用官方桌面端退出/登录，或在账号页重新添加账号。