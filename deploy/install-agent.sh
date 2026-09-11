#!/usr/bin/env sh
# mon-agent 一键安装脚本（Linux + systemd）
#
# 用法：
#   sudo MON_TOKEN=<令牌> sh install-agent.sh \
#        --node-id web-01 \
#        --master-url https://monitor.example.com/api/v1/report
#
#   sudo sh install-agent.sh --uninstall
#
# 为什么令牌推荐用环境变量而不是 --token：
#   --token 会出现在 `ps` 里（同机任何用户都能看到）以及 shell 历史里；
#   环境变量只对 root 与进程属主可读。两者都不给时脚本会交互式索要（不回显）。
#
# 这个脚本只做四件事：建用户、放二进制、写配置、装并启动 systemd 服务。
# 它不下载任何东西、不访问网络、不需要主控在线。

set -eu

# ---------------------------------------------------------------- 常量

SVC=mon-agent
RUN_USER=mon-agent
BIN_DST=/usr/local/bin/mon-agent
CFG_DIR=/etc/mon-agent
CFG="$CFG_DIR/agent.json"
UNIT="/etc/systemd/system/$SVC.service"
DROPIN_DIR="/etc/systemd/system/$SVC.service.d"
SHARE=/usr/local/share/mon-agent

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
HAVE_SYSTEMCTL=0
if command -v systemctl >/dev/null 2>&1; then
  HAVE_SYSTEMCTL=1
fi

# ---------------------------------------------------------------- 输出

say()  { printf '  %s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
warn() { printf '  [注意] %s\n' "$*" >&2; }
die()  { printf '\n[错误] %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
mon-agent 安装脚本

  --node-id <名字>        本节点的唯一名字，仅允许字母数字 . _ -，1-64 字符
  --master-url <URL>      主控上报地址，如 https://monitor.example.com/api/v1/report
  --token <令牌>          上报令牌（建议改用环境变量 MON_TOKEN，避免出现在 ps 里）
  --interval <秒>         上报间隔，默认 10（范围 1-3600）
  --labels <k=v,k2=v2>    看板分组标签，如 env=prod,zone=cn-hz-a,role=web
  --binary <路径>         指定 agent 二进制；默认按架构从 ../dist/ 自动找
  --sha256 <十六进制>     校验二进制的 SHA256，不匹配则中止
  --no-gpu                关闭 GPU 采集
  --allow-insecure-http   允许明文 http 上报（会泄露令牌，仅限可信内网）
  --no-start              装好但不启动服务
  --force                 已存在配置时覆盖重写
  --dry-run               只打印将要执行的动作，不落盘

  --uninstall             卸载服务与二进制（保留配置，便于重装）
  -h, --help              显示本帮助
EOF
}

# ---------------------------------------------------------------- 参数

NODE_ID=""
MASTER_URL=""
TOKEN="${MON_TOKEN:-}"
INTERVAL=""
LABELS=""
BINARY=""
SHA256=""
ENABLE_GPU=1
ALLOW_HTTP=0
DO_START=1
FORCE=0
DRY=0
UNINSTALL=0

need_val() {
  # $1 = 参数名, $2 = 取值（可能为空）
  #
  # 顺带挡住 `--node-id --force` 这种写法：不加判断的话会把下一个选项
  # 当成取值吞掉，结果 node_id 变成 "--force"，而 --force 本身静默失效。
  case "${2:-}" in
    "")  die "参数 $1 后面缺少取值" ;;
    --*) die "参数 $1 后面看起来是另一个选项（$2），缺少取值" ;;
  esac
}

while [ $# -gt 0 ]; do
  case "$1" in
    --node-id)             need_val "$1" "${2:-}"; NODE_ID="$2";    shift 2 ;;
    --master-url)          need_val "$1" "${2:-}"; MASTER_URL="$2"; shift 2 ;;
    --token)               need_val "$1" "${2:-}"; TOKEN="$2";      shift 2 ;;
    --interval)            need_val "$1" "${2:-}"; INTERVAL="$2";   shift 2 ;;
    --labels)              need_val "$1" "${2:-}"; LABELS="$2";     shift 2 ;;
    --binary)              need_val "$1" "${2:-}"; BINARY="$2";     shift 2 ;;
    --sha256)              need_val "$1" "${2:-}"; SHA256="$2";     shift 2 ;;
    --no-gpu)              ENABLE_GPU=0;   shift ;;
    --allow-insecure-http) ALLOW_HTTP=1;   shift ;;
    --no-start)            DO_START=0;     shift ;;
    --force)               FORCE=1;        shift ;;
    --dry-run)             DRY=1;          shift ;;
    --uninstall)           UNINSTALL=1;    shift ;;
    -h|--help)             usage; exit 0 ;;
    *)                     die "未知参数：$1（用 --help 查看用法）" ;;
  esac
