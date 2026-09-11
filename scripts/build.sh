#!/usr/bin/env sh
# 不依赖 make 的构建脚本（Windows 的 Git Bash 通常没有 make）。
#
#   sh scripts/build.sh              构建 5 个平台到 dist/
#   sh scripts/build.sh linux        只构建 linux/amd64
#   VERSION=1.2.0 sh scripts/build.sh

set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DIST="$ROOT/dist"
VERSION="${VERSION:-1.0.0}"
BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
ONLY="${1:-}"

export CGO_ENABLED=0
LDFLAGS="-s -w -X main.version=$VERSION -X main.buildTime=$BUILD_TIME"

mkdir -p "$DIST"

# 注意：go.exe 是原生 Windows 程序，不认 Git Bash 的 /c/... 形式路径，
# 因此这里一律以「项目目录为基准的相对路径」传给 go build。
# 传入 POSIX 绝对路径会得到难以理解的 "No such file or directory"。
build_one() {
  proj="$1"; os="$2"; arch="$3"
  ext=""
  [ "$os" = "windows" ] && ext=".exe"
  name="mon-$proj-$os-$arch$ext"
  printf '  %-40s' "$name"
  ( cd "$ROOT/$proj" && GOOS="$os" GOARCH="$arch" \
      go build -trimpath -ldflags "$LDFLAGS" -o "../dist/$name" . )
  size=$(wc -c < "$DIST/$name")
  awk -v s="$size" 'BEGIN{printf "%6.2f MB\n", s/1048576}'
}

if [ -n "$ONLY" ]; then
  case "$ONLY" in
    linux)  PLATFORMS="linux/amd64 linux/arm64" ;;
    windows) PLATFORMS="windows/amd64" ;;
    darwin) PLATFORMS="darwin/amd64 darwin/arm64" ;;
    *) echo "未知平台：$ONLY（可选 linux|windows|darwin）"; exit 1 ;;
  esac
else
  PLATFORMS="linux/amd64 linux/arm64 windows/amd64 darwin/amd64 darwin/arm64"
fi

echo "==> 构建 mon-agent / mon-master（version=$VERSION）"
for p in $PLATFORMS; do
  build_one agent  "${p%/*}" "${p#*/}"
  build_one master "${p%/*}" "${p#*/}"
done
echo "==> 完成，产物在 dist/"
