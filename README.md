# X-Loom

X-Loom 是一个使用 Go 构建的协作探索系统，提供 `serve`、`dispatch`、`worker`，配套 Web 工作台与 Docker/Kali Worker。

> 仅支持 Linux。

## 使用

### 1. 克隆项目

```bash
git clone https://github.com/do-whilefor/X-Loom.git
cd X-Loom
```

### 2. 准备配置

```bash
cp dispatch.example.yaml dispatch.yaml
cp .env.example .env
```

编辑 `.env`，至少填写：

```env
ANTHROPIC_AUTH_TOKEN=your_token
```

模型地址与模型名称可通过以下变量调整：

```env
ANTHROPIC_BASE_URL=
ANTHROPIC_DEFAULT_FABLE_MODEL=
```

### 3. 构建 Docker 镜像

必须先构建 X-Loom 主镜像，再构建 Worker 镜像：

```bash
docker build -t xloom:dev .
docker build -f container/Dockerfile -t xloom-worker:dev .
```

### 4. 启动

确认本机 `dispatch.yaml` 中的 `container.image` 与已构建的 Worker 镜像标签一致
（上述命令为 `xloom-worker:dev`）；修改示例文件不会自动更新已有配置。

先检查配置和真实模型接入，再启动服务：

```bash
docker compose config --quiet
docker compose run --rm --no-deps dispatcher dispatch \
  --config /etc/xloom/dispatch.yaml --startup-healthcheck-only
docker compose up -d --no-build --wait server dispatcher
docker compose ps
```

模型检查会发送一次小型真实请求。Web 可访问只说明 Server 已启动；还需要 Dispatcher
持续运行，才能调度项目并动态创建 Worker。

访问：

```text
http://127.0.0.1:8000
```

停止：

```bash
docker compose down
```

默认 `docker compose up -d` 启动 Server 与 Dispatcher；Worker 容器由 Dispatcher 按任务动态创建。

新项目从 Decide（配置名 `reason`）开始：简单任务规划一个有边界的 Step，复杂任务根据当前信息规划少量互补方向，再由 Execute（`explore`）执行。Step 完成表示指定探索结束；有证据支持的阴性观察也能完成 Step，运行失败不能当作观察，项目完成仍需满足用户根目标及范围。最终报告在实质探索结束后规划，用户要求中期报告时除外。

创建项目的 `bootstrap_enabled` 参数已废弃：缺省、`true` 和 `false` 均保存并返回 `false`，原有合法布尔转换输入仍接受，无效类型仍拒绝。原 Web 的初始探索复选框暂时保留，已不能改变新项目启动顺序。历史项目保存的启动策略不变，停止后恢复、reopen 和 restart 继续沿用原策略；旧 bootstrap 执行仍按登记时的身份、预算与结果合同恢复。

示例 Worker 只启用 `reason`、`explore`；配置缺少 `reason` 能力会报错。需要继续处理历史 bootstrap 项目时，将 `bootstrap` 加回 Worker 的 `task_types`，并添加 `tasks.bootstrap: {timeout: 0, conclude_timeout: 60}`。

CTF 模式内置 TSEC flag 提交说明：仅依据证据、高置信时提交，禁止把 `/api/submit` 用于试猜、枚举或爆破；提交成功必须由接口响应明确确认。使用比赛环境提供的 `TSEC_SERVER_HOST` 和 `TSEC_AGENT_TOKEN`。规则随新建执行的提示词保存，升级前已经保存的旧会话提示词不会自动替换。

## 黑板与提交协议

