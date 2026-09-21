# 统一使用 linux/amd64 构建，避免 Kali 工具与应用二进制架构不一致。
# 本镜像以仓库根为构建上下文，且末层依赖已构建好的 xloom:dev：
#   docker build -t xloom:dev .
#   docker build -f container/Dockerfile -t xloom-worker:dev .

ARG XLOOM_IMAGE=xloom:dev
FROM --platform=linux/amd64 ${XLOOM_IMAGE} AS application

FROM --platform=linux/amd64 kalilinux/kali-rolling@sha256:30399bd65187e06525008dd13eecc2b3439d26a82b2c6b0ba32dee72bd843117 AS runtime

ENV TZ=Asia/Shanghai \
    PYTHONUNBUFFERED=1

# 基础系统依赖 + Kali 工具环境
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       bash ca-certificates curl wget ripgrep fd-find python3 python3-pip python3-venv \
       jq git coreutils procps iproute2 dnsutils unzip zip sudo tzdata \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y kali-linux-headless \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       bsdextrautils npm iputils-ping sshpass ncat rlwrap \
       bloodyad coercer enum4linux-ng pwncat chisel-common-binaries \
       dirsearch yq krb5-user gitleaks naabu nikto netexec adb \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# 用户与工作目录：保留 kali 用户用于手动命令，最终 Worker 仍以 root 运行
RUN if ! id kali >/dev/null 2>&1; then useradd -m -s /bin/bash kali; fi \
    && printf 'kali ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/kali \
    && chmod 0440 /etc/sudoers.d/kali \
    && install -d -o kali -g kali /workspace \
    && rm -rf /home/kali/workspace \
    && ln -s /workspace /home/kali/workspace \
    && ln -sfn /usr/bin/fdfind /usr/local/bin/fd

# Python 统一虚拟环境，避免依赖某个用户的系统 Python 安装目录
RUN python3 -m venv /opt/xloom-venv \
    && /opt/xloom-venv/bin/pip install --no-cache-dir --upgrade pip \
    && /opt/xloom-venv/bin/pip install --no-cache-dir \
       pwntools pymongo tccli awscli

ENV PATH="/opt/xloom-venv/bin:${PATH}"

# Playwright 浏览器安装到共享目录，避免依赖 root/kali 的用户缓存
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright
ENV PLAYWRIGHT_MCP_BROWSER=chromium

# 固定版本以保证可复现；升级时同步更新此处并重新验证 check-worker.sh
RUN npm install -g @playwright/cli@0.1.21 \
    && playwright-cli install \
    && chmod -R a+rX /opt/ms-playwright

