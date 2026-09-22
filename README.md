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

```bash
docker compose up -d
```

访问：

```text
http://127.0.0.1:8000
```

停止：

```bash
docker compose down
```

默认 `docker compose up -d` 启动 Server 与 Dispatcher；Worker 容器由 Dispatcher 按任务动态创建。

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
