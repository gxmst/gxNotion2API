# Notion2API

一个基于 Go 的 Notion AI OpenAI 兼容桥接服务，提供标准 API、WebUI 管理面、多账号池和本地 SQLite 持久化，方便本地部署、调试和统一接入。

## 功能概览

- OpenAI 兼容接口：`/v1/models`、`/v1/chat/completions`、`/v1/responses`
- 支持流式响应
- 支持多账号池、账号切换、登录态刷新
- 支持图片、PDF、CSV 等附件请求
- 自带 WebUI 管理面：`/admin`
- 使用 SQLite 持久化账号、会话和运行状态

## 快速开始

### 本地运行

```bash
go run ./cmd/notion2api --config ./config.example.json
```

### 本地构建

```bash
go build ./cmd/notion2api
```

## Docker 部署

先按实际环境修改 `config.docker.json`，再启动：

```bash
docker compose up -d --build
```

如果使用偏生产配置：

```bash
docker compose -f docker-compose.prod.yml up -d --build
```

本地从源码开发需 Go `1.25.0+`（`go.mod` 已声明）。

## 默认入口

- API：`http://127.0.0.1:8787/v1/*`
- Health：`http://127.0.0.1:8787/healthz`
- WebUI：`http://127.0.0.1:8787/admin`

## 代理与 Resin 粘性代理

### 代理模式

`proxy_mode` 支持：

- `off`：关闭代理
- `env`：从环境变量读取（优先 `N2A_*`）
- `http`：固定 HTTP 代理
- `https`：按协议拆分 HTTP/HTTPS 代理
- `socks5`：SOCKS5/SOCKS5H 代理
- `resin_forward`：Resin 粘性代理转发

### 环境变量优先级（`proxy_mode=env`）

HTTPS 请求优先顺序：

1. `N2A_PROXY_HTTPS_URL`
2. `N2A_UPSTREAM_PROXY_HTTPS_URL`
3. `N2A_PROXY_URL`
4. `N2A_UPSTREAM_PROXY_URL`
5. `HTTPS_PROXY` / `https_proxy`
6. `ALL_PROXY` / `all_proxy`

HTTP 请求优先顺序：

1. `N2A_PROXY_HTTP_URL`
2. `N2A_UPSTREAM_PROXY_HTTP_URL`
3. `N2A_PROXY_URL`
4. `N2A_UPSTREAM_PROXY_URL`
5. `HTTP_PROXY` / `http_proxy`
6. `ALL_PROXY` / `all_proxy`

也可以直接用环境变量覆盖配置文件中的代理字段：

- `N2A_PROXY_MODE`
- `N2A_PROXY_URL`
- `N2A_PROXY_HTTP_URL`
- `N2A_PROXY_HTTPS_URL`
- `N2A_RESIN_ENABLED`
- `N2A_RESIN_URL`
- `N2A_RESIN_PLATFORM`
- `N2A_RESIN_MODE`

### Resin 粘性代理（按账号隔离）

每个账号都可以独立设置粘性身份：

- `accounts[].sticky_proxy_account`：显式设置粘性账号名（推荐）
- 未设置时会回退到邮箱派生值

当启用 `resin_forward` 时：

- 代理认证用户名格式：`<resin_platform>.<sticky_proxy_account>`
- 密码使用 `resin_url` 中 token
- 请求会附带 `X-Resin-Account` 头

## 配置说明

建议优先检查这些字段：

- `api_key`：OpenAI 兼容接口密钥
- `admin.password`：WebUI 登录密码
- `admin.trusted_proxies`：仅在反代场景需要，见下方「把管理台放到公网」
- `upstream_base_url` / `upstream_origin`
- `proxy_mode` / `proxy_url` / `proxy_http_url` / `proxy_https_url`
- `resin_enabled` / `resin_url` / `resin_platform` / `resin_mode`
- `accounts[*].sticky_proxy_account`
- `accounts` / `active_account`
- `accounts[*].workspaces` / `accounts[*].default_workspace_id`
- `active_workspace_id`
- `storage.sqlite_path`

可直接参考：

- `config.example.json`
- `config.docker.json`

### 额度与缓存复用

