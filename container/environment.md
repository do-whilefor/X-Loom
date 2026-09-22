# X-Loom Worker 环境

- 基于官方 Kali rolling 镜像（`linux/amd64`），使用 Go `xloom worker` 执行 Agent Loop。
- `/workspace` 是同一项目共享的工作目录；`/home/kali/workspace` 指向同一目录，且已初始化为 git 仓库。
- `/workspace/.xloom/runs/<run-id>` 保存单次执行的任务、会话和工具输出。
- 默认用户为 root，以兼容 Dispatcher 写入的私有任务文件。保留 `kali` 用户和免密码 sudo，便于手动使用。
- 时区为 `Asia/Shanghai`，Python 输出不缓冲。

## 基础工具

bash、curl、wget、ripgrep (`rg`)、fd (`fdfind`)、Python 3、pip、venv、jq、git、coreutils、procps (`ps`)、iproute2 (`ip`)、dnsutils (`dig`)、zip/unzip、sudo，以及 CA 证书和时区数据。

`/opt/xloom-venv/bin` 已加入 PATH，`python`/`python3` 和 `pip`/`pip3` 默认使用该虚拟环境。镜像不预装第三方 Python 包，任务可按需安装依赖。

## 安装范围

仅预装上述基础运行工具和 X-Loom 二进制。不预装 Kali 安全工具集、浏览器、云 CLI、知识库或 PoC，也不复制原竞赛环境的 `AGENTS.md`、`CLAUDE.md` 和技能目录。

此文档只是镜像环境说明，不自动注入模型提示词。
