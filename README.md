# X-Loom

使用 Go 重写 Cairn 的协作探索系统。一个二进制提供 `serve`、`dispatch`、`worker`；保留 Cairn 黑板图、三类任务和原 Web，使用自研的 Pi 风格两层 Agent Loop。

仅支持 Linux。计划与验收依据见 [DEVELOPMENT.md](DEVELOPMENT.md)，实际兼容边界见 [docs/compatibility.md](docs/compatibility.md)。

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
web/static         原 Cairn Web，原样嵌入
tests/integration  真实容器链路与真实模型的显式验收
tests/compatibility 原 Cairn 与 Go API 差分检查
```

`process` 供 Worker 生命周期和 shell 工具共用，避免重复实现 Linux 进程取消。包内单元测试与实现文件相邻。

## 构建与测试

在带有 Go 1.23+、C 编译器、bash、ripgrep 的 Linux 环境中：

```sh
go test -race ./...
go vet ./...
go build -o ./bin/xloom ./cmd/xloom
```

项目 Dockerfile 的构建阶段强制执行 race 测试与 vet，通过后才产出运行镜像：

```sh
docker build -t xloom:dev .
```

默认自动测试使用 Mock 模型，不消耗模型额度。真实 Docker 和真实模型测试需要显式启用，命令见 [验收记录](docs/validation.md)。

## 启动

```sh
cp dispatch.example.yaml dispatch.yaml
cp .env.example .env
# 在 .env 中填写 ANTHROPIC_AUTH_TOKEN
docker compose up -d
```

使用前先构建 `xloom:dev`。访问 <http://localhost:8000>。Server 的数据库在 `xloom-data` 卷中，项目 Worker 容器由 Dispatcher 动态创建。初始镜像只包含 Go 程序、bash、ripgrep 与 curl；任务需要的其他程序应添加到自定义 Worker 镜像，并更新 `container.image`。

不使用 Compose 时，在 Linux 上可直接运行：

```sh
./bin/xloom serve --host 127.0.0.1 --port 8000 --db-path ./data/xloom.db
./bin/xloom dispatch --config ./dispatch.yaml
```

此时把配置的 `server` 改为实际 Server URL；Dispatcher 仍通过 Docker socket 管理项目容器。`worker --job <job.json>` 由 Dispatcher 调用；`worker --cancel <run-directory>` 只取消该次执行。

## 配置与行为

- 指定模型默认目标为 `https://opencode.ai/zen/go`、`deepseek-v4.1-flash`；凭据只从环境注入。OpenCode Go 请求携带稳定会话标识和 X-Loom 客户端标识。
- `timeout: 0` 禁用任务探索时间预算，不禁用模型请求、工具或独立收尾超时。没有固定模型轮次/工具总次数上限。
- 非零探索预算在当前轮次完成后切换收尾；运行时内联图与本次执行的有界证据快照，收尾阶段关闭全部工具，独立截止到期后结束执行。
- 每次执行在 `/workspace/.xloom/runs/<run_id>/` 保存会话、事件与长输出；项目文件共享，临时数据隔离。
- Web 停止保持硬停止；停止/删除/失去认领后不接受旧执行的结果。
- 第一版只运行一个 Dispatcher。多个 Worker 配置可按任务类型、优先级与并发额度共同工作。
- `completed_action: stop` 保留项目容器文件；`remove` 会删除容器及其可写层中的项目文件。项目删除同样清理对应容器。

已有 Cairn SQLite 文件可通过 `--db-path` 接入；迁移与明确差异见 [服务端兼容说明](docs/compatibility-server.md)。
