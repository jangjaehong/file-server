[CmdletBinding()]
param(
    # File server base URL, e.g. http://211.231.121.41:9100 (default: $env:FS_SERVER)
    [string]$Server=$env:FS_SERVER,
    # API token from 관리자 → API 토큰 (default: $env:FS_TOKEN)
    [string]$Token=$env:FS_TOKEN,
    # Program slug = folder in the update URL: onetaskbar, ics-scm
    [Parameter(Mandatory=$true)][string]$Program,
    # Display name used when the program is created on first deployment (default: the slug)
    [string]$Name='',
    # The executable / setup to publish as a new version.
    [string]$File='',
    [string]$Platform='windows-amd64',
    # Version. Default: version.txt next to -File, else the exe's FileVersion (Windows only).
    [string]$Version='',
    [ValidateSet('stable','beta')][string]$Channel='stable',
    [string]$Notes='',
    [string]$NotesFile='',
    # Patch notes from commit subjects: "<ref>" (= <ref>..HEAD) or "<a>..<b>". Ignored when -Notes/-NotesFile is given.
    [string]$NotesFromGit='',
    [switch]$Mandatory,
    # Upload only; publish later in the admin page.
    [switch]$Draft,
    # Unversioned files (e.g. defaults.json), replaced on every upload.
    [string[]]$FixedFile=@(),
    # Exit 0 without doing anything when -Server/-Token are empty (CI without secrets).
    [switch]$SkipIfUnconfigured
)
# Publishes a build to the file server. Safe to re-run (CI retries):
#  - the program is created on first deployment;
#  - a missing version is created, uploaded and published;
#  - a draft version is resumed (a file of the same platform with another SHA-256 is replaced);
#  - a published version with the same SHA-256 -> "already published", exit 0;
#    with a different SHA-256 -> error (published versions are immutable; build a new version number).
# Windows PowerShell 5.1 and PowerShell 7 (Windows / Linux runners). Same logic as `fsctl publish`.
$ErrorActionPreference='Stop'
if(-not $Server -or -not $Token){
    if($SkipIfUnconfigured){Write-Output 'FS_SERVER/FS_TOKEN not set; skipping publish';exit 0}
    throw 'Server and token required: -Server/-Token or $env:FS_SERVER/$env:FS_TOKEN (관리자 페이지 → API 토큰)'
}
if(-not $File -and $FixedFile.Count -eq 0){throw 'Nothing to publish: give -File and/or -FixedFile'}
if(-not $Name){$Name=$Program}
Add-Type -AssemblyName System.Net.Http
$base=$Server.TrimEnd('/')
$slug=[Uri]::EscapeDataString($Program)
$latestJson="$base/$Program/latest.json"

$http=New-Object System.Net.Http.HttpClient
$http.Timeout=[TimeSpan]::FromMinutes(30)
$http.DefaultRequestHeaders.Authorization=New-Object System.Net.Http.Headers.AuthenticationHeaderValue('Bearer',$Token)

# Returns @{Code;Body}; throws unless the status is 2xx or listed in -Allow.
function Invoke-Api([string]$Method,[string]$Path,$Content=$null,[int[]]$Allow=@()){
    $request=New-Object System.Net.Http.HttpRequestMessage((New-Object System.Net.Http.HttpMethod($Method)),"$base$Path")
    if($Content){$request.Content=$Content}
    $response=$http.SendAsync($request).GetAwaiter().GetResult()
    $body=$response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
    $code=[int]$response.StatusCode
    if(-not $response.IsSuccessStatusCode -and $Allow -notcontains $code){
        $message=$body
        try{$message=($body | ConvertFrom-Json).error}catch{}
        throw "$Method $Path -> $code $message"
    }
    return @{Code=$code;Body=$body}
}
function Json($Object){
    $text=$Object | ConvertTo-Json -Compress -Depth 5
    return New-Object System.Net.Http.StringContent($text,[Text.Encoding]::UTF8,'application/json')
}
function Send-File([string]$Path,[string]$LocalFile,[string]$PlatformValue){
    $form=New-Object System.Net.Http.MultipartFormDataContent
    if($PlatformValue){$form.Add((New-Object System.Net.Http.StringContent($PlatformValue)),'platform')}
    $stream=[IO.File]::OpenRead($LocalFile)
    try{
        $part=New-Object System.Net.Http.StreamContent($stream)
        $part.Headers.ContentType=New-Object System.Net.Http.Headers.MediaTypeHeaderValue('application/octet-stream')
        $form.Add($part,'file',[IO.Path]::GetFileName($LocalFile))
        Invoke-Api 'POST' $Path $form | Out-Null
    }finally{$stream.Dispose();$form.Dispose()}
}
function Set-CiOutput([string]$Key,[string]$Value){
    if($env:GITHUB_OUTPUT){Add-Content -LiteralPath $env:GITHUB_OUTPUT -Value "$Key=$Value" -Encoding utf8}
}