同一上游线程会保留上下文，官方也建议在同一线程中继续对话。复用线程能减少历史重放，但具体扣额度、缓存命中和模型计费以 Notion 当前规则为准，本项目不会据此推算剩余额度。相关开关：

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `features.force_fresh_thread_per_request` | `false` | 开启后每个请求都新建上游线程并重放全部历史。需要隔离每次请求时再开启。 |
| `features.conversation_idle_ttl_hours` | `0` | 默认长期保留普通会话及上游线程。显式设为正数后才按空闲小时数删除；已有配置中的正数仍然生效。 |
| `features.ephemeral_all_conversations` | `false` | 开启后每轮结束清理线程，下次需重新创建并处理上下文。需要临时会话时再开启。 |
| `responses.store_ttl_seconds` | `3600` | 响应正文的保留时间；正文过期后 GET 返回 404，`previous_response_id` 仍可通过保留的关联继续现有会话。 |
| `features.ephemeral_ttl_seconds` | 未设置 | 仅作用于 `ephemeral_all_conversations` 标记的会话。不设置时该路径用 2 分钟。 |
| `features.continuation_failover` | `true` | 绑定账号的上游 AI 额度耗尽后，将对话历史重放到备用账号的新线程；没有可用备用账号时保留原始额度错误。 |

会话延续按「显式 conversation_id → `previous_response_id` → thread_id → 请求指纹 → 历史段落」逐级匹配。隐式匹配按客户端、接口、模型、指定账号和指定工作区划分范围；指纹还包含隐藏提示词及开头的用户、助手消息，因此每轮重发完整历史的客户端（如 SillyTavern）能自动命中同一线程。改选 `workspace_id` 后，隐式匹配会在新工作区开始独立会话；显式指定属于其他工作区的会话 ID 仍会报错。升级前未记录工作区范围的会话，可携带原 `conversation_id` 继续。

客户端身份优先取 `X-Client-ID`，其次是 `X-Session-ID`、`OpenAI-Organization`；缺省使用连接 IP 和 User-Agent。同一反代或 NAT 下、使用相同 User-Agent 的不同客户端应设置不同的身份头，或显式使用独立的 `conversation_id`。提供稳定身份头后，IP 或 User-Agent 改变仍可通过历史匹配续写；旧记录缺少匹配范围时，可用显式会话 ID 继续。

完全重复的最后一轮可回放已有答案，SillyTavern 的 `continue` 始终生成新内容。附件回放需要匹配持久化的内联内容 SHA-256；旧记录没有摘要、附件使用 URL 或本地路径时，会重新执行请求，避免同名文件或同一地址的内容变化后返回旧答案。

SillyTavern 的 `quiet` / `impersonate` 属于辅助请求（摘要、世界书触发、代打），一次性使用，会独立按 10 分钟回收，不受上面两个 ephemeral 开关影响。

续聊关联与会话一同删除，不能复活已删除的线程。启用默认 SQLite 持久化时，关联在重启后仍可恢复；关闭会话或响应持久化时，相应关联不会跨重启保留。`/metrics` 和 `/debug/vars` 提供 `notion2api_inference_activity_total`，区分推理尝试、增量续聊、新线程请求和历史重放次数；这些计数不是实际扣费 token。

默认不因普通拒答自动增加推理请求。保留的兼容策略配置 `prompt.max_refusal_retries` 默认为 `0`，最多允许 `1`；当前推理主链不调用该拒答重试器。

### 上游指纹

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `features.use_surf_main_transport` | `true` | 主请求路径走 surf/utls 的 Chrome 伪装。关闭后回到 Go 原生 `net/http`，其 TLS 与 HTTP/2 指纹与请求头声明的 Chrome 不一致，上游可能以 `sub_type=trust-rule-denied` 拒绝。 |
| `features.allow_native_transport_fallback` | `false` | surf/utls 构造或请求失败时是否允许回退到 Go 原生传输。默认关闭以避免伪装请求意外改变指纹；只在明确接受该取舍时开启。 |
| `features.timezone` | `Asia/Shanghai` | 上报给上游的 IANA 时区。 |
| `features.accept_language` | 跟随时区推导 | `Accept-Language` 头。留空时按时区推导匹配值，避免出现「Windows/en-US 浏览器却报 Asia/Shanghai」这类组合。账号 cookie 里的 `NEXT_LOCALE` / `notion_locale` 优先级更高。 |

证书校验默认开启，仅在显式设置 `upstream_tls_server_name` 或 `upstream_host`（域前置场景，证书本就不匹配）时才跳过。

遇到 `trust-rule-denied` 后不会切换 HTTP 客户端重发或通过登录刷新重试，同一账号的全部工作区暂停 30 分钟。HTTP 429 会保留并遵守 `Retry-After`，账号至少暂停 2 分钟；上游要求更长时间时以其为准。已确认的 `quota-exhausted` 使用 6 小时本地退避，这不是对上游额度重置时间的预测。不同工作区共享 `dispatch.account_max_concurrency`（默认 `1`），同时仍受各工作区的并发上限约束。

