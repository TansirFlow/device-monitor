#Requires -Version 5.1
<#
.SYNOPSIS
    mon-agent 一键安装脚本（Windows + 计划任务）

.DESCRIPTION
    用法（管理员 PowerShell）：

        $env:MON_TOKEN = '<令牌>'
        .\install-agent.ps1 -NodeId web-01 -MasterUrl https://monitor.example.com/api/v1/report

        .\install-agent.ps1 -Uninstall          # 卸载
        .\install-agent.ps1 -WhatIf             # 只看会做什么，不动系统（不需要管理员）

    什么时候**真的**需要管理员：写 %ProgramFiles% / %ProgramData%（安装目录与配置目录），
    以及注册以 SYSTEM 身份运行的计划任务。除此以外不必提权，因此：

      - -WhatIf 全程不需要管理员 —— 参数校验、二进制定位、架构核对、配置渲染都会真实走一遍，
        可以直接拿它预演一次安装，这也是本脚本在 CI/临时环境里做非破坏性验证的方式；
      - 若把 %ProgramFiles% / %ProgramData% 指到当前用户可写的位置（如临时目录、自定义目录），
        安装本身也不需要提权。此时配置 ACL 会额外授权"执行安装的账户"，
        否则装完 agent 连自己的配置都读不到；
      - 不想注册计划任务就加 -NoTask，改用你熟悉的方式守护该进程。

    为什么令牌推荐用环境变量而不是 -Token：
      -Token 会留在 PowerShell 历史（PSReadLine 的 ConsoleHost_history.txt）里；
      环境变量只对管理员与进程属主可读。

    这个脚本只做四件事：放二进制、写配置、收紧配置 ACL、注册开机自启的计划任务。
    它不下载任何东西、不访问网络、不需要主控在线，也**不会**添加任何入站防火墙规则
    —— agent 只需要出向 TCP/443。

    Windows 计划任务的小知识：任务以 SYSTEM 身份运行，且执行时长为"无限制"。
    agent 是个常驻循环进程，如果沿用默认的 3 天上限，会在第 4 天被任务计划程序
    静默杀掉——这是个很难排查的坑，所以 XML 里显式写了 ExecutionTimeLimit=PT0S。

.NOTES
    本文件必须保存为 UTF-8 **带 BOM**，否则 Windows PowerShell 5.1 会把中文读成乱码。
#>

[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [string] $NodeId,
    [string] $MasterUrl,
    [string] $Token = $env:MON_TOKEN,
    [int]    $IntervalSec = 10,
    [string] $Labels,
    [string] $BinaryPath,
    [string] $Sha256,
    [switch] $NoGpu,
    [switch] $AllowInsecureHttp,
    [switch] $NoStart,
    [switch] $NoTask,
    [switch] $Force,
    [switch] $Uninstall
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# ---------------------------------------------------------------- 常量

$TaskName   = 'mon-agent'
$InstallDir = Join-Path $env:ProgramFiles 'mon-agent'
$ExePath    = Join-Path $InstallDir 'mon-agent.exe'
$DataDir    = Join-Path $env:ProgramData  'mon-agent'
$CfgPath    = Join-Path $DataDir 'agent.json'

# 用 SID 而不是组名：非英文版 Windows 上 "Administrators" 是本地化的，
# 写名字会直接失效。
$SidSystem       = '*S-1-5-18'
$SidAdministrators = '*S-1-5-32-544'

# ---------------------------------------------------------------- 输出

function Write-Step { param([string]$Text) Write-Host "`n==> $Text" }
function Write-Say  { param([string]$Text) Write-Host "  $Text" }
function Write-Note { param([string]$Text) Write-Host "  [注意] $Text" -ForegroundColor Yellow }

function Fail {
    param([string]$Text)
    Write-Host "`n[错误] $Text" -ForegroundColor Red
    exit 1
}

function Test-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    (New-Object Security.Principal.WindowsPrincipal $id).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)
}

