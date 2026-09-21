# 统一使用 linux/amd64 构建，避免 Kali 工具与应用二进制架构不一致：
# docker build --platform linux/amd64 -t xloom:dev .
# docker build --platform linux/amd64 -f Dockerfile.kali -t xloom:kali .

ARG XLOOM_IMAGE=xloom:dev
FROM --platform=linux/amd64 ${XLOOM_IMAGE} AS application

FROM --platform=linux/amd64 kalilinux/kali-rolling@sha256:30399bd65187e06525008dd13eecc2b3439d26a82b2c6b0ba32dee72bd843117 AS runtime

ENV TZ=Asia/Shanghai \
    PYTHONUNBUFFERED=1

# 基础系统依赖 + Kali 工具环境
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       bash ca-certificates curl ripgrep fd-find python3 python3-pip python3-venv \
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

RUN npm install -g @playwright/cli@latest \
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

# Nuclei 模板统一放到共享目录，并给 root/kali 写入显式配置
RUN rm -rf /opt/nuclei-templates \
    && mkdir -p /opt/nuclei-templates /root/.config/nuclei /home/kali/.config/nuclei \
    && git clone --depth 1 "https://github.com/projectdiscovery/nuclei-templates.git" /opt/nuclei-templates \
    && printf '%s\n' \
       'disable-update-check: true' \
       'update-template-dir: /opt/nuclei-templates' \
       > /root/.config/nuclei/config.yaml \
    && cp /root/.config/nuclei/config.yaml /home/kali/.config/nuclei/config.yaml \
    && chown -R kali:kali /home/kali/.config/nuclei

# 知识库
RUN mkdir -p /home/kali/knowledges \
    && cd /home/kali/knowledges \
    && git clone --depth=1 https://github.com/swisskyrepo/PayloadsAllTheThings.git \
    && git clone --depth=1 https://github.com/swisskyrepo/InternalAllTheThings.git \
    && git clone --depth=1 https://github.com/HackTricks-wiki/hacktricks.git \
    && git clone --depth=1 https://github.com/HackTricks-wiki/hacktricks-cloud.git

# 工具与 Java 利用工具；修正 jdwp-shellifier 的克隆路径
RUN mkdir -p /home/kali/tools \
    && cd /home/kali/tools \
    && wget -q "https://github.com/frohoff/ysoserial/releases/download/v0.0.6/ysoserial-all.jar" -O ysoserial.jar \
    && git clone --depth=1 https://github.com/ticarpi/jwt_tool.git /home/kali/tools/jwt_tool \
    && /opt/xloom-venv/bin/pip install --no-cache-dir -r /home/kali/tools/jwt_tool/requirements.txt \
    && git clone --depth=1 https://github.com/IOActive/jdwp-shellifier.git /home/kali/tools/jdwp-shellifier

# PoC / 漏洞库
RUN mkdir -p /home/kali/pocs \
    && cd /home/kali/pocs \
    && git clone --depth=1 https://github.com/iSee857/CVE-PoC.git \
    && git clone --depth=1 https://github.com/zhzyker/exphub.git \
    && git clone --depth=1 https://github.com/passwa11/2023Hvv_.git \
    && git clone --depth=1 https://github.com/Threekiii/Awesome-POC.git \
    && git clone --depth=1 https://github.com/vulhub/vulhub.git

# 云 CLI
RUN bash -c "$(curl -fsSL https://aliyuncli.alicdn.com/install.sh)"

# 保留清理，但不再放宽 /etc/ssl/private 等系统目录权限
RUN rm -rf /usr/lib/llvm-*/build || true

# kali 用户提示符与工作目录初始化
RUN printf '%s\n' 'PS1="${PS1%\\n*} \[\033[0;36m\]\D{%y-%m-%d} - \t\[\033[;32m\]\n${PS1##*\\n}"' >> /home/kali/.bashrc \
    && mkdir -p /workspace \
    && cd /workspace \
    && git init -q

# 项目侧 Agent 说明文件统一挂到 /workspace
COPY ./.agents /workspace/.agents
COPY ./.agents /workspace/.claude
COPY ./AGENTS.md /workspace/AGENTS.md
COPY ./AGENTS.md /workspace/CLAUDE.md

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