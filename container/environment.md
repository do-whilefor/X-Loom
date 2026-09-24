# X-Loom Worker 环境

- 基于官方 Kali rolling 镜像（`linux/amd64`），使用 Go `xloom worker` 执行 Agent Loop。
- `/workspace` 是同一项目共享的工作目录；`/home/kali/workspace` 指向同一目录，且已初始化为 git 仓库。
- `/workspace/.xloom/runs/<run-id>` 保存单次执行的任务、会话和工具输出。
- 默认用户为 root，以兼容 Dispatcher 写入的私有任务文件。保留 `kali` 用户和免密码 sudo，便于手动使用。
- 时区为 `Asia/Shanghai`，Python 输出不缓冲。
- 首次 APT 请求前从控制镜像提供 CA 证书，支持 HTTPS 镜像源；构建完成后由 Kali 的 `ca-certificates` 管理证书。

## 基础工具

bash、curl、wget、ripgrep (`rg`)、fd (`fdfind`)、Python 3.13、pip、venv、jq、git、coreutils、procps (`ps`)、iproute2 (`ip`)、dnsutils (`dig`)、zip/unzip、sudo，以及 CA 证书和时区数据。另含 binutils、cpp、C/C++ 构建工具和 Python 3.13 开发依赖，支持 pwntools 的本机汇编及 Python 包源码安装。

## Python 与云 CLI

`/opt/xloom-venv/bin` 已加入 PATH，`python`/`python3` 和 `pip`/`pip3` 默认使用该虚拟环境。两个虚拟环境均使用 Python 3.13，避免 pwntools 4.15.0 对 Python 3.14 部分字节码不兼容的问题。预装：

| 包 | 默认版本 | 用法 |
| --- | --- | --- |
| pwntools | 4.15.0 | Python 中 `from pwn import ...`，支持 amd64 汇编 |
| pymongo | 4.18.1 | Python 中 `import pymongo`，提供 MongoDB 客户端和 BSON 编解码 |
| awscli | 1.46.1 | AWS CLI v1，命令 `aws`；预装 groff-base 支持离线帮助 |

`tccli` 3.1.173.1 使用独立的 `/opt/tccli-venv`，并通过 `/usr/local/bin/tccli` 加入 PATH，避免其 SDK 依赖与任务追加的 Python 包互相影响。`REQUESTS_CA_BUNDLE` 指向 `/etc/ssl/certs/ca-certificates.crt`，requests 使用系统 CA。

`aliyun` 3.5.1 来自官方 Linux amd64 发布包，构建时校验固定 SHA256。三个云 CLI 的版本命令均可离线执行；访问云服务时由任务提供相应凭据和网络连接。镜像不预置云凭据。

任务可继续用 `python -m pip install ...` 向共享虚拟环境安装依赖。

## 浏览器

从官方 Linux x64 发布包安装 Node.js 24.21.0 和 npm，构建时校验 SHA256，提供 `node`、`npm`、`npx`。全局安装 `@playwright/cli@latest`，命令为 `playwright-cli`；构建时通过 `playwright-cli install` 下载 Chromium，并安装浏览器运行库及字体。

浏览器文件位于 `/opt/ms-playwright`；`/usr/local/bin/xloom-chromium` 指向该版本的 Chromium。环境变量默认选择 Chromium、无头模式和 `sandbox=false`，适配镜像默认的 root 用户。CLI 通过 `PLAYWRIGHT_MCP_EXECUTABLE_PATH` 使用固定入口，可在任意工作目录运行，无需再次安装浏览器。

`latest` 在安装层实际执行时解析；Docker 缓存命中时沿用已有版本。更新方式和离线验证命令见仓库中的 `container/README.md`。

## 安装范围

预装上述工具及 X-Loom 二进制；不安装整套 Kali 安全工具、nmap、nuclei、知识库或 PoC，也不复制原竞赛环境的 `AGENTS.md`、`CLAUDE.md` 和技能目录。Playwright 初始化在临时目录完成，不向项目注入 skills 或配置。

此文档只是镜像环境说明，不自动注入模型提示词。
