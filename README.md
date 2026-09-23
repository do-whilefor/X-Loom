# X-Loom

X-Loom 是一个使用 Go 构建的协作探索系统，提供 Server、Dispatcher、Worker 与 Web 工作台，并通过 Docker 动态创建隔离的 Worker 执行任务。

> 当前主要面向 Linux 环境。

## Docker 使用

### 1. 克隆项目

```bash
git clone https://github.com/do-whilefor/X-Loom.git
cd X-Loom
```

### 2. 准备配置

```bash
cp .env.example .env
cp dispatch.example.yaml dispatch.yaml
```

编辑 `.env`，至少配置模型访问凭证：

```env
ANTHROPIC_AUTH_TOKEN=
ANTHROPIC_BASE_URL=
ANTHROPIC_DEFAULT_FABLE_MODEL=
```

如有需要，可同时调整模型 API 地址和模型名称。

### 3. 构建镜像

```bash
docker build -t xloom:dev .
docker build -f container/Dockerfile -t xloom-worker:dev .
```

确保 `dispatch.yaml` 中的 Worker 镜像为：

```yaml
container:
  image: xloom-worker:dev
```

### 4. 启动

```bash
docker compose up -d --no-build
docker compose ps
```

默认 Web 地址：

```text
http://127.0.0.1:8000
```

停止服务：

```bash
docker compose down
```

Dispatcher 会根据任务动态创建 Worker 容器，因此需要访问 Docker Socket，请仅在可信主机上运行。

## 使用限制

本项目仅可用于合法、安全且已获得明确授权的学习、研究和测试活动。

禁止将本项目用于未经授权的系统、网络、账号或设备，包括但不限于非法入侵、破坏、数据窃取、恶意攻击、规避安全措施或其他违反适用法律法规的行为。

使用者应自行确保其使用行为符合所在地法律法规及目标系统的授权范围，由违规使用产生的责任由使用者自行承担。

## License

本项目采用 **PolyForm Noncommercial License 1.0.0**。

允许在许可证规定范围内进行非商业学习、研究、实验、修改和分发；未经项目作者另行授权，不得用于商业产品、商业服务、SaaS、收费安全服务或其他商业用途。

完整许可条款请参阅仓库中的 [`LICENSE`](./LICENSE) 文件。
