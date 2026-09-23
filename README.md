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

## 测试与 CI

GitHub Actions 会在推送代码、提交 PR 或手动触发时，在新的 Linux 环境中检出对应提交，执行单元测试、Mock 集成测试、竞态检查、`go vet` 和程序编译。

下载源码后，在项目根目录运行同一套检查（需要 Docker）：

```bash
docker build --target build --progress=plain .
```

无需创建 `.env` 或 `dispatch.yaml`，也无需启动项目、准备 Worker 镜像或提供模型密钥。检查使用已提交的测试及代码内合成的数据；模型响应由 Mock 提供。拉取基础镜像、安装工具和下载 Go 依赖需要联网，之后测试、静态检查及编译阶段禁用外网。CI 不调用真实模型，不发布镜像，也不部署服务。

GitHub CI 使用根目录 Dockerfile 指定的 Go 1.26，在 Linux amd64 上检查；本地命令默认使用 Docker 主机架构。这不代表已验证 Go 1.23 的最低版本兼容性或其他架构。本地专用的真实模型、Docker 联调及参考源码验收脚本不属于这套 CI。

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
