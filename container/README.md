# Kali 基础 Worker 镜像

仅安装基础运行工具：bash、curl、wget、rg、fd、Python 3/pip/venv、jq、git、
coreutils、procps、ip、dig、zip/unzip、sudo、CA 证书和时区数据。
不预装安全工具集、浏览器、云 CLI、第三方 Python 包或知识库/PoC。
`python`/`python3` 和 `pip`/`pip3` 使用 `/opt/xloom-venv`，便于任务按需安装依赖。
完整运行约定见 [environment.md](environment.md)。

在**仓库根目录**执行。本 Dockerfile 的所有 `COPY` 源路径都以仓库根为构建上下文
（与 `compose.yaml` 中 `worker-image` 的 `context: .` 一致），在 `container/` 目录内构建会找不到源文件。

先构建控制镜像，再构建 Worker 镜像（Worker 末层从 `xloom:dev` 复制 Go 二进制）：

```bash
docker build -t xloom:dev .
docker build -f container/Dockerfile -t xloom-worker:dev .
```

使用其他控制镜像标签时，通过 `--build-arg XLOOM_IMAGE=xloom:<tag>` 指定。

或通过 compose：

```bash
docker compose --profile images build worker-image
```

### apt 镜像源

镜像默认使用 Kali 官方源。官方源不可用时可显式指定镜像源，缺省留空表示行为不变：

```bash
docker build -f container/Dockerfile \
  --build-arg KALI_MIRROR=http://mirrors.tuna.tsinghua.edu.cn/kali \
  -t xloom-worker:dev .
```

该参数只改写 `sources.list.d/kali.sources` 的 `URIs` 字段，套件、组件与签名配置保持不变。

必须使用 `http://`。基础 Kali 镜像为精简镜像，在执行该替换时尚未安装
`ca-certificates`，`https://` 源会因无法校验证书而失败；官方默认源本身也是 http。
包完整性由 apt 的 `Signed-By`（kali-archive-keyring）校验，不依赖传输层加密。

镜像自检。`check-worker.sh` 已随镜像分发到 `/usr/local/share/xloom/`，可直接离线运行，并顺带验证镜像自身的 ENTRYPOINT：

```bash
# 运行镜像内置的环境与运行结构检查
docker run --rm --pull never --network none --init \
  --entrypoint /usr/local/share/xloom/check-worker.sh xloom-worker:dev

# 验证镜像自身 ENTRYPOINT/CMD（等价于 xloom worker --help）
docker run --rm --pull never --network none xloom-worker:dev
```
