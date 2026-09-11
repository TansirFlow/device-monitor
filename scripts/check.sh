#!/usr/bin/env sh
# 不依赖 make 的检查脚本：静态检查 + 安全审计 + 前端渲染冒烟。
# 建议接到 CI 上，任何一次提交都跑。

set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
FAIL=0

echo "==> 1/5 go vet"
( cd "$ROOT/agent"  && go vet ./... ) || FAIL=1
( cd "$ROOT/master" && go vet ./... ) || FAIL=1

echo ""
echo "==> 2/5 单元测试"
( cd "$ROOT/agent"  && go test ./... ) || FAIL=1
( cd "$ROOT/master" && go test ./... ) || FAIL=1

echo ""
echo "==> 3/5 安全审计"
sh "$ROOT/scripts/security-audit.sh" || FAIL=1

echo ""
echo "==> 4/5 看板渲染冒烟"
if command -v node >/dev/null 2>&1; then
  # 用相对路径调用：Git Bash 下把 POSIX 绝对路径交给原生 node.exe 会被错误拼接
  ( cd "$ROOT" && node scripts/render-smoke.mjs ) || FAIL=1
else
  echo "  (未检测到 node，跳过)"
fi

echo ""
echo "==> 5/5 PowerShell 脚本检查（BOM 与语法）"
if command -v powershell.exe >/dev/null 2>&1; then
  # 同样是相对路径：原生的 powershell.exe 不认 /c/... 形式的 POSIX 路径
  ( cd "$ROOT" && powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass \
      -File scripts/check-ps1.ps1 ) || FAIL=1
else
  echo "  (未检测到 powershell.exe，跳过)"
fi

echo ""
if [ "$FAIL" -eq 0 ]; then
  echo "全部检查通过。"
else
  echo "存在失败项。"
fi
exit $FAIL
