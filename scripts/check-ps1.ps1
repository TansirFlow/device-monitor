# PowerShell 脚本检查：BOM 与语法。
#
# 为什么值得单做一道检查：这两类问题都是**静默**的，不看的话根本不知道发生了。
# 仓库里的 .ps1 带中文注释与提示语，一旦保存成"UTF-8 不带 BOM"，Windows
# PowerShell 5.1 会按系统 ANSI 码页（中文 Windows 上是 GBK）解码它 ——
# 不只中文变乱码，GBK 的双字节解码还会吞掉紧随其后的字节，
# 把换行、反引号续行甚至整行语句吃掉，脚本会毫无报错地少执行几段。
# 纯 ASCII 的脚本不受影响，因此只在检测到非 ASCII 字节时要求 BOM。
#
# 用法（在仓库根目录）：
#   powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/check-ps1.ps1
#   powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/check-ps1.ps1 -Path .devtest/ps-neg

param(
    [string] $Path
)

if ($Path) {
    $root = (Resolve-Path -LiteralPath $Path).Path
} else {
    $root = Split-Path -Parent $PSScriptRoot
}

$fail = 0

# .devtest/ 是本地临时验证目录，不属于交付物，不参与检查
$files = @(Get-ChildItem -Path $root -Recurse -Filter *.ps1 -File |
    Where-Object { $_.FullName.Substring($root.Length).TrimStart('\', '/') -notlike '.devtest*' })

if ($files.Count -eq 0) {
    Write-Host "  (在 $root 下没有找到 .ps1)"
    exit 0
}

foreach ($f in $files) {
    $rel = $f.FullName.Substring($root.Length).TrimStart('\', '/').Replace('\', '/')
    $bytes = [System.IO.File]::ReadAllBytes($f.FullName)

    $hasBom = ($bytes.Length -ge 3) -and
              ($bytes[0] -eq 0xEF) -and ($bytes[1] -eq 0xBB) -and ($bytes[2] -eq 0xBF)
    $nonAscii = $false
    foreach ($b in $bytes) { if ($b -gt 127) { $nonAscii = $true; break } }

    $errs = $null
    [void][System.Management.Automation.Language.Parser]::ParseFile($f.FullName, [ref]$null, [ref]$errs)

    $problems = @()
    if ($nonAscii -and -not $hasBom) {
        $problems += '含非 ASCII 但没有 UTF-8 BOM（PS 5.1 会按 ANSI 码页解码：中文变乱码，还可能吞掉后续语句）'
    }
    if ($errs -and $errs.Count -gt 0) {
        $problems += "语法错误 $($errs.Count) 处（首个在 L$($errs[0].Extent.StartLineNumber)：$($errs[0].Message)）"
    }

    if ($problems.Count -gt 0) {
        Write-Host "  FAIL $rel"
        foreach ($p in $problems) { Write-Host "       - $p" }
        $fail = 1
    } else {
        Write-Host "  ok   $rel  (BOM=$hasBom, 非ASCII=$nonAscii)"
    }
}

Write-Host ''
if ($fail -eq 0) {
    Write-Host "PowerShell 脚本检查通过（$($files.Count) 个文件）。"
    exit 0
}
Write-Host 'PowerShell 脚本检查未通过。'
Write-Host '修复编码：把文件转成 UTF-8 带 BOM + CRLF（例如 python .devtest/fix-encoding.py <文件>）。'
exit 1