- 新版实时 Decide 使用 `decision.version: 2`：`graph_action` 暂存本轮计划，`preview` 校验，`commit` 一次提交。新增 Goal／Step 可通过 `$key` 在同批次中引用；最多 64 项，任何一项非法则整批不生效。提交后本轮立即结束。
- 提交在同一 SQLite 事务中写入计划、事件与执行成功回执。响应丢失时查询原运行的回执；进程恢复时丢弃未提交草稿，保留原身份、截止时间和恢复额度。版本冲突后必须刷新概览并补读相关内容，再重新规划。服务端仍检查提交时的内容版本。
- Decide 因时间预算、恢复额度耗尽或暂时性基础设施错误而最终失败时，同一轮次、同一输入最多自动创建一个接续执行。原运行身份、输入和预算不变，新运行关联 `previous_run_id`；接续额度由持久化执行记录约束，Dispatcher 重启不会刷新。拒绝、非法输出、配置错误和人工取消不会自动重试；额度耗尽后需要新信息或显式重试。
- 新 Execute 使用 `result_contract_version: 2`。完成 Step 的结果必须引用本 Step 的有效证据事实 `data.fact_id`，或提供 `data.fact: {description, scope, observed_at, evidence}`。运行时绑定 Step、运行身份、证据片段与文件快照，最终事实、Step 收尾和执行回执一起提交。过程观察可以复用，无需重复生成 Fact。
- 事实被反驳或替代后，未启动的依赖 Step 显示 `needs_review`／`invalid_sources`，认领、登记和启动时都会复核。已运行的 Step 保留原输入及独立观察，由 Decide 决定是否放弃；撤销后拒绝迟到结果。
- Finding 默认合并支持；复核已有 Finding 时，可保持 `claim`／`scope`，使用 `replace_support:true`、非空 `reason` 和有效 `sources`，显式以本次 `sources`／`evidence` 替换当前支持。原事实、证据快照和事件历史保留。完全相同的 Finding 支持／状态／理由或事实纠正关系，即使使用不同请求 key，也返回 `unchanged:true`，不新增业务事件或推进版本。
- 根目标来自用户输入。Decide 可在同一批次中说明理由、撤回辅助计划并完成项目；仍须引用有效观察，不能靠空队列或耗尽预算自动完成。结构校验保证状态一致性，证据是否充分覆盖用户要求仍需模型或用户判断。
- 每轮 Decide 从当前 FGS、原始根条件、有限全局概览和修订变化索引构造干净上下文，不读取上轮 Job 或会话作为记忆。成功运行仅确认其原始输入修订，运行期间出现的新信息仍可触发下一轮；变化索引缺失或过大时退回当前图的有限概览。
- `read_graph` 在服务端按实际响应字节数分页，使用返回的 `next_offset` 继续读取。单条 Fact／Finding 的支持数组过大时返回 `evidence_omitted`／`sources_omitted` 和数量；使用 `section:evidence` 或 `section:sources`、`ids:[该记录ID]` 分页补读。单条正文、证据项或概览仍过大时返回 `record_omitted`；沿用 section／ids，设置 `offset:record_offset`、`byte_offset:0`，带回 `state_version`（作为 `expected_version`）和 `record_version`，按 `next_byte_offset` 拼接 `content` 得到完整 JSON。版本变化会拒绝续读。证据字节不截断，省略不表示不存在。超大写入的成功回执保留身份、版本及 `result_omitted`，完整实体通过图接口读取。
- Dispatcher 使用有界调度与执行摘要、当前输入的定向查询和按 ID 的身份查询；当前 FGS 的 Step 状态仅查询各 Step 最新执行的状态和必要失败字段。日常调度不下载完整 State 或历史 Job。新运行由 Server 在同一事务中捕获当前 FGS、构造有界初始视图、保存不可变输入快照并登记执行；Job 只携带快照引用、初始视图和运行配置，不再携带完整图。相同准备请求返回原登记，同一 run 改变模板返回 409。
- `read_snapshot` 分页读取本运行的原始输入，`read_graph` 分页读取当前 FGS。恢复、初始视图和 shadow replan 仍绑定原输入；当前图的版本刷新不能替代旧快照，也不能把旧结论自动视为仍然有效。快照缺失或摘要校验失败时明确失败，不回退当前图。新 Decide 仍从干净上下文启动，不把上一轮快照作为跨运行记忆。
- 快照目前仍在 SQLite 中保存完整序列化 State，只复用项目内字节完全相同的快照。它不是按 revision 重建历史，也未实现节点结构共享或增量存储；换 run 后租约等字段不同，仍可能生成不同快照。Server 内部准备、调度与图读取仍会加载 State，本次改动解决跨进程传输和 Job 重复携带整图，不代表服务端内存开销已与图大小无关。
- 初始 FGS 视图的 32 KiB 字节预算也用于写入准入，并预留 1 KiB 运行元数据空间。创建项目、修改标题、追加 Hint、改变 Goal／Step 及兼容计划入口，会检查 Decide 和未完成、未弃用 Step 的必需输入；超限返回带 `input_context_limit` 的 422 并回滚本次变更。原始输入、全部 Hint、相关目标祖先和当前 Step 不会被静默截断或丢弃。此限制不同于模型完整上下文的 token／字节预算；既有超限数据不会被自动删减。
- Decide 的读取、草稿、预览、提交、冲突及模型耗时等指标独立附加到成功执行记录，不重新写入业务结果。提交后给予 Worker 最多 10 秒回传收尾指标，人工停止和重启仍立即取消；进程故障可能留下缺失的观测，缺失不代表零调用。Mock 验证协议和故障行为，不代表已证明真实模型的规划质量或成本改善。