# 1. Program: created on first deployment.
$r=Invoke-Api 'POST' '/api/v1/admin/programs' (Json @{slug=$Program;name=$Name}) -Allow @(409)
if($r.Code -eq 201){Write-Output "Created program $Program ($Name)"}

$status=''
if($File){
    if(-not(Test-Path -LiteralPath $File)){throw "File not found: $File"}
    $filePath=(Resolve-Path -LiteralPath $File).Path
    if(-not $Version){
        $versionFile=Join-Path (Split-Path -Parent $filePath) 'version.txt'
        $onWindows=($PSVersionTable.PSEdition -eq 'Desktop') -or $IsWindows
        if(Test-Path -LiteralPath $versionFile){$Version=([IO.File]::ReadAllText($versionFile)).Trim().TrimStart([char]0xFEFF)}
        elseif($onWindows){$Version=[Diagnostics.FileVersionInfo]::GetVersionInfo($filePath).FileVersion}
    }
    if(-not $Version){throw "Version unknown: pass -Version or put version.txt next to $filePath"}
    $Version=$Version.Trim()
    if($Version -notmatch '^\d+(\.\d+){1,3}(-[0-9A-Za-z.-]+)?$'){throw "Invalid version '$Version' (expected e.g. 1.0.0.57)"}
    if($Version -match '^\d+\.\d+\.\d+\.0$'){Write-Warning "Version $Version has no build number; clients only update to a higher version than they run."}
    if($NotesFile){$Notes=[IO.File]::ReadAllText((Resolve-Path -LiteralPath $NotesFile).Path)}
    elseif(-not $Notes -and $NotesFromGit){
        $range=$NotesFromGit; if($range -notmatch '\.\.'){$range="$range..HEAD"}
        $log=& git log --no-merges '--pretty=format:- %s' $range
        if($LASTEXITCODE -ne 0){throw "git log $range failed"}
        $Notes=(($log | Out-String).Trim()); if(-not $Notes){$Notes='- 변경 사항 없음'}
    }
    $hash=(Get-FileHash -LiteralPath $filePath -Algorithm SHA256).Hash.ToLowerInvariant()
    $releases="/api/v1/admin/programs/$slug/releases"
    $releasePath="$releases/$([Uri]::EscapeDataString($Version))"

    # 2. Existing version?
    $r=Invoke-Api 'GET' $releasePath -Allow @(404)
    if($r.Code -eq 404){
        $r=Invoke-Api 'POST' $releases (Json @{version=$Version;channel=$Channel;notes=$Notes;mandatory=[bool]$Mandatory})
        Write-Output "Created version $Program $Version"
    }
    $release=$r.Body | ConvertFrom-Json
    $same=$false
    foreach($asset in @($release.assets)){
        if(-not $asset -or $asset.platform -ne $Platform){continue}
        if($asset.sha256 -eq $hash){$same=$true}
        elseif($release.status -eq 'draft'){
            Invoke-Api 'DELETE' "/api/v1/admin/assets/$($asset.id)" | Out-Null
            Write-Output "Replaced $($asset.filename) ($Platform) in draft $Version"
        }
    }

    # 3. Decide by status.
    if($release.status -eq 'published'){
        if(-not $same){throw "$Program $Version is already published with a different file (SHA-256 $hash). Published versions are immutable: rebuild with a new version number."}
        $status='already-published'
        Write-Output "$Program $Version is already published with the same file; nothing to do."
    }elseif($release.status -eq 'yanked'){
        throw "$Program $Version was withdrawn (yanked); publish a new version number."
    }else{
        if(-not $Notes){$Notes=[string]$release.notes}
        Invoke-Api 'PUT' $releasePath (Json @{channel=$Channel;notes=$Notes;mandatory=([bool]$Mandatory -or [bool]$release.mandatory)}) | Out-Null
        if(-not $same){
            Send-File "$releasePath/assets" $filePath $Platform
            Write-Output "Uploaded $([IO.Path]::GetFileName($filePath)) ($Platform) as $Program $Version"
        }
        if($Draft){
            $status='draft'
            Write-Output "Left $Program $Version as draft; publish it in the admin page."
        }else{
            Invoke-Api 'POST' "$releasePath/publish" | Out-Null
            $status='published'
            Write-Output "Published $Program $Version"
        }
    }
    Write-Output "latest.json: $latestJson"
    Set-CiOutput 'version' $Version
    Set-CiOutput 'status' $status
    Set-CiOutput 'latest_json' $latestJson
}

foreach($fixed in $FixedFile){
    if(-not(Test-Path -LiteralPath $fixed)){throw "File not found: $fixed"}
    $fixedName=[IO.Path]::GetFileName($fixed)
    Send-File "/api/v1/admin/programs/$slug/files" (Resolve-Path -LiteralPath $fixed).Path ''
    Write-Output "Uploaded fixed file $fixedName -> $base/$Program/$fixedName"
}
$http.Dispose()
