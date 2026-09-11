package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 一键安装脚本的生成
//
// 设计要点：
//  1. 脚本由主控**被动应答**生成：节点自己来拉，主控不主动联系任何机器
//     （单向性不变，scripts/security-audit.sh 依然全绿）。
//  2. 脚本里必然含有该节点的上报令牌 —— 否则装完还得手工填。
//     所以脚本头部的警告必须写清楚，且安装码要能单独轮换/撤销，
//     让"把命令贴到工单里"这种事的爆炸半径可控。
//  3. 脚本自包含：目标机器上通常没有仓库，systemd 单元直接内联在里面。
//  4. 架构在脚本里自动识别，一份脚本同时适用 amd64 / arm64。
// ---------------------------------------------------------------------------

type installContext struct {
	NodeID    string
	Code      string
	MasterURL string
	Token     string
	Interval  int
	Generated time.Time
}

const installWarnBlock = `# ⚠ 这个脚本里含有该节点的**上报令牌**，请当作密钥处理：
#   不要提交进代码仓库，不要贴到工单或聊天记录里。
#   令牌一旦泄露，在主控后台点「轮换令牌」即可作废；已安装的机器需要重新执行本脚本。
#   安装命令（其中 code）可单独轮换/撤销，不影响已经在正常上报的节点。`