运行与输入接口：

| 接口 | 边界与用途 |
| --- | --- |
| `GET /projects/{pid}/scheduling?offset=0` | 调度元数据，每页最多 100 个 Intent／Step 身份与状态；有 `next_offset` 时继续并带回 `expected_version=state_version`，版本变化返回 409。无 Fact 正文或证据。 |
| `POST /projects/{pid}/executions/prepare` | 携带执行身份、租约和小 Job 模板，不传 State、完整图或客户端快照。Server 原子捕获、冻结并登记；首次返回 201，相同请求重放返回 200。 |
| `POST /projects/{pid}/executions/{rid}/input/read` | 携带该运行的执行条件头及 `op:read_snapshot`；支持与 `read_graph` 相同的节点、证据数组和 JSON 字节续读，始终读取原快照。 |
| `POST /projects/{pid}/state/read` | `op:read_graph`，读取当前 FGS 的有界页；Worker 桥接不先下载完整 State。 |
| `GET /executions/pending?namespace=...&after=...&limit=100` | 有界待处理执行摘要，按 `next_cursor` 继续；`/projects/{pid}/executions/check` 定向查询运行与重试条件。 |
| `GET /projects/{pid}/executions/{rid}?namespace=...` | 仅在恢复／审计单次运行时读取完整登记；末尾加 `/identity` 只返回身份摘要。 |
| `GET /projects/{pid}/state/changes?after=...&through=...` | 最多 1000 条变化索引，按最后一项 revision 继续；该 revision 不是可直接恢复历史 State 的快照地址。 |

旧 `/executions?namespace=...` 完整历史接口保留，Dispatcher 不再调用。工作台改用 `/projects/{pid}/executions?limit=20&cursor=0&through=0`：返回 `items/through/next_cursor`，单页最多 1 MiB、条数上限 100；保持 `through` 并沿游标取完历史。查询只读取执行元数据和有界公开结果字段，不读取 Job。无分页参数仍返回旧数组。时间线导出 `/export?format=timeline` 读取当前轮 FGS 事件与状态，包括过程 Fact、Finding、目标／步骤变化、事实纠正和证据；旧 YAML 导出保持兼容。

`POST /projects/{pid}/restart` 先在同一事务中归档旧轮 State、事件、幂等动作和执行登记，再清空当前投影并递增 generation；归档失败则整个重启回滚。旧快照、证据目录、运行撤销记录和节点计数保留，新轮沿用原始输入、目标与 Hint。归档不会授予旧 Worker 恢复或写入权限，也不会自动成为新 Decide 的记忆。历史归档只读；显式删除项目仍会级联删除其数据库记录。

