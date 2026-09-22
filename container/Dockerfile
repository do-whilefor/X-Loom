# 在仓库根目录构建；先构建 xloom:dev，再构建本 Worker 镜像。
ARG XLOOM_IMAGE=xloom:dev
FROM --platform=linux/amd64 ${XLOOM_IMAGE} AS application

FROM --platform=linux/amd64 kalilinux/kali-rolling@sha256:30399bd65187e06525008dd13eecc2b3439d26a82b2c6b0ba32dee72bd843117 AS runtime

ARG KALI_MIRROR=
ENV TZ=Asia/Shanghai \
    PYTHONUNBUFFERED=1

# 可选镜像源只替换 URI，保留套件、组件和签名校验配置。
# 安装 ca-certificates 前使用 HTTP；软件包仍由 Kali keyring 验证。
RUN if [ -n "$KALI_MIRROR" ]; then \
        case "$KALI_MIRROR" in http://?*) ;; *) echo 'KALI_MIRROR must start with http://' >&2; exit 1 ;; esac; \
        awk -v mirror="$KALI_MIRROR" '$1 == "URIs:" {$0 = "URIs: " mirror} {print}' \
            /etc/apt/sources.list.d/kali.sources > /tmp/kali.sources \
        && mv /tmp/kali.sources /etc/apt/sources.list.d/kali.sources; \
    fi \
    && apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        bash ca-certificates curl wget ripgrep fd-find \
        python3 python3-pip python3-venv jq git coreutils procps \
        iproute2 dnsutils unzip zip sudo tzdata \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Python/pip 使用独立虚拟环境，供任务按需安装依赖。
RUN python3 -m venv /opt/xloom-venv \
    && ln -s /usr/bin/fdfind /usr/local/bin/fd
ENV PATH="/opt/xloom-venv/bin:${PATH}"

# 保留手动使用的 kali 用户；Dispatcher 注入的私有任务文件仍由 root 读取。
RUN if ! id kali >/dev/null 2>&1; then useradd -m -s /bin/bash kali; fi \
    && printf 'kali ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/kali \
    && chmod 0440 /etc/sudoers.d/kali \
    && install -d -o kali -g kali /workspace \
    && ln -s /workspace /home/kali/workspace \
    && git -C /workspace init -q \
    && git config --system --add safe.directory /workspace

COPY container/environment.md container/check-worker.sh /usr/local/share/xloom/
RUN chmod 0755 /usr/local/share/xloom/check-worker.sh
COPY --from=application /usr/local/bin/xloom /usr/local/bin/xloom

USER root
WORKDIR /workspace
RUN /usr/local/share/xloom/check-worker.sh
ENTRYPOINT ["/usr/local/bin/xloom"]
CMD ["worker", "--help"]
