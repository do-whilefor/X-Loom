# 在仓库根目录构建；先构建 xloom:dev，再构建本 Worker 镜像。
ARG XLOOM_IMAGE=xloom:dev
FROM --platform=linux/amd64 ${XLOOM_IMAGE} AS application

# kali-linux-headless 是 APT 元包：在官方 Kali 基础镜像中安装其工具集。
FROM --platform=linux/amd64 kalilinux/kali-rolling@sha256:30399bd65187e06525008dd13eecc2b3439d26a82b2c6b0ba32dee72bd843117 AS runtime

ARG KALI_MIRROR=
ENV TZ=Asia/Shanghai \
    PYTHONUNBUFFERED=1 \
    REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt \
    PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright \
    PLAYWRIGHT_MCP_BROWSER=chromium \
    PLAYWRIGHT_MCP_HEADLESS=true \
    PLAYWRIGHT_MCP_SANDBOX=false \
    NO_UPDATE_NOTIFIER=1

# 在第一次 APT 请求前，从已有控制镜像提供 CA，避免 HTTPS 源的冷启动依赖。
# 随后安装 Kali 的 ca-certificates 接管证书更新；不关闭 TLS 或 APT 签名校验。
COPY --from=application /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# 软件包安装期间不启动 Apache、SSH、数据库等服务；运行时按需手动启动。
# 可选镜像源只替换 URI，保留套件、组件和签名校验配置。
RUN printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d \
    && chmod 0755 /usr/sbin/policy-rc.d \
    && if [ -n "$KALI_MIRROR" ]; then \
        case "$KALI_MIRROR" in http://?*|https://?*) ;; *) echo 'KALI_MIRROR must start with http:// or https://' >&2; exit 1 ;; esac; \
        awk -v mirror="$KALI_MIRROR" '$1 == "URIs:" {$0 = "URIs: " mirror} {print}' \
            /etc/apt/sources.list.d/kali.sources > /tmp/kali.sources \
        && mv /tmp/kali.sources /etc/apt/sources.list.d/kali.sources; \
    fi \
    && apt-get -o Acquire::https::CaInfo=/etc/ssl/certs/ca-certificates.crt \
        -o APT::Update::Error-Mode=any -o Acquire::Retries=2 update \
    && DEBIAN_FRONTEND=noninteractive apt-get \
        -o Acquire::https::CaInfo=/etc/ssl/certs/ca-certificates.crt \
        -o Acquire::Retries=2 install -y --no-install-recommends \
        kali-linux-headless \
        bsdextrautils nodejs npm jq iputils-ping sshpass ncat rlwrap yq krb5-user adb \
        bash ca-certificates curl wget ripgrep fd-find \
        python3.13 python3.13-venv git coreutils procps \
        iproute2 dnsutils unzip zip xz-utils sudo tzdata groff-base \
        binutils cpp build-essential python3.13-dev libssl-dev libffi-dev \
        libasound2t64 libatk-bridge2.0-0t64 libatk1.0-0t64 libatspi2.0-0t64 \
        libcairo2 libcups2t64 libdbus-1-3 libdrm2 libgbm1 libglib2.0-0t64 \
        libnspr4 libnss3 libpango-1.0-0 libx11-6 libxcb1 libxcomposite1 \
        libxdamage1 libxext6 libxfixes3 libxkbcommon0 libxrandr2 \
        libfontconfig1 libfreetype6 fonts-liberation fonts-wqy-zenhei fonts-noto-color-emoji \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# pwntools 4.15 尚未完整支持 Python 3.14；任务环境使用 3.13。
RUN python3.13 -m venv /opt/xloom-venv \
    && ln -s /usr/bin/fdfind /usr/local/bin/fd
ENV PATH="/opt/xloom-venv/bin:${PATH}"

ARG PWNTOOLS_VERSION=4.15.0
ARG PYMONGO_VERSION=4.18.1
ARG AWSCLI_VERSION=1.46.1
RUN pip install --no-cache-dir \
        "pwntools==${PWNTOOLS_VERSION}" "pymongo==${PYMONGO_VERSION}" "awscli==${AWSCLI_VERSION}" \
    && pip check

# 单独隔离 tccli 的 SDK 依赖，避免与任务追加的 Python 包互相影响。
ARG TCCLI_VERSION=3.1.173.1
RUN /usr/bin/python3.13 -m venv /opt/tccli-venv \
    && /opt/tccli-venv/bin/pip install --no-cache-dir "tccli==${TCCLI_VERSION}" \
    && /opt/tccli-venv/bin/pip check \
    && ln -s /opt/tccli-venv/bin/tccli /usr/local/bin/tccli

ARG ALIYUN_CLI_VERSION=3.5.1
ARG ALIYUN_CLI_SHA256=3a9a45b9c7899228074135612d2b7ebbf32abb97826d825787cb454cd3011bdc
RUN curl --fail --show-error --location --retry 3 \
        "https://github.com/aliyun/aliyun-cli/releases/download/v${ALIYUN_CLI_VERSION}/aliyun-cli-linux-${ALIYUN_CLI_VERSION}-amd64.tgz" \
        --output /tmp/aliyun.tgz \
    && printf '%s  %s\n' "$ALIYUN_CLI_SHA256" /tmp/aliyun.tgz | sha256sum --check - \
    && mkdir /tmp/aliyun-install \
    && tar -xzf /tmp/aliyun.tgz -C /tmp/aliyun-install \
    && install -m 0755 /tmp/aliyun-install/aliyun /usr/local/bin/aliyun \
    && rm -rf /tmp/aliyun.tgz /tmp/aliyun-install

# 按需解析 latest；初始化只发生在临时目录，不向项目注入配置或 skills。
RUN npm install --global --no-audit --no-fund @playwright/cli@latest \
    && mkdir /tmp/playwright-setup \
    && cd /tmp/playwright-setup \
    && playwright-cli install \
    && ln -s "$(node -e 'process.stdout.write(require(process.argv[1]).chromium.executablePath())' \
        "$(npm root -g)/@playwright/cli/node_modules/playwright-core")" /usr/local/bin/xloom-chromium \
    && cd / \
    && rm -rf /tmp/playwright-setup \
    && npm cache clean --force
ENV PLAYWRIGHT_MCP_EXECUTABLE_PATH=/usr/local/bin/xloom-chromium

# 保留手动使用的 kali 用户；Dispatcher 注入的私有任务文件仍由 root 读取。
RUN if ! id kali >/dev/null 2>&1; then useradd -m -s /bin/bash kali; fi \
    && printf 'kali ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/kali \
    && chmod 0440 /etc/sudoers.d/kali \
    && install -d -o kali -g kali /workspace \
    && ln -s /workspace /home/kali/workspace \
    && git -C /workspace init -q \
    && git config --system --add safe.directory /workspace

COPY container/environment.md container/check-worker.sh /usr/local/share/xloom/
# Kali 为 Nmap 设置的 CAP_NET_ADMIN 超出 Docker 默认权限，连 --version 也会无法执行。
# 去掉文件 capabilities，让默认 root 进程沿用容器已有权限。
RUN chmod 0755 /usr/local/share/xloom/check-worker.sh \
    && if [ -f /usr/lib/nmap/nmap ] && [ -n "$(getcap /usr/lib/nmap/nmap)" ]; then \
        setcap -r /usr/lib/nmap/nmap; \
    fi
COPY --from=application /usr/local/bin/xloom /usr/local/bin/xloom

USER root
WORKDIR /workspace
RUN --network=none /usr/local/share/xloom/check-worker.sh
ENTRYPOINT ["/usr/local/bin/xloom"]
CMD ["worker", "--help"]
