# 服务器监控 · 构建入口
#
# 交叉编译 5 个平台，产物全部是静态单文件（CGO_ENABLED=0）。
# 用法：
#   make            构建全部产物到 dist/
#   make agent      只构建 agent
#   make check      静态检查 + 安全审计（CI 必跑）
#   make upx        额外用 upx 压缩（需自行安装 upx）

SHELL := /bin/sh

VERSION    ?= 1.0.0
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
DIST       ?= dist

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME)
BUILDFLAGS := -trimpath -ldflags "$(LDFLAGS)"

export CGO_ENABLED := 0

PLATFORMS := linux/amd64 linux/arm64 windows/amd64 darwin/amd64 darwin/arm64

.PHONY: all agent master check test clean size upx

all: agent master size

agent:
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		out="$(DIST)/mon-agent-$$os-$$arch$$ext"; \
		echo "  build $$out"; \
		( cd agent && GOOS=$$os GOARCH=$$arch go build $(BUILDFLAGS) -o "../$$out" . ) || exit 1; \
	done

master:
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		out="$(DIST)/mon-master-$$os-$$arch$$ext"; \
		echo "  build $$out"; \
		( cd master && GOOS=$$os GOARCH=$$arch go build $(BUILDFLAGS) -o "../$$out" . ) || exit 1; \
	done

# 静态检查 + 安全审计：审计脚本会检查"agent 不得监听端口""master 不得主动外连"
# 这类结构性约束，任何一次提交破坏它们都会在这里失败。
check:
	@echo "==> go vet"
	@( cd agent  && go vet ./... )
	@( cd master && go vet ./... )
	@echo "==> 单元测试"
	@( cd agent  && go test ./... )
	@( cd master && go test ./... )
	@echo "==> 安全审计"
	@sh scripts/security-audit.sh
	@echo "==> 看板渲染冒烟"
	@if command -v node >/dev/null 2>&1; then ( cd . && node scripts/render-smoke.mjs ); \
	 else echo "  (未检测到 node，跳过前端冒烟测试)"; fi

test:
	@( cd agent  && go test ./... )
	@( cd master && go test ./... )

size:
	@echo "==> 产物体积"
	@ls -l $(DIST) 2>/dev/null | awk 'NR>1 && NF>=9 {printf "  %-34s %6.2f MB\n", $$9, $$5/1048576}'

# 可选：UPX 可以把 6.2MB 压到约 2.4MB，但部分安全软件会误报，
# 且压缩后的二进制无法被某些调试工具读取，按需使用。
upx:
	@command -v upx >/dev/null 2>&1 || { echo "未安装 upx，跳过"; exit 0; }
	@for f in $(DIST)/*; do case "$$f" in *.sha256) continue;; esac; echo "  upx $$f"; upx -q -9 "$$f" >/dev/null; done
	@$(MAKE) size

clean:
	rm -rf $(DIST)