done

# 所有动作都写系统目录，必须先提权。
# --dry-run 例外：它什么都不落盘，因此不需要 root（也方便先看一眼会发生什么）。
if [ "$(id -u)" -ne 0 ] && [ "$DRY" -ne 1 ]; then
  die "请用 root 运行：sudo sh $0 --help"
fi

run() {
  # dry-run 下只打印，不真的执行。
  # 注意：调用处不要写 `[ 条件 ] && run ...`，条件为假时整个语句返回非零，
  # 在 set -e 下会直接退出脚本。一律用 if 包起来。
  if [ "$DRY" -eq 1 ]; then
    printf '  [dry-run] %s\n' "$*"
  else
    "$@"
  fi
}

# 声明"某个动作已完成"。dry-run 下必须换一种说法 ——
# 否则预演会打印"已创建系统用户""权限已设为 600"这类没发生的事。
done_say() {
  if [ "$DRY" -eq 1 ]; then
    say "未执行（dry-run 预演）"
  else
    say "$*"
  fi
}

# ---------------------------------------------------------------- 卸载

if [ "$UNINSTALL" -eq 1 ]; then
  step "卸载 $SVC"

  if [ "$HAVE_SYSTEMCTL" -eq 1 ]; then
    say "停止并禁用服务"
    run systemctl stop "$SVC"    2>/dev/null || true
    run systemctl disable "$SVC" 2>/dev/null || true
  fi

  say "删除 systemd 单元与 drop-in"
  run rm -f "$UNIT"
  run rm -rf "$DROPIN_DIR"
  if [ "$HAVE_SYSTEMCTL" -eq 1 ]; then
    run systemctl daemon-reload
  fi

  say "删除二进制与文档"
  run rm -f "$BIN_DST"
  run rm -rf "$SHARE"

  if id "$RUN_USER" >/dev/null 2>&1; then
    say "删除专用用户 $RUN_USER"
    run userdel "$RUN_USER" || true
  fi

  printf '\n'
  done_say "已卸载。配置**故意保留**在 $CFG（里面是上报令牌，重装可直接复用）。"
  say "确认不再需要后自行删除：rm -rf $CFG_DIR"
  exit 0
fi

# ---------------------------------------------------------------- 校验

step "校验参数"

if [ -z "$NODE_ID" ]; then
  die "缺少 --node-id（每台机器必须唯一）"
fi
if ! printf '%s' "$NODE_ID" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'; then
  die "node_id \"$NODE_ID\" 非法：仅允许 [A-Za-z0-9._-]，1-64 字符，且以字母或数字开头"
fi

if [ -z "$MASTER_URL" ]; then
  die "缺少 --master-url"
fi
case "$MASTER_URL" in
  https://*) ;;
  http://*)
    if [ "$ALLOW_HTTP" -ne 1 ]; then
      die "master_url 是明文 http，会泄露上报令牌。
       确认在可信内网中，请加 --allow-insecure-http 显式确认"
    fi
    warn "已允许明文 http：令牌将以明文经过网络，请确保链路可信"
    ;;
  *) die "master_url 必须以 http:// 或 https:// 开头" ;;
esac

if [ -z "$TOKEN" ] && [ -t 0 ]; then
  printf '  请输入上报令牌（输入时不回显）: '
  stty -echo 2>/dev/null || true
  read -r TOKEN || true
  stty echo 2>/dev/null || true
  printf '\n'
fi
if [ -z "$TOKEN" ]; then
  die "缺少上报令牌：请设置环境变量 MON_TOKEN，或用 --token（会出现在 ps 里）"
fi
if [ "${#TOKEN}" -lt 12 ]; then
  die "令牌过短（至少 12 字符），建议 openssl rand -hex 24"
fi

if [ -z "$INTERVAL" ]; then
  INTERVAL=10
fi
case "$INTERVAL" in
  *[!0-9]*) die "--interval 必须是整数" ;;
esac
if [ "$INTERVAL" -lt 1 ] || [ "$INTERVAL" -gt 3600 ]; then
  die "--interval 必须在 1-3600 之间"
fi

say "node_id    : $NODE_ID"
say "master_url : $MASTER_URL"
say "interval   : ${INTERVAL}s"
if [ -n "$LABELS" ]; then
  say "labels     : $LABELS"
fi
say "令牌       : 已提供（${#TOKEN} 字符，不显示内容）"

# ---------------------------------------------------------------- 二进制

step "定位 agent 二进制"