func (c installContext) configJSON() (string, error) {
	cfg := map[string]any{
		"node_id":      c.NodeID,
		"master_url":   strings.TrimSuffix(c.MasterURL, "/") + "/api/v1/report",
		"token":        c.Token,
		"interval_sec": c.Interval,
		"timeout_sec":  8,
		"buffer_max":   120,
		"collect": map[string]any{
			"cpu":          true,
			"mem":          true,
			"disk":         true,
			"net":          true,
			"gpu":          true,
			"temp":         true,
			"temp_limit":   32,
			"temp_exclude": []string{},
		},
		"gpu": map[string]any{"enabled": true, "binary": "nvidia-smi"},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (c installContext) render(kind string) (string, error) {
	cfgJSON, err := c.configJSON()
	if err != nil {
		return "", err
	}
	repl := strings.NewReplacer(
		"__NODE_ID__", c.NodeID,
		"__MASTER_URL__", strings.TrimSuffix(c.MasterURL, "/"),
		"__TOKEN__", c.Token,
		"__CODE__", c.Code,
		"__GENERATED_AT__", c.Generated.Format("2006-01-02 15:04:05 MST"),
		"__CONFIG_JSON__", cfgJSON,
		"__SYSTEMD_UNIT__", strings.TrimRight(systemdUnitText, "\n"),
		"__WARN__", installWarnBlock,
		"__INTERVAL__", fmt.Sprintf("%d", c.Interval),
	)
	switch kind {
	case "windows":
		return repl.Replace(windowsInstallTemplate), nil
	default:
		return repl.Replace(linuxInstallTemplate), nil
	}
}

// installCommand 返回可直接粘到目标机器上执行的一行命令。
// 它只带 node + code，不带令牌 —— 令牌由主控校验通过后写进脚本里。
func (c installContext) installCommand(kind, baseURL string) string {
	url := strings.TrimSuffix(baseURL, "/")
	switch kind {
	case "windows":
		return fmt.Sprintf(`powershell -NoProfile -ExecutionPolicy Bypass -Command "Invoke-Expression ((Invoke-WebRequest -UseBasicParsing '%s/api/v1/install.ps1?node=%s&code=%s').Content)"`,
			url, c.NodeID, c.Code)
	default:
		return fmt.Sprintf(`curl -fsSL '%s/api/v1/install.sh?node=%s&code=%s' | sudo sh`,
			url, c.NodeID, c.Code)
	}
}

// ---------------------------------------------------------------------------
// Linux：下载二进制 → 建专用用户 → 写配置 → 装 systemd 单元 → 自检
// ---------------------------------------------------------------------------

const linuxInstallTemplate = `#!/bin/sh
# mon-agent 一键安装脚本（由主控生成）
#
#   节点     : __NODE_ID__
#   主控     : __MASTER_URL__
#   生成时间 : __GENERATED_AT__
#
__WARN__
#
# 用法（在目标机器上以 root 执行）：
#   sh install-__NODE_ID__.sh

set -eu

MASTER_URL='__MASTER_URL__'
NODE_ID='__NODE_ID__'
CODE='__CODE__'
CFG='/etc/mon-agent/agent.json'
BIN='/usr/local/bin/mon-agent'
UNIT='/etc/systemd/system/mon-agent.service'
RUN_USER='mon-agent'

if [ "$(id -u)" -ne 0 ]; then
  echo "[错误] 请用 root 运行：sudo sh $0" >&2
  exit 1
fi

# 架构自动识别：同一份脚本同时适用 amd64 与 arm64
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "[错误] 不支持的架构：$(uname -m)（目前提供 amd64 与 arm64）" >&2; exit 1 ;;
esac
BIN_URL="$MASTER_URL/api/v1/agent/mon-agent-linux-$ARCH?node=$NODE_ID&code=$CODE"

echo "==> 1/5 下载 agent 二进制（$ARCH）"
mkdir -p "$(dirname "$BIN")"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$BIN_URL" -o "$BIN"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$BIN" "$BIN_URL"
else
  echo "[错误] 需要 curl 或 wget 之一" >&2
  exit 1
fi
chmod 0755 "$BIN"
"$BIN" -version

echo "==> 2/5 创建专用低权限用户 $RUN_USER"
if ! id "$RUN_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$RUN_USER"
  echo "已创建系统用户 $RUN_USER（不可登录、无家目录）"
else
  echo "用户 $RUN_USER 已存在，跳过"
fi
# 读 GPU 指标（nvidia-smi）通常需要 video 组；组不存在就跳过
if getent group video >/dev/null 2>&1; then
  usermod -aG video "$RUN_USER" 2>/dev/null || true
fi

echo "==> 3/5 写入配置 $CFG"
mkdir -p "$(dirname "$CFG")"
# umask 077：文件从创建的第一刻起就只有 root 能读，
# 不存在"先按默认权限建好、再 chmod"的那个中间窗口
( umask 077; cat > "$CFG" <<'MON_AGENT_JSON'
__CONFIG_JSON__
MON_AGENT_JSON
)
chown root:root "$CFG"
chmod 600 "$CFG"

echo "==> 4/5 安装 systemd 单元"
cat > "$UNIT" <<'MON_AGENT_UNIT'
__SYSTEMD_UNIT__
MON_AGENT_UNIT
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload
  systemctl enable --now mon-agent
else
  echo "[注意] 系统里没有 systemd，请自行守护该进程：" >&2
  echo "       $BIN -config $CFG" >&2
fi

echo "==> 5/5 自检（真实上报一条，验证网络与鉴权）"
if "$BIN" -config "$CFG" -selftest; then
  echo ""
  echo "安装完成。看板：$MASTER_URL"
  echo "  常用命令：systemctl status mon-agent / journalctl -u mon-agent -f"
else
  echo "[注意] 自检未通过。常见原因：主控地址不可达、令牌不匹配、该节点在主控后台被停用。" >&2
  echo "       看完整输出：$BIN -config $CFG -selftest" >&2
fi
`

// systemdUnitText 与 deploy/mon-agent.service 保持一致：
// 主控生成脚本时目标机器上没有仓库，单元只能内联。
// 加固项（ProtectSystem=strict 等）由 installscript_test.go 断言存在，
// 免得以后改了 deploy/mon-agent.service 却忘了同步这里。
const systemdUnitText = `[Unit]
Description=Server Monitor Agent（单向指标上报）
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=mon-agent
Group=mon-agent
ExecStart=/usr/local/bin/mon-agent -config /etc/mon-agent/agent.json

# agent 本身已经不监听端口、不写文件、不执行外部命令（GPU 采集除外），
# 这里再由内核把约束固化一遍：即使进程被完全控制也没有落脚点。
NoNewPrivileges=yes
PrivateTmp=yes
PrivateUsers=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

# 不需要任何特权
CapabilityBoundingSet=
AmbientCapabilities=

# 运行期不产生任何持久化状态，因此不开放任何可写路径
UMask=0077

MemoryMax=256M
CPUQuota=20%
TasksMax=64

Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=mon-agent

[Install]
WantedBy=multi-user.target
`

// ---------------------------------------------------------------------------
// Windows：下载 exe → 写配置并收紧 ACL → 注册 SYSTEM 计划任务
// ---------------------------------------------------------------------------

const windowsInstallTemplate = `# mon-agent 一键安装脚本（由主控生成）
#
#   节点     : __NODE_ID__
#   主控     : __MASTER_URL__
#   生成时间 : __GENERATED_AT__
#
__WARN__
#
# 用法（**管理员** PowerShell）：
#   powershell -ExecutionPolicy Bypass -File install-__NODE_ID__.ps1
#
# 本文件为 UTF-8 **带 BOM**：Windows PowerShell 5.1 按系统 ANSI 码页读取不带 BOM 的脚本，
# 中文会变乱码（而且 GBK 的双字节解码还可能吃掉后续字符）。

#Requires -Version 5.1
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$MasterUrl  = '__MASTER_URL__'
$NodeId     = '__NODE_ID__'
$Code       = '__CODE__'
$TaskName   = 'mon-agent'
$InstallDir = Join-Path $env:ProgramFiles 'mon-agent'
$ExePath    = Join-Path $InstallDir 'mon-agent.exe'
$DataDir    = Join-Path $env:ProgramData 'mon-agent'
$CfgPath    = Join-Path $DataDir 'agent.json'

# 需要管理员：写 Program Files / ProgramData，以及注册以 SYSTEM 身份运行的计划任务。
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not (New-Object Security.Principal.WindowsPrincipal $id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host '[错误] 请用**管理员** PowerShell 运行。' -ForegroundColor Red
    exit 1
}

# 架构自动识别
switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { Write-Host "[错误] 不支持的架构：$env:PROCESSOR_ARCHITECTURE" -ForegroundColor Red; exit 1 }
}
$BinUrl = "$MasterUrl/api/v1/agent/mon-agent-windows-$arch.exe?node=$NodeId&code=$Code"

Write-Host "==> 1/4 下载 agent 二进制（$arch）"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$tmp = Join-Path $env:TEMP ("mon-agent-" + [Guid]::NewGuid().ToString('N') + '.exe')
Invoke-WebRequest -Uri $BinUrl -OutFile $tmp -UseBasicParsing
New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
Move-Item -LiteralPath $tmp -Destination $ExePath -Force
& $ExePath -version

Write-Host "==> 2/4 写入配置 $CfgPath"
New-Item -ItemType Directory -Path $DataDir -Force | Out-Null
$json = @'
__CONFIG_JSON__
'@
# 必须写成 UTF-8 **不带 BOM**：Go 的 encoding/json 遇到开头的 BOM 会直接报错，
# 而 PowerShell 5.1 的 Set-Content -Encoding UTF8 恰恰会写 BOM。
[System.IO.File]::WriteAllText($CfgPath, $json, (New-Object System.Text.UTF8Encoding($false)))
# 收紧 ACL：配置里有上报令牌，不能让普通用户读到
$null = icacls $CfgPath /inheritance:r /grant:r '*S-1-5-18:(R)' '*S-1-5-32-544:(R)' 2>&1

Write-Host "==> 3/4 注册开机自启的计划任务（SYSTEM 身份）"
# ExecutionTimeLimit=PT0S 很关键：默认 3 天上限会把常驻进程在第 4 天静默杀掉
$taskXml = @"
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>mon-agent：单向指标上报。只发出站请求，不监听任何端口。</Description>
  </RegistrationInfo>
  <Triggers>
    <BootTrigger><Enabled>true</Enabled><Delay>PT15S</Delay></BootTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author"><UserId>S-1-5-18</UserId><RunLevel>LeastPrivilege</RunLevel></Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings><StopOnIdleEnd>false</StopOnIdleEnd><RestartOnIdle>false</RestartOnIdle></IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <UseUnifiedSchedulingEngine>true</UseUnifiedSchedulingEngine>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>"$ExePath"</Command>
      <Arguments>-config "$CfgPath"</Arguments>
    </Exec>
  </Actions>
</Task>
"@
Register-ScheduledTask -TaskName $TaskName -Xml $taskXml -Force | Out-Null

Write-Host "==> 4/4 自检（真实上报一条，验证网络与鉴权）"
& $ExePath -config $CfgPath -selftest
if ($LASTEXITCODE -eq 0) {
    Write-Host ''
    Write-Host "安装完成。看板：$MasterUrl"
    Write-Host "  常用命令：Get-ScheduledTask $TaskName | Get-ScheduledTaskInfo"
} else {
    Write-Host '[注意] 自检未通过。常见原因：主控地址不可达、令牌不匹配、该节点在主控后台被停用。' -ForegroundColor Yellow
}
Start-ScheduledTask -TaskName $TaskName
`
