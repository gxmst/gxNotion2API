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
- `upstream_base_url` / `upstream_origin`
- `proxy_mode` / `proxy_url` / `proxy_http_url` / `proxy_https_url`
- `resin_enabled` / `resin_url` / `resin_platform` / `resin_mode`
- `accounts[*].sticky_proxy_account`
- `accounts` / `active_account`
- `storage.sqlite_path`

可直接参考：

- `config.example.json`
- `config.docker.json`

### 额度与缓存复用

上游按缓存读取计费，复用同一个会话线程比每次新建便宜得多。相关开关：

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `features.force_fresh_thread_per_request` | `false` | 开启后每个请求都新建上游线程并重放全部历史，**缓存完全无法命中**。只在需要彻底隔离每次请求时才开。 |
| `features.conversation_idle_ttl_hours` | `24` | 会话空闲这么久后连同上游线程一起删除。设 `0` 表示永不删除。留空走默认值。 |
| `features.ephemeral_all_conversations` | `false` | 开启后**每一轮**对话结束就删线程，线程创建量翻倍且缓存归零。与省额度的目标冲突，除非你要求不留痕，否则别开。 |
| `features.ephemeral_ttl_seconds` | 未设置 | 仅作用于 `ephemeral_all_conversations` 标记的会话。不设置时该路径用 2 分钟。 |

会话延续按「显式 conversation_id → `previous_response_id` → thread_id → 请求指纹 → 历史段落」逐级匹配，指纹取隐藏提示词加首条用户消息，因此每轮重发完整历史的客户端（如 SillyTavern）能自动命中同一线程。

SillyTavern 的 `quiet` / `impersonate` 属于辅助请求（摘要、世界书触发、代打），一次性使用，会独立按 10 分钟回收，不受上面两个 ephemeral 开关影响。

### 上游指纹

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `features.use_surf_main_transport` | `true` | 主请求路径走 surf/utls 的 Chrome 伪装。关闭后回到 Go 原生 `net/http`，其 TLS 与 HTTP/2 指纹与请求头声明的 Chrome 不一致，上游会以 `sub_type=trust-rule-denied` 拒绝。传输层失败时仍会自动用原生客户端重试一次。 |
| `features.timezone` | `Asia/Shanghai` | 上报给上游的 IANA 时区。 |
| `features.accept_language` | 跟随时区推导 | `Accept-Language` 头。留空时按时区推导匹配值，避免出现「Windows/en-US 浏览器却报 Asia/Shanghai」这类组合。账号 cookie 里的 `NEXT_LOCALE` / `notion_locale` 优先级更高。 |

证书校验默认开启，仅在显式设置 `upstream_tls_server_name` 或 `upstream_host`（域前置场景，证书本就不匹配）时才跳过。

### 模型列表刷新

模型发现只在导入账号时执行，且当粘贴的 probe JSON 字段完整时会被整个跳过。要在不重新导入账号的前提下更新模型列表：

```bash
curl -X POST http://127.0.0.1:8787/admin/accounts/refresh-models \
  -H "X-Admin-Token: <token>" \
  -H "Content-Type: application/json" \
  -d '{"email":"you@example.com"}'
```

省略 `email` 时使用当前活动账号。与导入时相反，这个接口让上游返回的定义**覆盖**配置里的同名条目，因此上游改过代号的模型会被纠正，而不是被旧值挡住。

## 使用建议

- 首次启动后先访问 `/admin`，确认账号、配置和连通性是否正常
- 修改管理台前端后需执行 `npm --prefix ./frontend run build:static`
- 调整会话延续与存储时，建议同步检查 `internal/app/sqlite_store.go` 的 schema 与迁移兼容性

## 来源与关系

本仓库基于 [GALIAIS/Notion2API](https://github.com/GALIAIS/Notion2API)，保留其完整提交历史与 MIT 版权声明。

在上游基础上的主要改动：

- 会话线程复用（关闭默认的每请求新建线程）与空闲回收，降低上游缓存读取开销
- 修复 ephemeral TTL 覆盖外溢导致 SillyTavern 辅助会话寿命被压缩的问题
- 主请求路径改用 surf/utls 浏览器指纹伪装，原生 `net/http` 作为传输层回退
- 新增模型列表刷新接口，无需重新导入账号即可同步上游模型
- 恢复默认的 TLS 证书校验，仅在显式域前置配置下跳过
- 将 `internal/app` 的测试重新纳入版本控制，并在 CI 中执行

## 开源协议

MIT License

原始版权归 GALIAIS 所有（见 `LICENSE`），本仓库的修改同样以 MIT 发布。

## 致谢

本项目已在 [LINUX DO 社区](https://linux.do) 发布，感谢社区的支持与反馈。