# 目标目录本身是否归当前用户管。
# 用它来决定要不要提权，而不是无脑要求管理员：真正需要管理员的是
# 「写 Program Files / ProgramData」这件事，不是「运行本脚本」这件事。
# 因此把 $env:ProgramFiles / $env:ProgramData 指向一个临时目录，
# 就能在普通权限下把整套安装流程完整跑一遍，做非破坏性验证。
function Test-DirWritable {
    param([string]$Dir)
    if (-not (Test-Path -LiteralPath $Dir -PathType Container)) { return $false }
    $probe = Join-Path $Dir ('.mon-write-probe-' + [Guid]::NewGuid().ToString('N'))
    try {
        [System.IO.File]::WriteAllText($probe, '')
        # 用 .NET 直接删，而不是 Remove-Item：不同环境下 Remove-Item 可能被
        # 重定向到回收站/删除钩子（企业安全软件、受限沙箱），删不掉就会把
        # "目录不可写"这个判断判错。这里只需要一个"能不能建文件"的探测。
        [System.IO.File]::Delete($probe)
        return $true
    } catch {
        return $false
    }
}

$IsAdmin   = Test-Admin
$IsWhatIf  = [bool]$WhatIfPreference
$NeedAdmin = $false
if (-not $IsWhatIf -and -not $IsAdmin) {
    $installParent = Split-Path -Parent $InstallDir
    $dataParent    = Split-Path -Parent $DataDir
    if (-not (Test-DirWritable $installParent) -or -not (Test-DirWritable $dataParent)) {
        $NeedAdmin = $true
    }
}

# ---------------------------------------------------------------- 卸载

if ($Uninstall) {
    if (-not $IsAdmin -and -not $IsWhatIf) { Fail '请用**管理员** PowerShell 运行卸载（只想看会删什么：加 -WhatIf）。' }

    Write-Step "卸载 $TaskName"

    $task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    if ($task) {
        # 提示语必须放在 ShouldProcess 里面：否则 -WhatIf 预演时会打印"已删除"，
        # 让人以为真的删了。
        if ($PSCmdlet.ShouldProcess($TaskName, '停止并删除计划任务')) {
            Stop-ScheduledTask   -TaskName $TaskName -ErrorAction SilentlyContinue
            Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
            Write-Say '计划任务已删除'
        } else {
            Write-Say '未执行删除（-WhatIf 预演或已取消）'
        }
    } else {
        Write-Say '计划任务不存在，跳过'
    }

    $procs = Get-Process -Name 'mon-agent' -ErrorAction SilentlyContinue
    if ($procs) {
        if ($PSCmdlet.ShouldProcess('mon-agent.exe', '结束正在运行的进程')) {
            $procs | Stop-Process -Force -ErrorAction SilentlyContinue
            Write-Say '已结束正在运行的 agent 进程'
        } else {
            Write-Say '未执行结束进程（-WhatIf 预演或已取消）'
        }
    }

    if (Test-Path -LiteralPath $InstallDir) {
        if ($PSCmdlet.ShouldProcess($InstallDir, '删除安装目录')) {
            Remove-Item -LiteralPath $InstallDir -Recurse -Force
            Write-Say "已删除 $InstallDir"
        } else {
            Write-Say "未执行删除（-WhatIf 预演或已取消）：$InstallDir"
        }
    }

    Write-Host ''
    Write-Say "已卸载。配置**故意保留**在 $CfgPath（里面是上报令牌，重装可直接复用）。"
    Write-Say "确认不再需要后自行删除：Remove-Item -Recurse -Force '$DataDir'"
    exit 0
}

# ---------------------------------------------------------------- 校验

if ($NeedAdmin) {
    Fail "请用**管理员** PowerShell 运行（右键 PowerShell → 以管理员身份运行）。`n       目标目录 $(Split-Path -Parent $InstallDir) 或 $(Split-Path -Parent $DataDir) 当前用户不可写。`n       只想先看一眼会做什么：加 -WhatIf（不需要管理员）"
}

Write-Step '校验参数'

if (-not $NodeId) { Fail '缺少 -NodeId（每台机器必须唯一）' }
if ($NodeId -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$') {
    Fail "NodeId `"$NodeId`" 非法：仅允许 [A-Za-z0-9._-]，1-64 字符，且以字母或数字开头"
}

if (-not $MasterUrl) { Fail '缺少 -MasterUrl' }
if ($MasterUrl -notmatch '^(https?)://') {
    Fail 'MasterUrl 必须以 http:// 或 https:// 开头'
}
if ($MasterUrl -match '^http://') {
    if (-not $AllowInsecureHttp) {
        Fail "MasterUrl 是明文 http，会泄露上报令牌。`n       确认在可信内网中，请加 -AllowInsecureHttp 显式确认"
    }
    Write-Note '已允许明文 http：令牌将以明文经过网络，请确保链路可信'
}