# 额外二进制工具
RUN cd /tmp \
    && wget -q "https://github.com/projectdiscovery/katana/releases/download/v1.5.0/katana_1.5.0_linux_amd64.zip" -O katana.zip \
    && wget -q "https://github.com/projectdiscovery/nuclei/releases/download/v3.7.1/nuclei_3.7.1_linux_amd64.zip" -O nuclei.zip \
    && wget -q "https://github.com/hahwul/dalfox/releases/download/v2.12.0/dalfox-linux-amd64.tar.gz" -O dalfox.tar.gz \
    && wget -q "https://github.com/BishopFox/cloudfox/releases/download/v2.0.1/cloudfox-linux-amd64.zip" -O cloudfox.zip \
    && wget -q "https://github.com/ropnop/kerbrute/releases/download/v1.0.3/kerbrute_linux_amd64" -O kerbrute \
    && unzip -q katana.zip katana \
    && mv katana /usr/local/bin/ \
    && unzip -q nuclei.zip nuclei \
    && mv nuclei /usr/local/bin/ \
    && tar -xzf dalfox.tar.gz \
    && mv dalfox-linux-amd64 /usr/local/bin/dalfox \
    && unzip -q cloudfox.zip cloudfox/cloudfox \
    && mv cloudfox/cloudfox /usr/local/bin/ \
    && cp kerbrute /usr/local/bin/ \
    && chmod +x /usr/local/bin/kerbrute \
    && rm -rf /tmp/*

# Nuclei 模板统一放到共享目录，并给 root/kali 写入显式配置。
# 模板固定到指定 commit 保证可复现（更新方式：git ls-remote <url> HEAD）。
RUN rm -rf /opt/nuclei-templates \
    && mkdir -p /opt/nuclei-templates /root/.config/nuclei /home/kali/.config/nuclei \
    && git init -q /opt/nuclei-templates \
    && git -C /opt/nuclei-templates remote add origin "https://github.com/projectdiscovery/nuclei-templates.git" \
    && git -C /opt/nuclei-templates fetch -q --depth=1 origin 883b9d5b0f45857738f35446cdb1b03e09d969d5 \
    && git -C /opt/nuclei-templates checkout -q FETCH_HEAD \
    && printf '%s\n' \
       'disable-update-check: true' \
       'update-template-dir: /opt/nuclei-templates' \
       > /root/.config/nuclei/config.yaml \
    && cp /root/.config/nuclei/config.yaml /home/kali/.config/nuclei/config.yaml \
    && chown -R kali:kali /home/kali/.config/nuclei

# 知识库：固定到指定 commit 保证可复现。
# 更新方式：git ls-remote https://github.com/<owner>/<repo> HEAD
RUN mkdir -p /home/kali/knowledges \
    && cd /home/kali/knowledges \
    && git init -q PayloadsAllTheThings \
    && git -C PayloadsAllTheThings remote add origin https://github.com/swisskyrepo/PayloadsAllTheThings.git \
    && git -C PayloadsAllTheThings fetch -q --depth=1 origin 3ac27901c711bdf3f5b65a7b1d1820a1f65bd09a \
    && git -C PayloadsAllTheThings checkout -q FETCH_HEAD \
    && git init -q InternalAllTheThings \
    && git -C InternalAllTheThings remote add origin https://github.com/swisskyrepo/InternalAllTheThings.git \
    && git -C InternalAllTheThings fetch -q --depth=1 origin 203bb0c0b290bf7c9158c32d43523b8d66f292c1 \
    && git -C InternalAllTheThings checkout -q FETCH_HEAD \
    && git init -q hacktricks \
    && git -C hacktricks remote add origin https://github.com/HackTricks-wiki/hacktricks.git \
    && git -C hacktricks fetch -q --depth=1 origin 9c77624a5bfef944057e8d7063717bc73e42a2f3 \
    && git -C hacktricks checkout -q FETCH_HEAD \
    && git init -q hacktricks-cloud \
    && git -C hacktricks-cloud remote add origin https://github.com/HackTricks-wiki/hacktricks-cloud.git \
    && git -C hacktricks-cloud fetch -q --depth=1 origin 19318375e3d424181afedfbe1d8c808ecc818f3b \
    && git -C hacktricks-cloud checkout -q FETCH_HEAD

# 工具与 Java 利用工具：两个仓库固定到指定 commit 保证可复现
# （更新方式：git ls-remote https://github.com/<owner>/<repo> HEAD）
RUN mkdir -p /home/kali/tools \
    && cd /home/kali/tools \
    && wget -q "https://github.com/frohoff/ysoserial/releases/download/v0.0.6/ysoserial-all.jar" -O ysoserial.jar \
    && git init -q jwt_tool \
    && git -C jwt_tool remote add origin https://github.com/ticarpi/jwt_tool.git \
    && git -C jwt_tool fetch -q --depth=1 origin 3bc7407cf2222d6a821dcc19c776e5a1b1cb9a9b \
    && git -C jwt_tool checkout -q FETCH_HEAD \
    && /opt/xloom-venv/bin/pip install --no-cache-dir -r /home/kali/tools/jwt_tool/requirements.txt \
    && git init -q jdwp-shellifier \
    && git -C jdwp-shellifier remote add origin https://github.com/IOActive/jdwp-shellifier.git \
    && git -C jdwp-shellifier fetch -q --depth=1 origin e82ec26193861ba58179aae7f3fa93654b7abc8a \
    && git -C jdwp-shellifier checkout -q FETCH_HEAD

# PoC / 漏洞库：固定到指定 commit 保证可复现。
# 更新方式：git ls-remote https://github.com/<owner>/<repo> HEAD
RUN mkdir -p /home/kali/pocs \
    && cd /home/kali/pocs \
    && git init -q CVE-PoC \
    && git -C CVE-PoC remote add origin https://github.com/iSee857/CVE-PoC.git \
    && git -C CVE-PoC fetch -q --depth=1 origin 6e6a968cda696555b6cf0e5abffe2c1d8336a520 \
    && git -C CVE-PoC checkout -q FETCH_HEAD \
    && git init -q exphub \
    && git -C exphub remote add origin https://github.com/zhzyker/exphub.git \
    && git -C exphub fetch -q --depth=1 origin 946405b3447dbaf2a5eb49e2f56ec33fe1165e49 \
    && git -C exphub checkout -q FETCH_HEAD \
    && git init -q 2023Hvv_ \
    && git -C 2023Hvv_ remote add origin https://github.com/passwa11/2023Hvv_.git \
    && git -C 2023Hvv_ fetch -q --depth=1 origin 9f530c910492ba03b58befe84c301c865eb9fc81 \
    && git -C 2023Hvv_ checkout -q FETCH_HEAD \
    && git init -q Awesome-POC \
    && git -C Awesome-POC remote add origin https://github.com/Threekiii/Awesome-POC.git \
    && git -C Awesome-POC fetch -q --depth=1 origin 0f9e22b660cb1ea12c1c3668f9128d93e08da412 \
    && git -C Awesome-POC checkout -q FETCH_HEAD \
    && git init -q vulhub \
    && git -C vulhub remote add origin https://github.com/vulhub/vulhub.git \
    && git -C vulhub fetch -q --depth=1 origin 8fd63916f7a8711e2e01dda0d27237e4d6175d38 \
    && git -C vulhub checkout -q FETCH_HEAD

# 云 CLI：分步执行并校验产物。
# 原写法 bash -c "$(curl ...)" 在 curl 失败且无输出时会让 bash 收到空脚本并返回 0，
# 造成构建假成功。该脚本在 CWD 下载解包后 mv 到 /usr/local/bin，故先进入 /tmp。
RUN cd /tmp \
    && curl -fsSL -o aliyun-install.sh https://aliyuncli.alicdn.com/install.sh \
    && test -s aliyun-install.sh \
    && bash aliyun-install.sh \
    && rm -f aliyun-install.sh \
    && command -v aliyun

# 保留清理，但不再放宽 /etc/ssl/private 等系统目录权限
RUN rm -rf /usr/lib/llvm-*/build || true

