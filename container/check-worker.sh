#!/bin/sh
set -eu

. /etc/os-release
test "$ID" = kali
test "$(id -u)" = 0
test "$(getent passwd kali | cut -d: -f6)" = /home/kali
test "$(readlink -f /home/kali/workspace)" = /workspace
test "$TZ" = Asia/Shanghai
test "$PYTHONUNBUFFERED" = 1
for tool in bash curl rg fd python3 pip3 jq git cat ps ip dig unzip zip sudo; do
    command -v "$tool" >/dev/null
done
python3 -c 'import ssl; assert ssl.get_default_verify_paths().cafile'
sudo -l -U kali | grep -F 'NOPASSWD: ALL' >/dev/null
su -s /bin/sh kali -c 'test -w /workspace'
# /workspace 是 git 仓库且 Worker 以 root 运行，而目录属主是 kali。
# 必须能真正执行 git 操作，否则依赖 git 的任务会以 128 (dubious ownership) 失败。
# 仅检查 git 命令存在无法覆盖这一点，故这里实际执行一次仓库操作。
git -C /workspace status --short >/dev/null
file=$(mktemp /workspace/.xloom-smoke.XXXXXX)
trap 'rm -f -- "$file"' EXIT HUP INT TERM
printf 'xloom-worker-smoke\n' > "$file"
rg -q '^xloom-worker-smoke$' "$file"
test "$(cat "/home/kali/workspace/${file##*/}")" = xloom-worker-smoke
/usr/local/bin/xloom worker --help 2>&1 | grep -F -- '-job' >/dev/null
test -r /usr/local/share/xloom/environment.md
printf 'Kali Worker smoke passed: OS=%s user=%s workspace=%s\n' "$ID" "$(id -un)" "$PWD"
