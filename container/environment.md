# X-Loom Worker 环境

- 基于官方 Kali rolling 镜像，使用 Go `xloom worker` 执行 Agent Loop。
- `/workspace` 是同一项目共享的工作目录；`/home/kali/workspace` 指向同一目录。
- `/workspace/.xloom/runs/<run-id>` 保存单次执行的任务、会话和工具输出。
- 有 bash、curl、ripgrep (`rg`)、fd、Python 3/pip、jq、git、procps、iproute2、DNS 查询、zip/unzip。
- 默认用户为 root，以兼容 Dispatcher 写入的私有任务文件。保留 `kali` 用户和免密码 sudo，便于手动使用。
- 时区为 `Asia/Shanghai`，Python 输出不缓冲。

此文档只是镜像环境说明，不自动注入模型提示词。镜像未安装完整 Kali 工具集、漏洞利用或 PoC 库，也不包含 Cairn 的外部代理 CLI、竞赛提交技能。
