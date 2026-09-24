#!/bin/sh
set -eu

. /etc/os-release
test "$ID" = kali
test "$(id -u)" = 0
test "$(getent passwd kali | cut -d: -f6)" = /home/kali
test "$(readlink -f /home/kali/workspace)" = /workspace
test "$TZ" = Asia/Shanghai
test "$PYTHONUNBUFFERED" = 1
test -s /etc/ssl/certs/ca-certificates.crt
for package in ca-certificates kali-linux-headless bsdextrautils iputils-ping sshpass ncat rlwrap yq krb5-user adb nodejs npm jq ripgrep fd-find; do
    dpkg-query -W -f '${Status}\n' "$package" | grep -Fx 'install ok installed' >/dev/null
done
for tool in bash curl wget rg fd python python3 pip pip3 jq git cat ps ip dig unzip zip sudo as objcopy cpp aws tccli aliyun node npm playwright-cli \
    column hexdump ping sshpass ncat rlwrap yq kinit klist adb nmap sqlmap; do
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
ping -n -c 1 -W 2 127.0.0.1 >/dev/null
dig -v >/dev/null
pip --version
pip3 --version
python3 -m pip --version
python3 -m pip check
/opt/tccli-venv/bin/python -m pip check
test "$(readlink -f "$(command -v tccli)")" = /opt/tccli-venv/bin/tccli
node -e 'if (Number(process.versions.node.split(".")[0]) < 20) process.exit(1)'
test "$PLAYWRIGHT_MCP_BROWSER" = chromium
test "$PLAYWRIGHT_MCP_HEADLESS" = true
test "$PLAYWRIGHT_MCP_SANDBOX" = false
test "$PLAYWRIGHT_MCP_EXECUTABLE_PATH" = /usr/local/bin/xloom-chromium
test -x "$PLAYWRIGHT_MCP_EXECUTABLE_PATH"
test "$PLAYWRIGHT_BROWSERS_PATH" = /opt/ms-playwright
test -d "$PLAYWRIGHT_BROWSERS_PATH"

# 仅连接回环地址，因此构建及 --network none 下的测试均无需外网。
python - "$smoke_dir" <<'PY'
import http.server
import importlib.metadata
import os
import pathlib
import re
import ssl
import subprocess
import sys
import threading
import venv

assert sys.prefix == "/opt/xloom-venv", sys.prefix
assert sys.version_info[:2] == (3, 13), sys.version
assert ssl.create_default_context().cert_store_stats()["x509_ca"] > 0
smoke_dir = pathlib.Path(sys.argv[1])
os.environ["XDG_CACHE_HOME"] = str(smoke_dir / ".cache")
os.environ["PWNLIB_NOTERM"] = "1"

# Exercise the added text tools and YAML bridge with local fixture bytes.
columns = subprocess.check_output(
    ["column", "-t", "-s", ","], input="name,value\nxloom,42\n", text=True, timeout=10,
).splitlines()
assert [line.split() for line in columns] == [["name", "value"], ["xloom", "42"]], columns
assert columns[0].index("value") == columns[1].index("42"), columns
assert subprocess.check_output(
    ["hexdump", "-v", "-e", '1/1 "%02x"'], input=b"\x00AB\xff", timeout=10,
) == b"004142ff"
assert subprocess.check_output(
    ["yq", "-r", ".service.name"], input="service:\n  name: xloom-worker\n", text=True, timeout=10,
).strip() == "xloom-worker"

# Local version paths only: no scans, SSH login, Kerberos tickets or ADB daemon.
for command, label in (
    (["nmap", "--version"], "nmap"),
    (["sshpass", "-V"], "sshpass"),
    (["ncat", "--version"], "ncat"),
    (["rlwrap", "--version"], "rlwrap"),
    (["klist", "-V"], "kerberos"),
    (["adb", "version"], "android debug bridge"),
):
    output = subprocess.check_output(command, stderr=subprocess.STDOUT, text=True, timeout=15)
    assert label in output.lower(), (command, output)

versions = {package: importlib.metadata.version(package) for package in ("pwntools", "pymongo", "awscli")}
for package, version in versions.items():
    assert version, package
    print(f"{package} {version}")
tccli_version = subprocess.check_output(
    ["/opt/tccli-venv/bin/python", "-c", "import importlib.metadata; print(importlib.metadata.version('tccli'))"],
    text=True, timeout=10,
).strip()
assert tccli_version, "tccli has no installed version"

