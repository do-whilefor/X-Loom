# X-Loom Worker 环境

- 基于官方 Kali rolling 镜像（`linux/amd64`），使用 Go `xloom worker` 执行 Agent Loop。
- `/workspace` 是同一项目共享的工作目录；`/home/kali/workspace` 指向同一目录，且已初始化为 git 仓库。
- `/workspace/.xloom/runs/<run-id>` 保存单次执行的任务、会话和工具输出。
- 默认用户为 root，以兼容 Dispatcher 写入的私有任务文件。保留 `kali` 用户和免密码 sudo，便于手动使用。
- 时区为 `Asia/Shanghai`，Python 输出不缓冲。

## 基础工具

bash、curl、wget、ripgrep (`rg`)、fd、Python 3/pip、jq、git、procps、iproute2、DNS 查询、zip/unzip、sudo、npm。

## 安全工具

- Kali 工具集：`kali-linux-headless`，外加 bloodyad、coercer、enum4linux-ng、pwncat、dirsearch、yq、krb5-user、gitleaks、naabu、nikto、netexec、adb、ncat、rlwrap、sshpass。
- chisel 二进制位于 `/usr/share/chisel-common-binaries`。
- 额外二进制：katana、nuclei、dalfox、cloudfox、kerbrute、ysoserial。
- Python 虚拟环境 `/opt/xloom-venv`（已在 PATH 中）：pwntools、pymongo、tccli、awscli；`jwt_tool` 的依赖也装在这里。
- 云 CLI：`aliyun`。

## 浏览器

Playwright CLI 与其 Chromium 浏览器位于 `/opt/ms-playwright`（`playwright-cli --help` 查看用法，非必要不要使用）。

## 知识库与 PoC

- `/opt/nuclei-templates`：nuclei 模板，已为 root 和 kali 写好 `disable-update-check` 与 `update-template-dir` 配置。
- `/home/kali/knowledges`：PayloadsAllTheThings、InternalAllTheThings、hacktricks、hacktricks-cloud。
- `/home/kali/pocs`：CVE-PoC、exphub、2023Hvv_、Awesome-POC、vulhub。
- `/home/kali/tools`：ysoserial、jwt_tool、jdwp-shellifier。

以上仓库均固定在指定 commit，以保证镜像可复现。

## 不包含

Cairn 的外部代理 CLI（Codex、Claude Code、Pi）——任务由 X-Loom 自有 Go Agent Loop 执行。

此文档只是镜像环境说明，不自动注入模型提示词。
