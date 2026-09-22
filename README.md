# X-Loom

使用 Go 重写 Cairn 的协作探索系统。一个二进制提供 `serve`、`dispatch`、`worker`；保留 Cairn 黑板图与三类执行任务，接入 X-Loom 三栏 Web 工作台，使用自研的 Pi 风格两层 Agent Loop。

仅支持 Linux。

系统已接入会话、上下文压缩与 FGS 状态扩展。Web 仅保留 X-Loom 工作台，展示真实项目、FGS 状态、事件与执行摘要；原 Cairn 经典管理页及其专用资源已移除。

## 目录

```text
cmd/xloom           三个子命令的入口与组装
internal/board      图、SQLite、事务与兼容迁移
internal/server     HTTP API 与导出
internal/dispatcher 调度、租约、心跳和结果写回
internal/docker     Docker Engine HTTP 客户端
internal/contract   混合输出 JSON 提取与任务契约
internal/config     YAML 配置和环境变量校验
internal/worker     执行、会话、预算与收尾；prompts/ 为任务模板
internal/agent      两层循环、消息、事件、工具接口与上下文压缩
internal/provider   Anthropic 兼容请求和流式协议
internal/tools      read/bash/edit/write/grep/find/ls
internal/cvss       CVSS 3.1 Base 向量校验与确定性评分
internal/process    Linux 执行取消与子进程清理
container           Kali Worker 镜像、环境说明和镜像检查脚本
web/static          X-Loom 工作台与本地静态资源
```

`process` 供 Worker 生命周期和 shell 工具共用，避免重复实现 Linux 进程取消。

## 构建

在带有 Go 1.23+、C 编译器、bash、ripgrep 的 Linux 环境中：

```sh
go vet ./...
go build -o ./bin/xloom ./cmd/xloom
```

Web 使用原生 HTML/CSS/JavaScript，无 npm 安装或前端构建步骤。静态资源随 Go 二进制嵌入，更新页面后需要重新构建和启动 Server。

公开源码不附测试文件。项目 Dockerfile 执行包编译检查与 `go vet` 后产出运行镜像；本地包含测试文件时，其 `go test -race ./...` 步骤也会执行这些测试。Worker 镜像以**仓库根**为构建上下文，且末层从 `xloom:dev` 复制 Go 二进制，因此必须先构建控制镜像：

```sh
docker build -t xloom:dev .
docker build -f container/Dockerfile -t xloom-worker:dev .
```

现有验收覆盖合成本地任务，不代表已验证 1M 上下文容量、920,000 token 触发或压缩后 250,000 token 的实际效果，也不能据此判断实际探索效果或普遍成本收益。

## 启动

```sh
cp dispatch.example.yaml dispatch.yaml
cp .env.example .env
# 在 .env 中填写 ANTHROPIC_AUTH_TOKEN
docker compose up -d
```

模型配置统一放在项目根目录 `.env`：`ANTHROPIC_AUTH_TOKEN`、`ANTHROPIC_BASE_URL`、`ANTHROPIC_DEFAULT_FABLE_MODEL`。模板使用 StepFun 的 `https://api.stepfun.com/step_plan` 与 `step-5-preview`；`.env.example` 不包含真实密钥。Compose 将三项传入 Dispatcher，再由 `dispatch.yaml` 的 `common_env` 引用并传给 Worker。已有配置若仍硬编码地址或模型，应先按新模板把这两项改成环境变量引用，之后只需编辑根 `.env`。

修改 `.env` 后，使用与原启动一致的 Compose 项目及 `-f` 参数重建 Dispatcher，让新环境生效：

```sh
docker compose up -d --no-deps --force-recreate dispatcher
```

`docker compose restart dispatcher` 不会重新注入 `.env`。模型修改不需要重建镜像；新设置用于后续 Worker 执行，不会把正在运行的模型请求热切换到另一模型。具名 Worker 的 `env` 仍优先于 `common_env`；若配置了显式 `ANTHROPIC_MODEL`，它优先于 `ANTHROPIC_DEFAULT_FABLE_MODEL`，需要随模型切换一起检查。

Compose 插值时，启动终端里已导出的同名环境变量优先于根 `.env`。若编辑后仍使用旧设置，先清除终端里这三项旧导出，再重建 Dispatcher；排查时无需打印密钥值。Linux shell 可执行 `unset ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL ANTHROPIC_DEFAULT_FABLE_MODEL` 后再运行上述 Compose 命令。

使用前按上述顺序构建两张镜像。访问 <http://localhost:8000>。Server 的数据库在 `xloom-data` 卷中，项目 Worker 容器由 Dispatcher 动态创建。Server/Dispatcher 使用 Debian 上的 `xloom:dev`，Worker 使用 Kali 上的 `xloom-worker:dev`。Worker 镜像内置 Kali 工具集与常见 PoC、知识库，以及 Playwright 无头浏览器，具体内容见 `container/environment.md`。