# Resolve Requests' effective CA bundle without preparing or sending a request.
requests_ca_check = """import requests
with requests.Session() as session:
    settings = session.merge_environment_settings('https://example.invalid', {}, None, None, None)
assert settings['verify'] == '/etc/ssl/certs/ca-certificates.crt', settings['verify']
"""
for interpreter in (sys.executable, "/opt/tccli-venv/bin/python"):
    subprocess.run([interpreter, "-c", requests_ca_check], check=True, timeout=10)

# Exercise native assembly and encoding without a target process or service.
from pwn import asm, context, cyclic, cyclic_find
from pwnlib.util.safeeval import const
assert const("1") == 1
assert const("[1, 2, 3]") == [1, 2, 3]
with context.local(arch="amd64", os="linux", log_level="error"):
    assert asm("xor eax, eax; ret") == b"\x31\xc0\xc3"
    pattern = cyclic(64)
    assert cyclic_find(pattern[24:28]) == 24

import pymongo
from bson import BSON, ObjectId
document = {"_id": ObjectId("0123456789abcdef01234567"), "count": 3, "tags": ["xloom", "离线"]}
assert BSON(BSON.encode(document)).decode() == document
assert pymongo.version == versions["pymongo"], pymongo.version

# Version commands do not require cloud credentials or make service requests.
for command, version in (
    (["aws", "--version"], "aws-cli/" + versions["awscli"]),
    (["tccli", "--version"], tccli_version),
):
    output = subprocess.check_output(command, stderr=subprocess.STDOUT, text=True, timeout=15)
    assert version in output, (command, output)
    print(output.strip())
aws_help = subprocess.check_output(
    ["aws", "help"], stderr=subprocess.STDOUT, text=True, timeout=30,
    env=dict(os.environ, MANPAGER="cat", PAGER="cat", AWS_EC2_METADATA_DISABLED="true"),
)
# groff's terminal output may encode bold/underlining with backspace overstrikes.
aws_help = re.sub(r".\x08", "", aws_help)
assert "SYNOPSIS" in aws_help, "AWS CLI help did not render its synopsis"
aliyun_version = subprocess.check_output(["aliyun", "version"], text=True, timeout=15).strip()
assert re.fullmatch(r"\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?", aliyun_version), aliyun_version
print("aliyun " + aliyun_version)
subprocess.run(["playwright-cli", "--version"], check=True, timeout=15)

venv_path = smoke_dir / "venv"
venv.create(venv_path, with_pip=True)
subprocess.run([str(venv_path / "bin/python"), "-m", "pip", "--version"], check=True)


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8" if self.path == "/browser" else "text/plain")
        self.end_headers()
        if self.path == "/browser":
            self.wfile.write(b'<!doctype html><title>X-Loom browser smoke</title><p id="result">pending</p>'
                            b'<script>document.querySelector("#result").textContent = "chromium-script-ran";</script>')
        else:
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
        session = "xloom-smoke-" + str(os.getpid())
        cli = ["playwright-cli", "-s=" + session]
        # Use the installed CLI and its Chromium defaults, not a separate Node API.
        # Its run-code command exits nonzero when either browser assertion fails.
        try:
            subprocess.run(cli + ["open", url + "browser"], cwd=smoke_dir, check=True, timeout=45)
            subprocess.run(cli + ["run-code", """async page => {
                if ((await page.title()) !== 'X-Loom browser smoke') throw new Error('browser title mismatch');
                if ((await page.locator('#result').innerText()) !== 'chromium-script-ran') throw new Error('page script did not execute');
            }"""], cwd=smoke_dir, check=True, timeout=20)
        finally:
            subprocess.run(cli + ["close"], cwd=smoke_dir, check=True, timeout=20)
    finally:
        server.shutdown()
        thread.join()
PY

# 不恢复原竞赛环境的额外知识库或项目 Agent 指令。
for path in /home/kali/knowledges /home/kali/tools /home/kali/pocs /workspace/.agents /workspace/.claude /workspace/AGENTS.md /workspace/CLAUDE.md; do
    test ! -e "$path"
done
/usr/local/bin/xloom worker --help 2>&1 | grep -F -- '-job' >/dev/null
test -r /usr/local/share/xloom/environment.md
printf 'Kali Worker smoke passed: OS=%s user=%s workspace=%s\n' "$ID" "$(id -un)" "$PWD"