# kali 用户提示符与工作目录初始化。
# /workspace 属主是 kali（见上），但 git init 与最终 Worker 都以 root 运行；
# 不设置例外会让 `git -C /workspace status` 以 128 (dubious ownership) 失败。
# 这里只信任这一个可信工作目录，不使用 safe.directory '*' 全局放宽。
RUN printf '%s\n' 'PS1="${PS1%\\n*} \[\033[0;36m\]\D{%y-%m-%d} - \t\[\033[;32m\]\n${PS1##*\\n}"' >> /home/kali/.bashrc \
    && mkdir -p /workspace \
    && cd /workspace \
    && git init -q \
    && git config --system --add safe.directory /workspace

# 项目侧 Agent 说明文件统一挂到 /workspace。
# 本镜像以仓库根为构建上下文，源路径必须带 container/ 前缀：仓库根没有
# .agents 目录，根的 AGENTS.md 被 .dockerignore 排除且含本机开发凭据，
# 不能作为来源（也不应解除该排除规则）。
COPY container/.agents /workspace/.agents
COPY container/.agents /workspace/.claude
COPY container/AGENTS.md /workspace/AGENTS.md
COPY container/AGENTS.md /workspace/CLAUDE.md

# X-Loom Worker 运行结构
COPY container/environment.md container/check-worker.sh /usr/local/share/xloom/
RUN chmod 0755 /usr/local/share/xloom/check-worker.sh

# 最后复制应用二进制，最大化复用前面的 Kali 工具安装层
COPY --from=application /usr/local/bin/xloom /usr/local/bin/xloom

# Dispatcher currently injects root-owned private job/session files.
# Preserve that execution contract; the kali user remains available for manual commands.
USER root
WORKDIR /workspace
RUN /usr/local/share/xloom/check-worker.sh
ENTRYPOINT ["/usr/local/bin/xloom"]
CMD ["worker", "--help"]