Compose 的 `worker-image` 服务仅用于显式构建：先 `docker compose build server`，再 `docker compose --profile images build worker-image`。默认 `docker compose up -d` 只启动 Server 和 Dispatcher。

不使用 Compose 时，在 Linux 上可直接运行：

```sh
./bin/xloom serve --host 127.0.0.1 --port 8000 --db-path ./data/xloom.db
# 在另一个终端中导出本机可信的 .env，再启动 Dispatcher。
set -a
. ./.env
set +a
./bin/xloom dispatch --config ./dispatch.yaml
```

此时把配置的 `server` 改为实际 Server URL；Go 程序不会自动读取 `.env`，重新配置后需要重新导出并启动 Dispatcher。Dispatcher 仍通过 Docker socket 管理项目容器。`worker --job <job.json>` 由 Dispatcher 调用；`worker --cancel <run-directory>` 只取消该次执行。

## 配置与行为

- Web 的三种项目场景为 CTF（`ctf`）、渗透测试（`pentest`）、代码审计（`audit`），执行阶段仍为 `bootstrap/reason/explore`。渗透测试场景会在规划、执行、收尾和格式修复中加载证据过滤规则；指纹、配置、暴露面和未经动态证实的风险先作为线索，只有稳定复现安全边界失效及实际影响后才报告漏洞。原始观察仍可保留为 Fact，待验证推断使用 Finding `candidate`。
- 渗透测试执行提供 `cvss31` 工具，输入 `{"vector":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}`，返回规范化向量、9.8 分、`CRITICAL` 及计算过程字段。算法按本地参考计算器的行为用 Go 实现，遵循 [FIRST CVSS 3.1 Base 公式与舍入规则](https://www.first.org/cvss/v3.1/specification-document)，无需 Node.js。八项指标必须有证据依据，分数不证明漏洞成立；证据不足或收尾时尚无计算结果则标注未评分。规则位于任务模板，不增加全局系统提示词；CTF、代码审计和未分类项目不加载该工具或规则。
- 过滤规则来自本地 `refer/过滤.txt`，已内嵌于 Worker；运行和构建无需 `refer/`。过滤是模型的判定要求，不能机械验证语义或保证模型零误报；计算器只校验向量并计算分数。评分及其依据写入现有 Finding 或结果描述，Web、图导出和时间线沿用现有记录展示。
- 交付配置使用 `.env` 中的 StepFun 地址与 `step-5-preview`，通过 Anthropic 兼容消息协议请求；凭据只从环境注入。旧配置可以继续显式指定其他兼容端点和模型。
- 默认请求 `thinking.type=enabled` 与 `output_config.effort=max`；`XLOOM_REASONING_EFFORT` 可选 `low/high/max`。响应中的 `thinking:""` 是需要原样重放的思考块，不是强度配置。默认最大输出为 32768 token，可用 `XLOOM_MAX_OUTPUT_TOKENS` 调整；思考强度、输出上限和时间预算分别管理。
- 上下文压缩默认在估算输入超过 920000 token（`XLOOM_CONTEXT_TOKENS`）或请求输入超过 8 MiB（`XLOOM_CONTEXT_BYTES=8388608`）时触发，任一阈值超过即压缩。92 万是预留输出空间后的输入额度，不是摘要的输出额度，也不会自动随模型窗口或最大输出调整；切换模型时应按实际窗口重新配置。token 数优先根据 Provider usage 加新增内容估算，无有效 usage 时按请求字节数除以 3 估算，因此不是精确 tokenizer 计数；字节阈值提供独立保护。
- 压缩后的目标独立设置为 250000 token（`XLOOM_CONTEXT_TARGET_TOKENS`），作为包含系统与任务提示、工具定义、摘要及近期消息的完整请求估算上限，并继续受上述输入预算约束。压缩时按完整工具调用与结果组尽量保留近期消息；内容较少或大组不能拆开时可能低于 20 万，不填充内容凑数。生成摘要所读取的历史仍使用原请求输入预算；25 万只约束压缩后的请求，摘要输出额度另行管理。
- `timeout: 0` 禁用任务探索时间预算，不禁用模型请求、工具或独立收尾超时。没有固定模型轮次/工具总次数上限。
- 非零探索预算在当前轮次完成后切换收尾；运行时内联图与本次执行的有界证据快照，收尾阶段关闭全部工具，独立截止到期后结束执行。
- 每次执行在 `/workspace/.xloom/runs/<run_id>/` 保存会话、事件与长输出；项目文件共享，临时数据隔离。
- Web 停止保持硬停止；停止/删除/失去认领后不接受旧执行的结果。
- 第一版只运行一个 Dispatcher。默认实例任务并发上限 16、单项目上限 4；每个具名 Worker 后端的 `max_running` 在所有项目间共享，三层限制同时生效。默认 general 后端上限为 16，最多同时接纳 4 个项目；实际执行数量由可执行任务和剩余额度决定，没有独立“实际配额”。
- `completed_action: stop` 保留项目容器文件；`remove` 会删除容器及其可写层中的项目文件。项目删除同样清理对应容器。

已有 Cairn SQLite 文件可通过 `--db-path` 接入；数据库结构与兼容差异以 `internal/board` 中的迁移逻辑为准。

## 原件与证据摘录

Execute/Bootstrap 通过 `graph_action` 提交新 Fact/Finding 时，模型选择已有 UTF-8 文件，Go 从采集时的原始字节生成证据。模型不需要填写 `run_id` 或重抄 `excerpt`：

```json
{"op":"fact","idempotency_key":"observe-response","payload":{"description":"对观察结果的解释","scope":"本次测试范围","observed_at":"2026-09-22T00:00:00Z","evidence":[{"path":"response.json"}]}}
```

相对路径从 workspace 解析；可同时指定 `start_line` 和 `end_line`，从 1 开始、包含首尾行。默认选择全文，超过 8,192 字节时必须缩小行范围，不静默截断。原文件最多 32 MiB，必须为普通 UTF-8 文本文件；选区保留原始换行、转义和 Unicode 码点。整份原件保存到当前 run 的 `evidence/<sha256>.raw`，图继续使用现有 EvidenceRef 字段，路径改为留存文件路径。描述与判断仍由模型负责，原文保真不证明解释正确。

同一幂等键的证据在提交前落盘；相同参数重试复用已保存原件，并核对哈希和摘录，源文件后来改变或删除也不改变该次观察。不同参数需要新键。旧调用若提供 `excerpt`，必须是所选范围内的精确字节子串，不做 Unicode 归一化或猜测修正。Finding 通过 `sources` 复用已有 Fact 最省事；历史跨 run 引用仍由 Server 验证，原记录不自动迁移。

该保证覆盖 Worker 的新证据提交，不改变直接 HTTP API 或普通 `write` 输出；也不会让 Decide 自动打开证据路径。原件随 run 所在的容器/卷保留，`completed_action: remove` 或删除项目仍按现有规则清理文件，需长期保留时自行导出。

## Decide 输入与观测

Decide 保留原有触发规则，以最近成功执行的原始状态快照为基线，优先输入本次变化、当前计划及相关证据。首次运行、事件缺口或上下文过大时回退到有界全景，明确列出省略内容；`read_graph` 支持按节点 `ids` 补读。原始图和执行记录完整保留。图分页、计划操作与最终结果校验状态版本；遇到 `state_changed`，需刷新 overview 并重新读取受影响的证据。失败不会推进成功基线，恢复仍使用原会话与剩余预算。

规划提示优先使用已给出的证据、按 ID 补读缺失支持与冲突依据，必要时仍可扩大分页读取。完全相同的 Step priority 与 reason 写入返回 `unchanged:true`，保留幂等回执，但不增加图版本或计划事件，也不能单独作为 `decided` 的依据；理由改变仍会记录。Server 错误日志包含请求方法、路径和取消状态，用于定位事务收尾问题，不记录查询参数或请求正文。

Dispatcher 的 `decision input` 日志记录触发原因、视图模式、原视图与选中视图的字节数及重复输入；`decision observation` 和执行结果的 `metrics` 记录模型调用、摘要、输入量、usage、耗时、补读与动作结果。相同 run 恢复从 `events.jsonl` 重建累计值，费用无法核实则为 `unknown`，usage 为 `reported_only`、`partial` 或 `unknown`，不代表完整账单。`no_op_observed` 是 Worker 观察结果，实际写入以执行回执和图事件为准。

`replan / keep / unknown` 判断已提供可选旁路试验，默认关闭。在 `dispatch.yaml` 的 `common_env` 或某个 Worker 的 `env` 中设置 `XLOOM_REPLAN_SHADOW: "1"` 后，新建且具有可靠 `changes` 基线、仍有开放 Step 的 Decide 会先进行只读判断；首次全景或没有开放任务时直接走原规划。关闭时删除该配置或设为 `"0"`，重启 Dispatcher；已有会话不补跑。

判断使用原输入的冻结图，按需补读，校验三态枚举与实际已读节点引用；判断历史不进入正式规划。`unknown`、协议错误、任务拒绝、传输故障与预算耗尽分别记录，然后在剩余预算内回到 Decide。即使得到 `keep`，当前版本也照常运行 Decide，不跳过任务、不修改图、不单独推进成功基线。

旁路最多进入 Provider 3 次（含摘要与溢出重试）、成功执行至多 4 次图读取，时间最多 60 秒且受原任务绝对截止时间约束；同 run 恢复不重新获得判断额度。逻辑请求指标可能包含被本地额度拒绝的尝试，Provider 内部 HTTP 重试仍沿用现有机制。执行 `metrics.replan` 保存判断、绑定版本、回退原因和单独消耗，外层 `metrics` 累计旁路与正式规划的总量。必须比较总成本与后续结果，不能把旁路输出或原 Decide 当成正确性的证明。
