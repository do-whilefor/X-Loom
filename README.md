# X-Loom

使用 Go 重写 Cairn 的协作探索系统。一个二进制提供 `serve`、`dispatch`、`worker`；保留 Cairn 黑板图与三类执行任务，接入 X-Loom 三栏 Web 工作台，使用自研的 Pi 风格两层 Agent Loop。

仅支持 Linux。

系统已接入会话、上下文压缩与 FGS 状态扩展。Web 首页展示真实项目、FGS 状态、事件与执行摘要；经典 Cairn 管理页保留在 `/static/legacy.html`。

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
internal/process    Linux 执行取消与子进程清理
container           Kali Worker 镜像、环境说明和镜像检查脚本
web/static          X-Loom 工作台、经典 Cairn 管理页与本地静态资源
web/tests           Web 数据转换、图模型和请求隔离测试
```

`process` 供 Worker 生命周期和 shell 工具共用，避免重复实现 Linux 进程取消。包内单元测试与实现文件相邻。

## 构建与测试

在带有 Go 1.23+、C 编译器、bash、ripgrep 的 Linux 环境中：

```sh
go test -race ./...
go vet ./...
go build -o ./bin/xloom ./cmd/xloom
```

Web 使用原生 HTML/CSS/JavaScript，无 npm 安装或前端构建步骤。安装 Node.js 后可执行 `node --test web/tests/*.test.js`；静态资源随 Go 二进制嵌入，更新页面后需要重新构建和启动 Server。

项目 Dockerfile 的构建阶段强制执行 race 测试与 vet，通过后才产出运行镜像。Worker 镜像以**仓库根**为构建上下文，且末层从 `xloom:dev` 复制 Go 二进制，因此必须先构建控制镜像：

```sh
docker build -t xloom:dev .
docker build -f container/Dockerfile -t xloom-worker:dev .
```

默认自动测试使用 Mock 模型，不消耗模型额度。真实 Docker 与真实模型测试需要显式设置环境变量后才运行，未设置时自动跳过（如 `XLOOM_DOCKER_TEST_IMAGE`、`XLOOM_LIVE_MODEL_TEST=1`）。

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

- 交付配置使用 `.env` 中的 StepFun 地址与 `step-5-preview`，通过 Anthropic 兼容消息协议请求；凭据只从环境注入。旧配置可以继续显式指定其他兼容端点和模型。
- 默认请求 `thinking.type=enabled` 与 `output_config.effort=max`；`XLOOM_REASONING_EFFORT` 可选 `low/high/max`。响应中的 `thinking:""` 是需要原样重放的思考块，不是强度配置。默认最大输出为 32768 token，可用 `XLOOM_MAX_OUTPUT_TOKENS` 调整；思考强度、输出上限和时间预算分别管理。
- `timeout: 0` 禁用任务探索时间预算，不禁用模型请求、工具或独立收尾超时。没有固定模型轮次/工具总次数上限。
- 非零探索预算在当前轮次完成后切换收尾；运行时内联图与本次执行的有界证据快照，收尾阶段关闭全部工具，独立截止到期后结束执行。
- 每次执行在 `/workspace/.xloom/runs/<run_id>/` 保存会话、事件与长输出；项目文件共享，临时数据隔离。
- Web 停止保持硬停止；停止/删除/失去认领后不接受旧执行的结果。
- 第一版只运行一个 Dispatcher。默认实例任务并发上限 16、单项目上限 4；每个具名 Worker 后端的 `max_running` 在所有项目间共享，三层限制同时生效。默认 general 后端上限为 16，最多同时接纳 4 个项目；实际执行数量由可执行任务和剩余额度决定，没有独立“实际配额”。
- `completed_action: stop` 保留项目容器文件；`remove` 会删除容器及其可写层中的项目文件。项目删除同样清理对应容器。

已有 Cairn SQLite 文件可通过 `--db-path` 接入；数据库结构与兼容差异以 `internal/board` 中的迁移逻辑为准。