if (-not $Token) {
    Fail '缺少上报令牌：请设置环境变量 MON_TOKEN，或用 -Token（会留在 PowerShell 历史里）'
}
if ($Token.Length -lt 12) { Fail '令牌过短（至少 12 字符），建议用 32 字节随机串' }

if ($IntervalSec -lt 1 -or $IntervalSec -gt 3600) { Fail '-IntervalSec 必须在 1-3600 之间' }

$labelMap = [ordered]@{}
if ($Labels) {
    foreach ($kv in $Labels.Split(',')) {
        $item = $kv.Trim()
        if ($item -eq '') { continue }
        $i = $item.IndexOf('=')
        if ($i -lt 1) { Fail "-Labels 格式应为 k=v,k2=v2，收到：$item" }
        $k = $item.Substring(0, $i).Trim()
        $v = $item.Substring($i + 1).Trim()
        if ("$k$v" -notmatch '^[A-Za-z0-9._:@/-]*$') {
            Fail "标签 $item 含不安全字符（仅允许字母数字 . _ : @ / -）"
        }
        $labelMap[$k] = $v
    }
}

Write-Say "node_id    : $NodeId"
Write-Say "master_url : $MasterUrl"
Write-Say "interval   : ${IntervalSec}s"
if ($Labels) { Write-Say "labels     : $Labels" }
Write-Say "令牌       : 已提供（$($Token.Length) 字符，不显示内容）"

# ---------------------------------------------------------------- 选二进制

Write-Step '定位 agent 二进制'

$rawArch = $env:PROCESSOR_ARCHITEW6432
if (-not $rawArch) { $rawArch = $env:PROCESSOR_ARCHITECTURE }
switch ($rawArch) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { Fail "不支持的架构：$rawArch（目前只提供 amd64 与 arm64）" }
}

if ($BinaryPath) {
    $binSrc = $BinaryPath
} else {
    $binSrc = Join-Path $PSScriptRoot "..\dist\mon-agent-windows-$arch.exe"
}

if (-not (Test-Path -LiteralPath $binSrc)) {
    Fail "找不到二进制：$binSrc`n       用 -BinaryPath <路径> 指定，或从解压后的 release 目录里运行本脚本"
}
$binSrc = (Resolve-Path -LiteralPath $binSrc).Path
Write-Say "使用 $binSrc（$rawArch）"

if ($Sha256) {
    $actual = (Get-FileHash -LiteralPath $binSrc -Algorithm SHA256).Hash
    if ($actual -ne $Sha256.ToUpperInvariant()) {
        Fail "SHA256 校验失败`n       期望 $($Sha256.ToUpperInvariant())`n       实际 $actual"
    }
    Write-Say 'SHA256 校验通过'
}

# 先验一次二进制能否运行，避免装完才发现拷错了架构
try {
    $null = & $binSrc -version 2>&1
    if ($LASTEXITCODE -ne 0) { throw "退出码 $LASTEXITCODE" }
} catch {
    Fail "二进制无法执行：$binSrc（架构不对或文件损坏）—— $_"
}
Write-Say "自报版本：$(& $binSrc -version)"

# ---------------------------------------------------------------- 安装

Write-Step "1/4 安装二进制到 $InstallDir"
if ($PSCmdlet.ShouldProcess($InstallDir, '创建目录并复制可执行文件')) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    Copy-Item -LiteralPath $binSrc -Destination $ExePath -Force
    Write-Say '完成'
} else {
    Write-Say '未执行（-WhatIf 预演或已取消）'
}

Write-Step "2/4 写入配置 $CfgPath"