ARCH=""
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
esac
if [ -z "$ARCH" ]; then
  die "不支持的架构：$(uname -m)（目前只提供 amd64 与 arm64）"
fi

if [ -n "$BINARY" ]; then
  BIN_SRC="$BINARY"
else
  BIN_SRC="$SCRIPT_DIR/../dist/mon-agent-linux-$ARCH"
fi

if [ ! -f "$BIN_SRC" ]; then
  die "找不到二进制：$BIN_SRC
       用 --binary <路径> 指定，或从解压后的 release 目录里运行本脚本"
fi
say "使用 $BIN_SRC（$(uname -m)）"

if [ -n "$SHA256" ]; then
  if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL=$(sha256sum "$BIN_SRC" | awk '{print $1}')
    if [ "$ACTUAL" != "$SHA256" ]; then
      die "SHA256 校验失败
       期望 $SHA256
       实际 $ACTUAL"
    fi
    say "SHA256 校验通过"
  else
    warn "系统没有 sha256sum，跳过校验"
  fi
fi

# 先验一次二进制能否运行，避免装完才发现拷错了架构。
# dry-run 下不执行任何东西（也可能是在另一台机器上预览），只提示。
if [ "$DRY" -eq 1 ]; then
  printf '  [dry-run] %s -version\n' "$BIN_SRC"
elif ! "$BIN_SRC" -version >/dev/null 2>&1; then
  die "二进制无法执行：$BIN_SRC（架构不对或文件损坏）"
fi

# ---------------------------------------------------------------- 安装

step "1/5 创建专用低权限用户"
if id "$RUN_USER" >/dev/null 2>&1; then
  say "用户 $RUN_USER 已存在，跳过"
else
  run useradd --system --no-create-home --shell /usr/sbin/nologin "$RUN_USER"
  done_say "已创建系统用户 $RUN_USER（不可登录、无家目录）"
fi

# 读 GPU 指标（nvidia-smi）通常需要 video 组；组不存在就跳过
if [ "$ENABLE_GPU" -eq 1 ] && getent group video >/dev/null 2>&1; then
  run usermod -aG video "$RUN_USER" || true
  done_say "已加入 video 组（用于读取 GPU 指标）"
fi

step "2/5 安装二进制到 $BIN_DST"
# 不假设 /usr/local/bin 一定存在：精简镜像里它可能真的没有
run mkdir -p "$(dirname "$BIN_DST")"
run install -m 0755 "$BIN_SRC" "$BIN_DST"
done_say "完成"

step "3/5 写入配置 $CFG"

