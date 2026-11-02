# CI smoke test: starts the built server and runs tools/Publish-FileServer.ps1 through
# the scenarios CI relies on (first deployment, re-run, immutability, draft resume, fixed files).
# Usage (from the repository root, after `go build -o bin/ ./cmd/server`): pwsh ./tools/ci/Test-PublishScript.ps1
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$script = Join-Path $root 'tools/Publish-FileServer.ps1'
$serverExe = Join-Path $root ($(if ($IsWindows) { 'bin/server.exe' } else { 'bin/server' }))
$work = Join-Path ([IO.Path]::GetTempPath()) ("fs-ci-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $work | Out-Null
$port = 18090
$base = "http://127.0.0.1:$port"
$failures = 0

function Check([bool]$Condition, [string]$Message) {
    if ($Condition) { Write-Host "PASS  $Message" -ForegroundColor Green }
    else { Write-Host "FAIL  $Message" -ForegroundColor Red; $script:failures++ }
}
# Runs the publish script in a child pwsh so exit codes are real. Returns @{Code; Out}.
function Publish([string[]]$Arguments, [hashtable]$Env = @{}) {
    $saved = @{}
    foreach ($k in $Env.Keys) { $saved[$k] = [Environment]::GetEnvironmentVariable($k); [Environment]::SetEnvironmentVariable($k, $Env[$k]) }
    try {
        $out = & pwsh -NoProfile -File $script @Arguments 2>&1 | Out-String
        $code = $LASTEXITCODE
    } finally {
        foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
    }
    Write-Host "---- Publish-FileServer.ps1 $($Arguments -join ' ') => exit $code`n$out"
    return @{ Code = $code; Out = $out }
}
function Get-Json([string]$Path) { Invoke-RestMethod -Uri "$base$Path" -Headers @{ 'Cache-Control' = 'no-cache' } }

$env:FS_ADMIN_PASSWORD = 'ci-password'
$proc = Start-Process -FilePath $serverExe -ArgumentList @('-addr', "127.0.0.1:$port", '-data', (Join-Path $work 'data')) -PassThru `
    -RedirectStandardError (Join-Path $work 'server.log') -RedirectStandardOutput (Join-Path $work 'server.out')
try {
    for ($i = 0; $i -lt 50; $i++) {
        try { if ((Invoke-WebRequest -Uri "$base/healthz" -UseBasicParsing).StatusCode -eq 200) { break } } catch { Start-Sleep -Milliseconds 200 }
    }
    Invoke-RestMethod -Method Post -Uri "$base/api/v1/auth/login" -ContentType 'application/json' `
        -Body '{"username":"admin","password":"ci-password"}' -SessionVariable session | Out-Null
    $token = (Invoke-RestMethod -Method Post -Uri "$base/api/v1/admin/tokens" -WebSession $session -ContentType 'application/json' `
        -Headers @{ 'X-Requested-With' = 'ci' } -Body '{"name":"ci"}').secret
    $env:FS_SERVER = $base
    $env:FS_TOKEN = $token

    $dist = Join-Path $work 'dist'
    New-Item -ItemType Directory -Path $dist | Out-Null
    $exe = Join-Path $dist 'ics-scm.exe'
    $versionTxt = Join-Path $dist 'version.txt'
    Set-Content -LiteralPath $exe -Value 'build 4' -NoNewline
    Set-Content -LiteralPath $versionTxt -Value '1.0.0.4'

    # 1. CI without secrets: skipped, success.
    $r = Publish @('-Program', 'ics-scm', '-File', $exe, '-SkipIfUnconfigured') @{ FS_SERVER = ''; FS_TOKEN = '' }
    Check ($r.Code -eq 0 -and $r.Out -match 'skipping') 'no secrets -> skipped with exit 0'

    # 2. First deployment: program created automatically, version from version.txt, notes from git.
    $outputs = Join-Path $work 'gh-output.txt'
    $r = Publish @('-Program', 'ics-scm', '-File', $exe, '-NotesFromGit', 'HEAD~1') @{ GITHUB_OUTPUT = $outputs }
    Check ($r.Code -eq 0 -and $r.Out -match 'Created program ics-scm') 'first deployment creates the program'
    $latest = Get-Json '/ics-scm/latest.json'
    Check ($latest.version -eq '1.0.0.4' -and $latest.file -eq 'ics-scm-1.0.0.4.exe') "latest.json = $($latest.version) $($latest.file)"
    Check ($latest.sha256 -eq (Get-FileHash -LiteralPath $exe -Algorithm SHA256).Hash.ToLowerInvariant()) 'latest.json sha256 matches the uploaded file'
    Check ($latest.notes -match '^- ') 'patch notes generated from git'
    Check ((Get-Json '/api/v1/programs/ics-scm').name -eq 'ics-scm') 'program name defaults to the slug'
    $gh = Get-Content -LiteralPath $outputs -Raw
    Check ($gh -match 'status=published' -and $gh -match 'version=1\.0\.0\.4' -and $gh -match 'latest_json=') 'GITHUB_OUTPUT values written'

    # 3. CI re-run with the same build: success, nothing changed.
    $r = Publish @('-Program', 'ics-scm', '-File', $exe)
    Check ($r.Code -eq 0 -and $r.Out -match 'already published') 're-run of the same version -> already published, exit 0'

    # 4. Same version, different bytes: refused.
    Set-Content -LiteralPath $exe -Value 'build 4 but different' -NoNewline
    $r = Publish @('-Program', 'ics-scm', '-File', $exe)
    Check ($r.Code -ne 0 -and $r.Out -match 'immutable') 'same version with a different file -> error'

    # 5. Next build is published and becomes latest.json.
    Set-Content -LiteralPath $versionTxt -Value '1.0.0.5'
    $r = Publish @('-Program', 'ics-scm', '-File', $exe, '-Notes', 'build five')
    Check ($r.Code -eq 0 -and (Get-Json '/ics-scm/latest.json').version -eq '1.0.0.5') 'next version published automatically'
    $download = Join-Path $work 'download.exe'
    Invoke-WebRequest -Uri "$base/ics-scm/ics-scm-1.0.0.5.exe" -OutFile $download -UseBasicParsing
    Check ((Get-FileHash $download).Hash -eq (Get-FileHash $exe).Hash) 'exe named in latest.json downloads intact'

    # 6. Draft, then resume after a rebuild: file replaced, stored notes kept.
    $r = Publish @('-Program', 'ics-scm', '-File', $exe, '-Version', '1.0.0.6', '-Notes', 'draft notes', '-Draft')
    Check ($r.Code -eq 0 -and (Get-Json '/ics-scm/latest.json').version -eq '1.0.0.5') 'draft is not offered to clients'
    Set-Content -LiteralPath $exe -Value 'build 6 rebuilt' -NoNewline
    $r = Publish @('-Program', 'ics-scm', '-File', $exe, '-Version', '1.0.0.6')
    $rel = Get-Json '/api/v1/programs/ics-scm/releases/1.0.0.6'
    Check ($r.Code -eq 0 -and $r.Out -match 'Replaced' -and $rel.notes -eq 'draft notes' -and @($rel.assets).Count -eq 1) 'draft resumed, file replaced, notes kept'

    # 7. Fixed file only.
    $defaults = Join-Path $work 'defaults.json'
    Set-Content -LiteralPath $defaults -Value '{"NativeNotifications":true}' -NoNewline
    $r = Publish @('-Program', 'onetaskbar', '-Name', 'One TaskBar', '-FixedFile', $defaults)
    $served = (Invoke-WebRequest -Uri "$base/onetaskbar/defaults.json" -UseBasicParsing).Content
    if ($served -is [byte[]]) { $served = [Text.Encoding]::UTF8.GetString($served) }
    Check ($r.Code -eq 0 -and $served -eq '{"NativeNotifications":true}') 'fixed file uploaded and served'

    # 8. Explicit -Version and -Name on another program; wrong token is an error.
    $setup = Join-Path $work 'OneTaskBar-Setup.exe'
    Set-Content -LiteralPath $setup -Value 'setup' -NoNewline
    $r = Publish @('-Program', 'onetaskbar', '-Name', 'One TaskBar', '-File', $setup, '-Version', '2.0.0.1', '-Mandatory')
    $otb = Get-Json '/onetaskbar/latest.json'
    Check ($r.Code -eq 0 -and $otb.version -eq '2.0.0.1' -and $otb.file -eq 'OneTaskBar-Setup-2.0.0.1.exe' -and $otb.mandatory) 'onetaskbar published with -Version/-Mandatory'
    $r = Publish @('-Program', 'onetaskbar', '-File', $setup, '-Version', '2.0.0.2') @{ FS_TOKEN = 'fst_wrong' }
    Check ($r.Code -ne 0 -and $r.Out -match '401') 'wrong token -> error'

    # 9. Older than the newest published version: refused unless -AllowOlder.
    $r = Publish @('-Program', 'ics-scm', '-File', $exe, '-Version', '1.0.0.3')
    Check ($r.Code -ne 0 -and $r.Out -match 'older than 1\.0\.0\.6') 'older version refused'
    $r = Publish @('-Program', 'ics-scm', '-File', $exe, '-Version', '1.0.0.3', '-AllowOlder')
    Check ($r.Code -eq 0 -and (Get-Json '/ics-scm/latest.json').version -eq '1.0.0.6') '-AllowOlder publishes a maintenance build without changing latest'
    Check ($r.Out -match 'Server: ics-scm stable 1\.0\.0\.6' -and $r.Out -match '"version":"1\.0\.0\.6"') 'server version and latest.json shown'

    # 10. Token store: save once, then publish with neither FS_SERVER nor FS_TOKEN set.
    $storePath = if ($IsWindows) { Join-Path $env:APPDATA 'FileServer\credentials.json' } else { Join-Path $HOME '.config/fileserver/credentials.json' }
    $noEnv = @{ FS_SERVER = ''; FS_TOKEN = '' }
    $r = Publish @('-Server', $base, '-Token', $token, '-SaveToken') $noEnv
    Check ($r.Code -eq 0 -and $r.Out -match 'Token saved' -and (Test-Path -LiteralPath $storePath)) 'token saved to the store'
    $stored = Get-Content -LiteralPath $storePath -Raw
    if ($IsWindows) { Check (-not $stored.Contains($token) -and $stored -match '"dpapi"') 'stored token is DPAPI-encrypted' }
    else { Check (((& stat -c '%a' $storePath) -join '') -eq '600') 'store file mode is 600' }
    Set-Content -LiteralPath $versionTxt -Value '1.0.0.7'
    $r = Publish @('-Program', 'ics-scm', '-File', $exe) $noEnv
    Check ($r.Code -eq 0 -and $r.Out -match 'Published ics-scm 1\.0\.0\.7') 'saved server + token used when nothing is configured'
    $r = Publish @('-Program', 'ics-scm', '-File', $exe, '-Token', 'fst_revoked') @{ FS_SERVER = '' }
    Check ($r.Code -ne 0 -and $r.Out -match '401') 'rejected token reported'
    $r = Publish @('-ForgetToken') $noEnv
    Check ($r.Code -eq 0 -and $r.Out -match 'Forgot') 'token forgotten'
    $r = Publish @('-Program', 'ics-scm', '-File', $exe, '-SkipIfUnconfigured') $noEnv
    Check ($r.Code -eq 0 -and $r.Out -match 'skipping') 'no token after -ForgetToken -> skipped'
}
finally {
    if ($proc -and -not $proc.HasExited) { Stop-Process -Id $proc.Id -Force }
    if ($failures -gt 0) {
        Write-Host '---- server log'; Get-Content (Join-Path $work 'server.log') -ErrorAction SilentlyContinue | Select-Object -Last 50
    }
}
if ($failures -gt 0) { Write-Error "$failures check(s) failed"; exit 1 }
Write-Host 'All publish script checks passed.' -ForegroundColor Green
# The last scenario intentionally ends a child with exit 1; do not let $LASTEXITCODE leak.
exit 0