$cfgExists = Test-Path -LiteralPath $CfgPath
if ($cfgExists -and -not $Force) {
    Write-Say '配置已存在，保持不变（要覆盖请加 -Force）'
    try {
        $cur = Get-Content -LiteralPath $CfgPath -Raw | ConvertFrom-Json
        Write-Say "当前 node_id：$($cur.node_id)"
    } catch {
        Write-Note "现有配置无法解析为 JSON：$_"
    }
} else {
    $cfgObj = [ordered]@{
        node_id      = $NodeId
        master_url   = $MasterUrl
        token        = $Token
        interval_sec = $IntervalSec
        timeout_sec  = 8
        buffer_max   = 120
        labels       = $labelMap
        collect      = [ordered]@{
            cpu          = $true
            mem          = $true
            disk         = $true
            net          = $true
            gpu          = (-not $NoGpu)
            temp         = $true
            temp_limit   = 32
            temp_exclude = @()
        }
        gpu          = [ordered]@{
            enabled = (-not $NoGpu)
            binary  = 'nvidia-smi'
        }
    }

    if ($PSCmdlet.ShouldProcess($CfgPath, '写入配置文件')) {
        New-Item -ItemType Directory -Path $DataDir -Force | Out-Null
        $json = $cfgObj | ConvertTo-Json -Depth 6

        # 关键：必须写成 UTF-8 **不带 BOM**。
        # agent 用 Go 的 encoding/json 解析，它遇到开头的 BOM 会直接报
        # "invalid character 'ï' looking for beginning of value"——
        # 而 PowerShell 5.1 的 `Set-Content -Encoding UTF8` 恰恰会写入 BOM。
        $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
        [System.IO.File]::WriteAllText($CfgPath, $json, $utf8NoBom)
        Write-Say '已写入配置'
    } else {
        Write-Say '未写入配置（-WhatIf 预演或已取消）'
    }
}

# 收紧 ACL：配置文件里有上报令牌，不能让普通用户读到。
# /inheritance:r 先断开继承，再只授权 SYSTEM、Administrators 与**执行安装的账户**。
# 为什么带上安装账户：
#   以管理员身份安装时它本来就在 Administrators 里（这条是幂等的）；
#   但目标目录本来就归当前用户时（见上面的提权判定）脚本允许不提权安装，
#   此时若不显式授权，安装完 agent 连自己的配置都读不了（Access is denied）。
$installerSid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
if ($PSCmdlet.ShouldProcess($CfgPath, '收紧文件 ACL（仅 SYSTEM / Administrators / 安装账户 可读）')) {
    $null = icacls $CfgPath /inheritance:r `
        /grant:r "$SidSystem`:(R)" "$SidAdministrators`:(R)" "*$installerSid`:(R)" 2>&1
    if ($LASTEXITCODE -ne 0) { Write-Note "icacls 返回 $LASTEXITCODE，请手动确认 $CfgPath 的权限" }
    Write-Say 'ACL 已设为：SYSTEM / Administrators / 安装账户 可读，其他用户无权限'
} else {
    Write-Say '未收紧 ACL（-WhatIf 预演或已取消）'
}

if ($AllowInsecureHttp) {
    # 机器级环境变量会让所有账户继承，写它需要管理员令牌。
    if (-not $IsAdmin -and -not $IsWhatIf) {
        Fail "设置机器级环境变量 MON_ALLOW_INSECURE_HTTP 需要管理员权限。`n       若只是想在本机试试，可改成在调用处临时设置进程级变量。"
    }
    if ($PSCmdlet.ShouldProcess('机器级环境变量', '设置 MON_ALLOW_INSECURE_HTTP=1')) {
        [Environment]::SetEnvironmentVariable('MON_ALLOW_INSECURE_HTTP', '1', 'Machine')
    }
    Write-Say '已写入机器级环境变量 MON_ALLOW_INSECURE_HTTP=1'
    Write-Say "  不再需要时用这条命令移除："
    Write-Say "    [Environment]::SetEnvironmentVariable('MON_ALLOW_INSECURE_HTTP', `$null, 'Machine')"
}

Write-Step '3/4 注册开机自启的计划任务'