if [ -f "$CFG" ] && [ "$FORCE" -ne 1 ]; then
  say "配置已存在，保持不变（要覆盖请加 --force）"
  CUR_ID=$(sed -n 's/.*"node_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CFG" | head -1)
  if [ -n "$CUR_ID" ]; then
    say "当前 node_id：$CUR_ID"
  fi
else
  # labels 从 k=v,k2=v2 转成 JSON 对象。键值只允许安全字符，
  # 免得有人顺手塞进引号把 JSON 结构破坏掉。
  LABELS_JSON="{}"
  if [ -n "$LABELS" ]; then
    LABELS_JSON="{"
    OLD_IFS=$IFS
    IFS=','
    FIRST=1
    for KV in $LABELS; do
      K=${KV%%=*}
      V=${KV#*=}
      if [ "$K" = "$KV" ]; then
        IFS=$OLD_IFS
        die "--labels 格式应为 k=v,k2=v2，收到：$KV"
      fi
      case "$K$V" in
        *[!A-Za-z0-9._:@/-]*)
          IFS=$OLD_IFS
          die "标签 $KV 含不安全字符（仅允许字母数字 . _ : @ / -）" ;;
      esac
      if [ "$FIRST" -eq 0 ]; then
        LABELS_JSON="$LABELS_JSON, "
      fi
      LABELS_JSON="$LABELS_JSON\"$K\": \"$V\""
      FIRST=0
    done
    IFS=$OLD_IFS
    LABELS_JSON="$LABELS_JSON}"
  fi

  GPU_JSON=true
  if [ "$ENABLE_GPU" -eq 0 ]; then
    GPU_JSON=false
  fi

  run mkdir -p "$CFG_DIR"
  if [ "$DRY" -eq 1 ]; then
    printf '  [dry-run] 写入 %s\n' "$CFG"
    printf '            node_id=%s interval=%s labels=%s gpu=%s\n' \
      "$NODE_ID" "$INTERVAL" "$LABELS_JSON" "$GPU_JSON"
  else
    # umask 077：配置文件从创建的第一刻起就只有 root 能读，
    # 不会出现"先 0644 再 chmod"中间那段全局可读的窗口。
    ( umask 077; cat > "$CFG" <<EOF
{
  "node_id": "$NODE_ID",
  "master_url": "$MASTER_URL",
  "token": "$TOKEN",
  "interval_sec": $INTERVAL,
  "timeout_sec": 8,
  "buffer_max": 120,
  "labels": $LABELS_JSON,
  "collect": {
    "cpu": true, "mem": true, "disk": true, "net": true, "gpu": $GPU_JSON,
    "temp": true, "temp_limit": 32, "temp_exclude": []
  },
  "gpu": { "enabled": $GPU_JSON, "binary": "nvidia-smi" }
}
EOF
    )
    say "已写入配置"
  fi
fi

# 无论新建还是复用，权限都要收紧：文件里有上报令牌
run chown root:root "$CFG"
run chmod 600 "$CFG"
done_say "权限已设为 600 root:root"

step "4/5 安装 systemd 单元"

# 单元文件是唯一事实来源，脚本不内嵌副本（避免两处内容跑偏）。
# 依次在脚本旁、上一级的 deploy/ 里找。
UNIT_SRC=""
for cand in "$SCRIPT_DIR/mon-agent.service" "$SCRIPT_DIR/../deploy/mon-agent.service"; do
  if [ -f "$cand" ]; then
    UNIT_SRC="$cand"
    break
  fi
done
if [ -z "$UNIT_SRC" ]; then
  die "找不到 systemd 单元文件 mon-agent.service。
       请在包含 deploy/ 目录的 release 目录里运行本脚本（当前脚本位于 $SCRIPT_DIR）"
fi
say "使用单元文件 $UNIT_SRC"
run mkdir -p "$(dirname "$UNIT")"
run install -m 0644 "$UNIT_SRC" "$UNIT"

# 明文 http 靠环境变量放行。写进 drop-in 而不是改单元文件，
# 这样以后升级单元文件不会把这一行冲掉。
if [ "$ALLOW_HTTP" -eq 1 ]; then
  run mkdir -p "$DROPIN_DIR"
  if [ "$DRY" -eq 1 ]; then
    printf '  [dry-run] 写入 drop-in：Environment=MON_ALLOW_INSECURE_HTTP=1\n'
  else
    ( umask 022; cat > "$DROPIN_DIR/insecure-http.conf" <<'EOF'
# 由 install-agent.sh --allow-insecure-http 写入。
# 允许 agent 用明文 http 上报：令牌会以明文经过网络，仅限可信内网。
[Service]
Environment=MON_ALLOW_INSECURE_HTTP=1
EOF
    )
    say "已写 drop-in 允许明文 http"
  fi
fi

run mkdir -p "$SHARE"
if [ -f "$SCRIPT_DIR/../SECURITY.md" ]; then
  run install -m 0644 "$SCRIPT_DIR/../SECURITY.md" "$SHARE/SECURITY.md"
fi

if [ "$HAVE_SYSTEMCTL" -eq 1 ]; then
  run systemctl daemon-reload
else
  warn "系统里没有 systemctl，稍后需要自行守护 mon-agent 进程"
fi

step "5/5 自检"

if [ "$DRY" -eq 1 ]; then
  printf '  [dry-run] %s -config %s -once\n' "$BIN_DST" "$CFG"
  printf '  [dry-run] %s -config %s -selftest\n' "$BIN_DST" "$CFG"
else
  # -once 只看采集；stderr 里才是降级/报错信息，因此只抓 stderr
  if out=$("$BIN_DST" -config "$CFG" -once 2>&1 >/dev/null); then
    say "采集正常（-once 通过）"
  else
    warn "采集自检未通过：$out"
  fi

  # -selftest 会真实往主控发一条，验证网络与鉴权全链路
  if out=$("$BIN_DST" -config "$CFG" -selftest 2>&1); then
    say "上报链路正常：$out"
  else
    warn "上报自检未通过：$out"
    say "  常见原因：master_url 写错、令牌不匹配、该节点未登记进主控的 node_tokens"
  fi
fi

if [ "$DO_START" -eq 1 ]; then
  if [ "$HAVE_SYSTEMCTL" -eq 1 ]; then
    run systemctl enable --now "$SVC"
  else
    warn "没有 systemctl，请手动启动：$BIN_DST -config $CFG"
  fi
else
  say "--no-start 已指定，未启动服务"
fi

printf '\n'
say "安装完成。常用命令："
say "  systemctl status $SVC"
say "  journalctl -u $SVC -f"
say "  $BIN_DST -config $CFG -selftest     # 重新验证链路"
say "  $BIN_DST -config $CFG -once         # 看一眼采集到的原始 JSON"
say "  改完配置后：systemctl restart $SVC"