归档查询：`GET /projects/{pid}/rounds?cursor=-1&limit=20` 返回轮次；`/rounds/{generation}/entries?cursor=0&limit=50` 返回条目身份、字节数和 SHA-256；两种列表均沿 `next_cursor` 继续，条数上限 100。`/rounds/{generation}/entries/{entry}?offset=0&limit=32768` 中的 `entry` 使用列表返回的 `cursor`，返回 `encoding:base64` 的字节块、`next_offset/total_bytes/sha256`。每块源字节最多 32 KiB，解码后拼接并校验摘要，勿将单块当作完整 UTF-8 或 JSON。归档入口拒绝携带 `X-Xloom-Run` 的请求；它是管理视图，不是当前 Worker 的图输入。

已登记的旧版任务继续按原内联 Job、原会话摘要和原结果合同恢复，不将当前 State 补入旧会话。数据库升级只补充调度元数据，不重写旧输入。旧 HTTP 接口保留兼容映射，并为业务写入生成事件；外部调用不能通过旧执行登记入口注入快照或将缺失引用的新 Job 降级为旧输入。心跳、租约活动和无变化重放不增加业务事件。新版运行不能通过旧收尾接口降级为无证据结论。工作台视觉保持不变。

升级前应备份 SQLite 数据库和运行／证据目录。新版增加的状态和执行协议不保证旧二进制能够处理；回退代码时应同时恢复对应备份，不能仅凭 `git revert` 推断数据兼容。

## 测试与 CI

GitHub Actions 会在推送代码、提交 PR 或手动触发时，在新的 Linux 环境中检出对应提交，执行单元测试、Mock 集成测试、竞态检查、`go vet` 和程序编译。

下载源码后，在项目根目录运行同一套检查（需要 Docker）：

```bash
docker build --target build --progress=plain .
```

无需创建 `.env` 或 `dispatch.yaml`，也无需启动项目、准备 Worker 镜像或提供模型密钥。检查使用已提交的测试及代码内合成的数据；模型响应由 Mock 提供。拉取基础镜像、安装工具和下载 Go 依赖需要联网，之后测试、静态检查及编译阶段禁用外网。CI 不调用真实模型，不发布镜像，也不部署服务。

GitHub CI 使用根目录 Dockerfile 指定的 Go 1.26，在 Linux amd64 上检查；本地命令默认使用 Docker 主机架构。这不代表已验证 Go 1.23 的最低版本兼容性或其他架构。本地专用的真实模型、Docker 联调及参考源码验收脚本不属于这套 CI。

## 原文保真

需要逐字复制文件时，`write` 使用 `source_path`，由 Go 直接复制并返回字节数和 SHA-256；可用 `source_sha256` 核对原文件，或用成对的 `source_start_line` / `source_end_line` 选择行范围。普通 `content` 写入标记为模型生成，不能据此声称与原文一致。

新生成的上下文摘要分为模型笔记与原文引用。模型只选择来源和行范围，Go 提取工具返回的原文字节并保存来源、偏移和哈希；转义、换行和 Unicode 不由模型重抄。引用保证与当时工具返回一致，不证明内容或解释真实；非法引用和超预算摘要不会替换原会话。完整记录保留在执行目录的 `events.jsonl`，旧摘要中已失真的内容仍需回查原件。

## 使用限制

- 仅在你拥有或已获得明确授权的系统、网络和目标上使用本项目。
- 禁止将本项目用于商业目的；商业授权需另行取得项目作者许可。
- 非商业学习、研究、实验、修改与分发须遵守 `LICENSE` 中的完整许可条款。
- Dispatcher 需要访问 Docker Socket，请仅在可信 Linux 主机上运行。

## License

X-Loom 使用 **PolyForm Noncommercial License 1.0.0**。

允许在许可条款范围内进行非商业使用，包括个人学习、研究、实验、修改以及非商业分发。

**Commercial use is not permitted under this license.**

如需将 X-Loom 用于商业产品、商业服务、SaaS、收费安全服务或其他商业场景，请先取得项目作者的单独授权。

完整条款见 [`LICENSE`](./LICENSE)。

> PolyForm Noncommercial 1.0.0 是带有非商业限制的 source-available 许可证，不是 OSI 批准的开源许可证。