### 模型列表刷新

模型发现只在导入账号时执行，且当粘贴的 probe JSON 字段完整时会被整个跳过。要在不重新导入账号的前提下更新模型列表：

```bash
curl -X POST http://127.0.0.1:8787/admin/accounts/refresh-models \
  -H "X-Admin-Token: <token>" \
  -H "Content-Type: application/json" \
  -d '{"email":"you@example.com"}'
```

省略 `email` 时使用当前活动账号。与导入时相反，这个接口让上游返回的定义**覆盖**配置里的同名条目，因此上游改过代号的模型会被纠正，而不是被旧值挡住。

### 多账号与多工作区

账号是凭证、登录态和共享并发的边界，工作区分别保存额度和运行状态。一个账号可以保存多个工作区，多个账号也可以共同指向同一个工作区。调度仅接受 Business、具备 Business 权益的商业试用和 Enterprise，排除 Free、Plus 和明确关闭 AI 的工作区。仅有 `trial`、`team`、`personal` 或 AI 开关不能证明套餐符合要求；套餐未确认时先刷新元数据。旧配置里的 `space_id` / `space_view_id` 仍会迁移成一个 workspace。

`workspace_id` 是新的请求字段，`space_id` 仍作为兼容别名。请求级选择支持 JSON 字段、`X-Workspace-ID`、`X-Notion-Workspace-ID`、`X-Notion-Space-ID`；未指定时优先使用活动工作区，再按账号默认工作区选择，首选目标不可用时仍保留账号池的回退能力。显式指定工作区时只在该区内调度；已有会话继续绑定原工作区。工作区选择不会改变账号的 cookie、probe、浏览器 profile 或代理身份。

管理台的账号详情可以查看全部工作区，并分别设置本地每小时额度、最大并发、默认工作区；激活和快速测试也会携带所选工作区。AI 额度查询按“账号 + 工作区”返回，不会把同一账号下的多个区合并成一个数。

账号详情的“刷新工作区套餐”通过 `/admin/accounts/refresh-workspaces` 更新套餐元数据，不发起推理。它保留凭证、代理与运行计数，并撤销已移除工作区的旧准入。聊天页只列出符合要求的工作区；新对话可以切换，已有对话保持绑定。左侧历史可搜索和恢复，切换到管理页面会保留当前生成和草稿，查看旧消息时不会被流式输出强制拉到底部。

### 工作区选择与旧配置迁移

`space_id` 决定推理落在哪个工作区。旧版本可能将请求发往 probe 记录的免费个人区，其试用额度耗尽后曾出现以下表现；当前调度会事先排除这些工作区：

- 认证、模型列表、账号状态全部正常
- 推理请求上游返回 200，消息也确实写进了 thread
- 但永远等不到 AI 回复，最后超时报 `thread <id> did not produce any agent-inference message`

没有 `quota-exhausted` 之类的明确错误，所以很容易误判成 cookie 失效或 IP 被风控。先确认账号有哪些区、各自什么档：

```bash
curl -X POST https://www.notion.so/api/v3/getSpaces \
  -H "cookie: <你的 cookie>" -H "content-type: application/json" -d '{}' \
  | jq -r 'to_entries[].value.space | to_entries[]
           | "\(.key)  \(.value.value.name)  plan=\(.value.value.plan_type) tier=\(.value.value.subscription_tier)"'
```

选择具有 Business／Enterprise 权益的工作区，商业试用也可；不要选择 Plus。导入时显式带上它的 `space_id` 和 `space_view_id`（显式字段优先于 probe 里记的值），导入后在账号详情刷新套餐以确认准入：

```bash
curl -X POST http://127.0.0.1:8787/admin/accounts/manual \
  -H "X-Admin-Token: <token>" -H "Content-Type: application/json" \
  -d '{"probe_json_text":"<probe json>","space_id":"<付费区>","space_view_id":"<对应 space_view>"}'
```

同一个邮箱再次导入另一个工作区时，账号的登录文件会复用，新的 workspace 会合并进原账号，不会覆盖原来的 workspace。需要将导入的 workspace 设为默认时，可在管理台点击“设为默认工作区”，或调用：

