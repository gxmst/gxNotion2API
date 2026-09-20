# 2026-09-20 HAR 核对与适配

来源：用户提供的 `app.notion.com.har`，仅离线解析。本文件不包含 Cookie、令牌、账号、工作区及会话标识；测试使用合成标识。

## 与此前调查的关键差异

- 7 次 `getAvailableModels` 来自同一工作区。6 次普通请求只带 `spaceId`，均返回 `modelSelectionRestricted: true`、`models: []`；1 次额外带 `surface: workspace_model_settings`，返回 `false` 和 36 条模型。差异来自请求用途，不能据此认定工作区恢复了手动选模权限。
- 设置页目录含 `workflow.isDisabled`、`workflow.disabledReason: trial_not_allowed`，模型顶层也可能标记禁用。只检查顶层开关会漏掉工作流限制。
- `modelConfiguration` 提供默认及支持的思考强度。这是目录信息，不能证明调用参数或本轮实际强度。
- 网页仍调用 `syncRecordValuesSpaceInitial`，并成功读回 thread / thread_message。因此旧笔记的“接口失效”不能推广到所有会话：保留此接口，缺失记录时对普通 `syncRecordValues` 作一次有界补读。
- 历史推理记录中可见 `step.model`；部分 thinking 内容同时带 `notionModelName` 和 `modelProvider`，另一些消息完全没有模型信息。配置步骤中的 `model` / `modelFromUser` 不作为实际推理模型证据。
- 设置更新分别使用 `personal_agent_model_policy` 与 `custom_agent_model_policy`，包含 `disabledModels` / `disabledProviders`。两者不可混为一谈。本轮不自动调用 `updateSpaceSettings`。
- 这份 HAR 没有 `runInferenceTranscript` 请求，所以不能用它证明新的流式载荷或思考强度参数。流解析用合成事件测试，并兼容上述已观察到的模型字段；真实在线推理仍需另行验证。

## 本轮实现

1. 模型能力按账号和工作区保存，只有普通聊天响应决定 manual / auto_only；缺失能力保持未知，允许 Auto。
2. “刷新模型能力”准确使用所选工作区，并将设置页目录单独保存。保留本地模型禁用配置。
3. 手动请求按能力筛选工作区；默认拒绝无法满足的指定模型请求，可显式启用 Auto 兼容。登录刷新不覆盖已更新的模型限制。
4. 每轮消息保存上游模型观察值，兼容响应保留原请求模型字段。未知模型代号原样显示，缺失信息显示未知。
5. 聊天与账号测试的选择器跟随工作区能力；目录说明与实际模型信息分别展示。
6. 续聊的基础配置与 `updated-config` 都使用本轮模型选择；Auto 清除历史指定模型。答案回读保留流中已报告的模型证据。

## 验证范围

自动化覆盖空列表与未知能力、设置目录权限隔离、工作区刷新目标、混合账号池与绑定会话、Auto 兼容、本地禁用优先级、模型证据解析和持久化、线程补读与鉴权错误不重试，以及桌面和手机交互。没有重放 HAR 中的真实请求，也没有修改 Notion 上的设置。

最终验证通过：`go test ./...`、`go vet ./...`、前端 `typecheck`、`build:static`，以及 Chrome 下 14 项桌面 / 手机 Playwright 测试。`frontend/out` 与 `static/admin` 的 33 个文件 SHA-256 一致。构建使用常规输出目录，未生成发布归档。

## 提交前追加审查

范围为上一提交 `92a4a44` 及本地未提交改动。以下三项已修复并补充回归测试：

- 管理台回读上游历史会覆盖本地选模信息：完成消息保存上游消息 ID，按 ID 合并请求模型、选择方式和实际模型证据，避免混入其他轮次。
- 查看、删除非默认工作区会话时使用了账号默认工作区：现在根据会话绑定的工作区创建客户端，不存在的工作区明确报错。
- 定时登录态刷新未遵守账号冷却期：现在在读取登录资料或访问上游前检查冷却，不把暂缓刷新记成登录失效。

修复后再次通过 Go 全量测试、`go vet`、前端类型检查及 14 项 Chrome 桌面 / 手机测试。本轮没有进行真实账号推理或部署。
