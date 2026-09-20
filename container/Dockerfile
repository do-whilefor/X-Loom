# Build the Go application first: docker build -t xloom:dev .
# Keep the application copy last so Go changes reuse the Kali package layer.
ARG XLOOM_IMAGE=xloom:dev
FROM ${XLOOM_IMAGE} AS application

FROM kalilinux/kali-rolling@sha256:30399bd65187e06525008dd13eecc2b3439d26a82b2c6b0ba32dee72bd843117
ENV TZ=Asia/Shanghai \
    PYTHONUNBUFFERED=1
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       bash ca-certificates curl ripgrep fd-find python3 python3-pip jq git \
       coreutils procps iproute2 dnsutils unzip zip sudo tzdata \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
RUN useradd -m -s /bin/bash kali \
    && printf 'kali ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/kali \
    && chmod 0440 /etc/sudoers.d/kali \
    && install -d -o kali -g kali /workspace \
    && ln -s /workspace /home/kali/workspace \
    && ln -s /usr/bin/fdfind /usr/local/bin/fd
COPY container/environment.md container/check-worker.sh /usr/local/share/xloom/
RUN chmod 0755 /usr/local/share/xloom/check-worker.sh
COPY --from=application /usr/local/bin/xloom /usr/local/bin/xloom
# Dispatcher currently injects root-owned private job/session files. Preserve
# that execution contract; the kali user remains available for manual commands.
USER root
WORKDIR /workspace
RUN /usr/local/share/xloom/check-worker.sh
ENTRYPOINT ["/usr/local/bin/xloom"]
CMD ["worker", "--help"]
