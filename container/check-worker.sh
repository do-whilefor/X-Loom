#!/bin/sh
set -eu

. /etc/os-release
test "$ID" = kali
test "$(id -u)" = 0
test "$(getent passwd kali | cut -d: -f6)" = /home/kali
test "$(readlink -f /home/kali/workspace)" = /workspace
test "$TZ" = Asia/Shanghai
test "$PYTHONUNBUFFERED" = 1
for tool in bash curl wget rg fd python python3 pip pip3 jq git cat ps ip dig unzip zip sudo; do
    command -v "$tool" >/dev/null
done
su -s /bin/sh kali -c 'test -w /workspace && test "$(sudo -n id -u)" = 0'
# /workspace 是 git 仓库且 Worker 以 root 运行，而目录属主是 kali。
# 必须能真正执行 git 操作，否则依赖 git 的任务会以 128 (dubious ownership) 失败。
# 仅检查 git 命令存在无法覆盖这一点，故这里实际执行一次仓库操作。
git -C /workspace status --short >/dev/null
smoke_dir=$(mktemp -d /workspace/.xloom-smoke.XXXXXX)
trap 'rm -rf -- "$smoke_dir"' EXIT HUP INT TERM
file="$smoke_dir/probe.txt"
printf 'xloom-worker-smoke\n' > "$file"
rg -q '^xloom-worker-smoke$' "$file"
fd --hidden --no-ignore --type f '^probe\.txt$' "$smoke_dir" | grep -Fx "$file" >/dev/null
test "$(cat "/home/kali/workspace/${smoke_dir##*/}/probe.txt")" = xloom-worker-smoke
bash -c 'test "$BASH_VERSION"'
ip -j link show lo | jq -e 'any(.[]; .ifname == "lo")' >/dev/null
dig -v >/dev/null
pip --version
pip3 --version
python3 -m pip --version

# 仅连接回环地址，因此构建及 --network none 下的测试均无需外网。
python - "$smoke_dir" <<'PY'
import http.server
import importlib.metadata
import pathlib
import ssl
import subprocess
import sys
import threading
import venv

assert sys.prefix == "/opt/xloom-venv", sys.prefix
assert ssl.create_default_context().cert_store_stats()["x509_ca"] > 0
packages = {package.metadata["Name"].lower() for package in importlib.metadata.distributions()}
assert packages <= {"pip", "setuptools", "wheel"}, packages
venv_path = pathlib.Path(sys.argv[1]) / "venv"
venv.create(venv_path, with_pip=True)
subprocess.run([str(venv_path / "bin/python"), "-m", "pip", "--version"], check=True)


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"xloom-worker-smoke\n")

    def log_message(self, *args):
        pass


with http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    url = f"http://127.0.0.1:{server.server_port}/"
    try:
        for command in (
            ["curl", "--noproxy", "*", "--fail", "--silent", "--show-error", "--max-time", "5", url],
            ["wget", "--no-proxy", "--quiet", "--timeout=5", "--tries=1", "-O", "-", url],
        ):
            assert subprocess.check_output(command, timeout=10) == b"xloom-worker-smoke\n"
    finally:
        server.shutdown()
        thread.join()
PY

# 精简镜像不再预装安全工具、浏览器、知识库或项目 Agent 指令。
for tool in nmap nuclei npm playwright-cli aliyun; do
    if command -v "$tool" >/dev/null 2>&1; then
        printf 'Unexpected preinstalled tool: %s\n' "$tool" >&2
        exit 1
    fi
done
for path in /opt/ms-playwright /opt/nuclei-templates /home/kali/knowledges /home/kali/tools /home/kali/pocs /workspace/.agents /workspace/.claude /workspace/AGENTS.md /workspace/CLAUDE.md; do
    test ! -e "$path"
done
/usr/local/bin/xloom worker --help 2>&1 | grep -F -- '-job' >/dev/null
test -r /usr/local/share/xloom/environment.md
printf 'Kali Worker smoke passed: OS=%s user=%s workspace=%s\n' "$ID" "$(id -un)" "$PWD"