if ($NoTask) {
    Write-Say '-NoTask 已指定，跳过计划任务注册（请用你喜欢的方式守护该进程）'
} else {
    # 注册以 SYSTEM 身份运行的计划任务，必须有管理员令牌——这一条无法绕过，
    # 也不能靠"目标目录可写"来推导（那两者是不同性质的特权）。
    if (-not $IsAdmin -and -not $IsWhatIf) {
        Fail "注册开机自启的计划任务需要管理员权限。`n       请改用管理员 PowerShell 运行，或加 -NoTask 自行守护该进程。"
    }
    # 以 SYSTEM 身份运行有几个好处：开机即起、不依赖谁登录、
    # 而且 SYSTEM 是管理员级令牌，磁盘温度这种需要提权的采集项也能读到。
    $taskXml = @"
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>mon-agent：单向指标上报。只发出站请求，不监听任何端口。</Description>
    <URI>\$TaskName</URI>
  </RegistrationInfo>
  <Triggers>
    <BootTrigger>
      <Enabled>true</Enabled>
      <Delay>PT15S</Delay>
    </BootTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>S-1-5-18</UserId>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <UseUnifiedSchedulingEngine>true</UseUnifiedSchedulingEngine>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>3</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>"$ExePath"</Command>
      <Arguments>-config "$CfgPath"</Arguments>
    </Exec>
  </Actions>
</Task>
"@

    if ($PSCmdlet.ShouldProcess($TaskName, '注册计划任务（开机自启 + 失败自动重启）')) {
        Register-ScheduledTask -TaskName $TaskName -Xml $taskXml -Force | Out-Null
        Write-Say "计划任务 $TaskName 已注册（SYSTEM 身份，开机 15 秒后启动）"
    } else {
        Write-Say "未注册计划任务 $TaskName（-WhatIf 预演或已取消）"
    }
}

Write-Step '4/4 自检'

if ($IsWhatIf) {
    # -WhatIf 下二进制根本没被拷过去，跑了只会得到一条误导的"自检未通过"
    Write-Say '-WhatIf：未落盘，跳过自检（真实安装时这里会依次跑 -once 与 -selftest）'
} else {
    # agent 是 Go 程序，往 stdout/stderr 写的是 UTF-8；PowerShell 5.1 却按
    # [Console]::OutputEncoding（中文 Windows 上通常是 GBK）解码原生程序输出，
    # 于是自检里那些中文报错会变成乱码 —— 而它恰恰是排查上报链路问题时唯一要看的字。
    # 因此只在调用 exe 期间临时切到 UTF-8，结束后立刻还原：
    # 还原是必须的，否则 icacls 等原生命令的中文输出反而会乱。
    $savedOutEnc = [Console]::OutputEncoding
    try {
        [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)

        $onceOk = $false
        try {
            $null = & $ExePath -config $CfgPath -once 2>&1
            $onceOk = ($LASTEXITCODE -eq 0)
        } catch {
            $onceOk = $false
        }
        if ($onceOk) {
            Write-Say '采集正常（-once 通过）'
        } else {
            Write-Note '采集自检未通过，请手动执行看完整输出：'
            Write-Say "  & '$ExePath' -config '$CfgPath' -once"
        }

        try {
            $out = & $ExePath -config $CfgPath -selftest 2>&1
            if ($LASTEXITCODE -eq 0) {
                Write-Say "上报链路正常：$out"
            } else {
                Write-Note "上报自检未通过：$out"
                Write-Say '  常见原因：MasterUrl 写错、令牌不匹配、该节点未登记进主控的 node_tokens'
            }
        } catch {
            Write-Note "上报自检未通过：$_"
            Write-Say '  常见原因：MasterUrl 写错、令牌不匹配、该节点未登记进主控的 node_tokens'
        }
    } finally {
        [Console]::OutputEncoding = $savedOutEnc
    }
}

if (-not $NoStart -and -not $NoTask) {
    if ($PSCmdlet.ShouldProcess($TaskName, '立即启动')) {
        Start-ScheduledTask -TaskName $TaskName
        Write-Say '服务已启动'
    } else {
        Write-Say '未启动服务（-WhatIf 预演或已取消）'
    }
} else {
    Write-Say '未自动启动服务'
}

Write-Host ''
Write-Say '安装完成。常用命令：'
Write-Say "  Get-ScheduledTask $TaskName | Get-ScheduledTaskInfo"
Write-Say "  Get-Process mon-agent"
Write-Say "  & '$ExePath' -config '$CfgPath' -selftest    # 重新验证链路"
Write-Say "  & '$ExePath' -config '$CfgPath' -once        # 看一眼采集到的原始 JSON"
Write-Say '  改完配置后：Restart-ScheduledTask mon-agent'
Write-Host ''
Write-Say '本 agent 只发出站请求，没有监听端口，因此**不需要**添加任何入站防火墙规则。'

# 显式给出成功退出码：脚本内部调用过若干原生程序（exe -version、icacls…），
# 不显式 exit 的话 $LASTEXITCODE 会残留最后一条原生命令的结果，
# 让调用方（自动化、CI）无法可靠判断安装是否成功。
exit 0
