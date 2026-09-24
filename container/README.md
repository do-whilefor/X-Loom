# Kali Headless Worker 镜像

以官方 `kalilinux/kali-rolling` 为基础，安装 `kali-linux-headless` 元包及其必需工具依赖，
再安装 pwntools、pymongo、AWS CLI v1、腾讯云 tccli、阿里云 aliyun，以及全局 Playwright CLI 和 Chromium。
`kali-linux-headless` 是 APT 元包，不是 Docker 的 `FROM` 镜像名。

明确补齐 bsdextrautils、nodejs/npm、jq、iputils-ping、sshpass、ncat、rlwrap、yq、krb5-user、adb、
ripgrep（`rg`）和 fd-find（`fd`）。其中 yq 使用 Kali 的 jq 风格 Python 实现，例如 `yq -r '.name' file.yaml`。
APT 使用 `--no-install-recommends`；安装期间禁止自动启动软件包服务，实际任务需要时再启动。

`python`/`python3` 和 `pip`/`pip3` 使用 `/opt/xloom-venv`；tccli 使用独立虚拟环境，
其命令同样已加入 PATH。任务可按需继续安装 Python 依赖。
两个虚拟环境均使用 Python 3.13，避免 pwntools 4.15.0 在 Python 3.14 下的字节码兼容问题。
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

### 工具版本

| 构建参数 | 默认值 |
| --- | --- |
| `PWNTOOLS_VERSION` | `4.15.0` |
| `PYMONGO_VERSION` | `4.18.1` |
| `AWSCLI_VERSION` | `1.46.1`（Python 包，CLI v1） |
| `TCCLI_VERSION` | `3.1.173.1` |
| `ALIYUN_CLI_VERSION` | `3.5.1` |

Aliyun 从官方 GitHub Release 下载；升级 `ALIYUN_CLI_VERSION` 时，必须同步
`ALIYUN_CLI_SHA256`，使用对应版本、Linux amd64 发布包的校验值。
默认值来源于 [v3.5.1 的 SHASUMS256.txt](https://github.com/aliyun/aliyun-cli/releases/download/v3.5.1/SHASUMS256.txt)。
可通过 `--build-arg <参数名>=<版本>` 覆盖这些默认值，修改后重新构建并运行自检。

Node.js 和 npm 统一由 Kali APT 提供，提供 `node`、`npm`、`npx`；自检要求 Node.js 至少为 20。
Playwright 按 `@playwright/cli@latest` 全局安装，构建时执行 `playwright-cli install`
预装匹配的 Chromium。Docker 缓存可能复用先前解析的版本；需要重新获取 `latest` 时，
使用 `docker build --no-cache -f container/Dockerfile -t xloom-worker:dev .`，并重新运行下方自检。

### apt 镜像源

镜像默认使用基础镜像中的 Kali 官方分发源。分发节点不可用时，可以固定使用
Kali 官方 HTTPS 下载站；也支持中科大 USTC 等镜像源。缺省留空表示行为不变：

```bash
docker build -f container/Dockerfile \
  --build-arg KALI_MIRROR=https://kali.download/kali \
  -t xloom-worker:dev .
```

该参数只改写 `sources.list.d/kali.sources` 的 `URIs` 字段，套件、组件与签名配置保持不变。

支持 `http://` 和 `https://`。固定的裸 Kali 镜像缺少 CA 证书；Dockerfile 在首次
APT 请求前从控制镜像复制 CA 证书包，并用 `Acquire::https::CaInfo` 显式指定路径，
再正常安装 Kali 的 `ca-certificates`。
因此 HTTPS 源不再依赖先用 HTTP 安装证书，也不会关闭 TLS 证书校验或 APT 签名校验。
自定义 `XLOOM_IMAGE` 必须同时提供 X-Loom 二进制和 `/etc/ssl/certs/ca-certificates.crt`。

APT 索引下载失败会使构建失败，避免把使用旧索引的警告误判为成功；临时下载错误最多重试两次。

### 离线验证与使用

`check-worker.sh` 已随镜像分发到 `/usr/local/share/xloom/`。自检检查 headless 元包和新增工具，
实际执行回环 ping、文本与 YAML 处理、pwntools 汇编和常量求值、
BSON 编解码、云 CLI 版本命令、由 groff-base 渲染的 AWS CLI 帮助，以及 Playwright CLI 打开回环地址页面并验证 JavaScript 运行结果；
无需外网或云凭据。

```bash
# 运行镜像内置的环境与运行结构检查
docker run --rm --pull never --network none --init \
  --entrypoint /usr/local/share/xloom/check-worker.sh xloom-worker:dev

# 验证镜像自身 ENTRYPOINT/CMD（等价于 xloom worker --help）
docker run --rm --pull never --network none xloom-worker:dev
```

也可在容器内直接使用这些包和命令。以下示例在 `/tmp` 打开 Chromium 空白页，全程离线：

```bash
docker run --rm --pull never --network none --init \
  --entrypoint /bin/sh xloom-worker:dev -ec '
    python -c "from pwn import context, asm; import pymongo; context.arch = \"amd64\"; print(asm(\"ret\").hex(), pymongo.version)"
    aws --version
    MANPAGER=cat PAGER=cat aws help >/dev/null
    tccli --version
    aliyun version
    playwright-cli --version
    cd /tmp
    playwright-cli -s=demo open about:blank
    playwright-cli -s=demo snapshot
    playwright-cli -s=demo close
  '
```

Chromium 默认无头运行，root 环境使用 `sandbox=false`。浏览器位于 `/opt/ms-playwright`，
CLI 通过 `/usr/local/bin/xloom-chromium` 固定入口运行，可从任意工作目录调用，不需要项目 skills。
实际访问 MongoDB、云服务或外部网页时，再由任务提供目标地址、所需凭据和网络连接。
