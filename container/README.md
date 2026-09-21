# Kali Worker 镜像

在**仓库根目录**执行。本 Dockerfile 的所有 `COPY` 源路径都以仓库根为构建上下文
（与 `compose.yaml` 中 `worker-image` 的 `context: .` 一致），在 `container/` 目录内构建会找不到源文件。

先构建控制镜像，再构建 Worker 镜像（Worker 末层从 `xloom:dev` 复制 Go 二进制）：

```bash
docker build -t xloom:dev .
docker build -f container/Dockerfile -t xloom-worker:dev .
```

或通过 compose：

```bash
docker compose --profile images build worker-image
```

镜像自检。`check-worker.sh` 已随镜像分发到 `/usr/local/share/xloom/`，可直接离线运行，并顺带验证镜像自身的 ENTRYPOINT：

```bash
# 运行镜像内置的环境与运行结构检查
docker run --rm --pull never --init \
  --entrypoint /usr/local/share/xloom/check-worker.sh xloom-worker:dev

# 验证镜像自身 ENTRYPOINT/CMD（等价于 xloom worker --help）
docker run --rm --pull never xloom-worker:dev
```