```bash
curl -X PUT http://127.0.0.1:8787/admin/accounts \
  -H "X-Admin-Token: <token>" -H "Content-Type: application/json" \
  -d '{"email":"you@example.com","workspace_id":"<付费区>","default_workspace_id":"<付费区>","hourly_quota":0,"max_concurrency":2}'
```

切区之后要清掉已存的会话记录：里面的 thread 属于旧区，复用它们会稳定失败。

选定的工作区会被保留：会话刷新、启动恢复和 SQLite 持久化都会使用活动 workspace，不会把推理悄悄挪回免费区。上游 `getSpacesInitial` 只返回 `space_view_pointers` 的顺序（不含 plan/tier 字段），而多区账号的第一个指针通常就是免费个人区，所以早期版本每次刷新都会把推理挪回免费区——症状和上面完全一样，但发生在已经切好区之后。首次导入走 `loadUserContent` 时按 `subscription_tier` 优选付费区；注意免费个人区的 `plan_type` 是 `personal` 而不是 `free`，只看 `plan_type` 区分不出来。

### 把管理台放到公网（反代）

默认只监听回环，管理台靠 SSH 端口转发访问。要长期免隧道访问就得反代进来，此时**必须**设 `admin.trusted_proxies`：

```json
{
  "admin": {
    "enabled": true,
    "password": "<强密码>",
    "token_ttl_hours": 24,
    "trusted_proxies": ["127.0.0.1"]
  }
}
```

登录锁定（15 分钟内失败 5 次）是管理密码唯一的暴力破解防线，而它按客户端 IP 计数。`X-Forwarded-For` 是客户端可控的，所以：

- **不设** `trusted_proxies`：一律用连接对端地址。直接暴露监听时这是正确答案；反代场景下所有人会挤在反代的那个 IP 上，一个人被锁全体被锁——失败方向是保守的
- **设了**：只有来自这些地址的请求才认转发头，且取 `X-Forwarded-For` **最右**一段（左边的是调用方自己塞的，可伪造）

反代那边要传 `X-Forwarded-Proto: https`，否则 Go 侧看到的是 HTTP，会话 cookie 不会带 `Secure`。

只反代 `/admin`，别把整个端口代出去——同一个监听上还有 `/v1/*`，代出去等于把推理接口也放到公网，API key 一旦泄露就是拿你的 Notion 额度。

## 使用建议

- 首次启动后先访问 `/admin`，确认账号、配置和连通性是否正常
- 修改管理台前端后需执行 `npm --prefix ./frontend run build:static`
- 调整会话延续与存储时，建议同步检查 `internal/app/sqlite_store.go` 的 schema 与迁移兼容性

### 附件来源限制

附件支持内联 Base64 / data URL，或不含账号密码的公网 HTTP(S) URL，每个附件最大 20 MiB。服务端本地路径（包括 `path`、`file://`、相对路径和网络共享路径）会被拒绝；需要发送本地文件时，请由客户端读取并转成内联数据，管理台的文件选择上传仍可正常使用。

URL 下载会检查全部 DNS 解析结果及每次重定向，拒绝回环、私网、链路本地和其他非公网地址，并直接连接已校验的 IP。下载沿用账号的代理设置，不携带 Notion Cookie，始终校验证书；HTTP/HTTPS 代理必须支持向目标 IP 发起 CONNECT（HTTP 附件也使用隧道）。下载使用独立的 Go HTTP 传输，不受 Notion 主请求指纹和域前置设置影响。

## 来源与关系

本仓库基于 [GALIAIS/Notion2API](https://github.com/GALIAIS/Notion2API)，保留其完整提交历史与 MIT 版权声明。

在上游基础上的主要改动：

- 会话线程复用（关闭默认的每请求新建线程）与空闲回收，降低上游缓存读取开销
- 修复 ephemeral TTL 覆盖外溢导致 SillyTavern 辅助会话寿命被压缩的问题
- 主请求路径改用 surf/utls 浏览器指纹伪装，原生 `net/http` 作为传输层回退
- 新增模型列表刷新接口，无需重新导入账号即可同步上游模型
- 恢复默认的 TLS 证书校验，仅在显式域前置配置下跳过
- 将 `internal/app` 的测试重新纳入版本控制，并在 CI 中执行
- 修复会话刷新把账号挪回免费工作区、以及成功后清不掉失败计数两处状态回退

## 开源协议

MIT License

原始版权归 GALIAIS 所有，本仓库的修改版权归 gxmst 所有，两者都以 MIT 发布（见 `LICENSE`）。

## 致谢

本项目已在 [LINUX DO 社区](https://linux.do) 发布，感谢社区的支持与反馈。
