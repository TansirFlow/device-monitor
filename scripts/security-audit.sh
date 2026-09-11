#!/usr/bin/env sh
# 结构性安全约束的自动化审计。
#
# 这些约束不是靠"当时写得小心"，而是靠每次提交都跑一遍来保证。
# 任何一条失败都意味着「主控不可能操作节点」这个前提被破坏了。
#
# 退出码：0 = 全部通过，1 = 存在违反项。

set -u

ROOT=$(cd "$(dirname "$0")/.." && pwd)
AGENT="$ROOT/agent"
MASTER="$ROOT/master"
FAIL=0

pass() { printf '  [通过] %s\n' "$1"; }
viol() { printf '  [违反] %s\n' "$1"; FAIL=1; }

# 审计范围只包含会进二进制的源码：*_test.go 必须排除。
#  - 测试代码不会编译进发布产物（go build 忽略 _test.go），
#    所以它写文件、起临时目录都不构成对"agent 不写文件"的破坏；
#  - 反过来，必须存在的防线（must_contain）也不能被测试文件里的字符串满足，
#    否则一句注释或一个测试常量就能让审计误判为通过。
GREP_SRC="--include=*.go --exclude=*_test.go"

# 断言：dir 下的 .go 文件中不得出现 pattern
must_not_contain() {
  desc="$1"; pattern="$2"; dir="$3"
  hits=$(grep -rnE "$pattern" "$dir" $GREP_SRC 2>/dev/null || true)
  if [ -n "$hits" ]; then
    viol "$desc"
    printf '%s\n' "$hits" | sed 's/^/         /'
  else
    pass "$desc"
  fi
}

# 断言：pattern 必须存在（防止有人"顺手删掉"了关键防线）
must_contain() {
  desc="$1"; pattern="$2"; dir="$3"
  hits=$(grep -rnE "$pattern" "$dir" $GREP_SRC 2>/dev/null || true)
  if [ -n "$hits" ]; then
    pass "$desc"
  else
    viol "$desc"
  fi
}

# 断言：pattern 只允许出现在白名单文件里
only_in() {
  desc="$1"; pattern="$2"; dir="$3"; allowed="$4"
  hits=$(grep -rlE "$pattern" "$dir" $GREP_SRC 2>/dev/null || true)
  bad=""
  for f in $hits; do
    case "$f" in
      */$allowed) ;;
      *) bad="$bad $f" ;;
    esac
  done
  if [ -n "$bad" ]; then
    viol "$desc"
    for f in $bad; do printf '         多余出现: %s\n' "$f"; done
  else
    pass "$desc"
  fi
}

printf '== 结构性约束审计 ==\n\n'

printf -- '-- agent：必须是一个纯粹的单向上报端 --\n'
must_not_contain 'agent 不得监听任何端口（否则主控就有了连入点）' \
  '\b(net\.Listen|ListenAndServe|ListenAndServeTLS|ListenPacket)\b' "$AGENT"
must_not_contain 'agent 不得注册 HTTP 路由或服务端处理器（没有可被调用的接口面）' \
  '\b(http\.Handle|http\.HandleFunc|ServeMux|http\.Server)\b' "$AGENT"
must_not_contain 'agent 不得删除文件或目录' \
  '\b(os\.RemoveAll|os\.Remove|syscall\.Unlink)\b' "$AGENT"
must_not_contain 'agent 不得写文件（情报只存在于内存和出站请求里）' \
  '\b(os\.Create|os\.WriteFile|ioutil\.WriteFile|os\.OpenFile)\b' "$AGENT"
must_not_contain 'agent 不得解析响应体（响应只用于判断 2xx/非 2xx）' \
  '\bjson\.NewDecoder\(resp\.Body\)|json\.Unmarshal\([a-zA-Z]*, resp' "$AGENT"
only_in '外部命令调用只允许出现在 GPU 采集器里（且是固定参数向量）' \
  '\b(os/exec|exec\.Command)\b' "$AGENT" "gpu.go"
only_in '出站网络只允许出现在 sender.go' \
  '\b(net/http|net\.Dial|tls\.Dial)\b' "$AGENT" "sender.go"
must_contain 'agent 必须拒绝跟随意外重定向（防令牌与指标外泄）' \
  'ErrUseLastResponse' "$AGENT"
must_contain 'agent 必须对 node_id 做白名单校验（防路径穿越）' \
  'nodeIDRe\.MatchString' "$AGENT"
must_contain 'agent 必须区分 http/https 的明文告警' \
  'MON_ALLOW_INSECURE_HTTP' "$AGENT"

printf '\n-- master：必须没有任何通往节点的手段 --\n'
must_not_contain 'master 不得执行任何外部命令' \
  '\b(os/exec|exec\.Command)\b' "$MASTER"
must_not_contain 'master 不得主动发起任何网络连接（只能被动应答）' \
  '\b(http\.Get|http\.Post|http\.Head|http\.NewRequest|net\.Dial|tls\.Dial)\b' "$MASTER"
must_not_contain 'master 不得删除整个目录' \
  '\bos\.RemoveAll\b' "$MASTER"
must_contain 'master 必须对 node_id 做白名单校验（防路径穿越）' \
  'nodeIDRe\.MatchString' "$MASTER"
must_contain 'master 必须做令牌恒定时间比较（防时序侧信道）' \
  'ConstantTimeCompare' "$MASTER"
must_contain 'master 必须校验写入路径不越出数据目录' \
  '\bwithin\(' "$MASTER"
must_contain 'master 必须限制上报请求体大小' \
  'LimitReader' "$MASTER"
must_contain 'master 必须限制温度指标名的字符集（不允许把任意字符串当指标查询）' \
  '\btempMetricKind\(' "$MASTER"

printf '\n-- master 管理后台：账号 / 令牌 / 一键安装 --\n'
must_contain '管理端写接口必须统一走「会话 + CSRF」校验（Cookie 认证不能直接信任）' \
  'requireAdminWrite' "$MASTER"
must_contain '管理端必须做 CSRF 防护（自定义请求头，浏览器表单伪造不了）' \
  'csrfHeader' "$MASTER"
must_contain '会话 Cookie 必须 HttpOnly（XSS 拿不到会话）' \
  'HttpOnly: true' "$MASTER"
must_contain '账号与令牌落盘必须走原子写（写坏就等于把后台锁死）' \
  'writeFileAtomic' "$MASTER"
must_contain '一键安装脚本必须凭节点 + 安装码换取（不能被匿名拉取）' \
  'validCode' "$MASTER"
must_contain '口令派生必须带随机盐且用恒定时间比较' \
  'subtle\.ConstantTimeCompare' "$MASTER"
must_not_contain '主控不得改写自己的配置文件（令牌只落 data_dir，配置保持只读）' \
  '(saveConfig|writeConfig|MarshalIndent\(s\.cfg)' "$MASTER"

printf '\n'
if [ "$FAIL" -eq 0 ]; then
  printf '结论：全部通过 —— 主控在架构上不具备操作节点服务器的手段。\n'
else
  printf '结论：存在违反项，请修复后再发布。\n'
fi
exit $FAIL
