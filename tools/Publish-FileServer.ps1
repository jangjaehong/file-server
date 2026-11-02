[CmdletBinding()]
param(
    # File server URL. Default: $env:FS_SERVER -> the server used last on this PC -> http://211.231.121.41:9100
    [string]$Server='',
    # API token (관리자 → API 토큰). Default: $env:FS_TOKEN -> token saved on this PC -> asked once and saved.
    [string]$Token='',
    # Program slug = folder in the update URL: onetaskbar, ics-scm
    [string]$Program='',
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
    # Unversioned files (e.g. defaults.json, a setup that always has the same name), replaced on every upload.
    [string[]]$FixedFile=@(),
    # Allow publishing a version lower than the newest one on the server (maintenance builds).
    [switch]$AllowOlder,
    # Save the token for this PC (verifies it first). Without -File/-FixedFile it only saves.
    [switch]$SaveToken,
    # Delete the token saved for the server and exit.
    [switch]$ForgetToken,
    # Exit 0 without doing anything when no token is available (CI without secrets).
    [switch]$SkipIfUnconfigured
)
# Publishes a build from this PC to the file server. Safe to re-run:
#  - the program is created on first deployment;
#  - the newest version on the server is shown; an older version is refused (-AllowOlder to override);
#  - a missing version is created, uploaded (with progress) and published;
#  - a draft version is resumed (a file of the same platform with another SHA-256 is replaced);
#  - a published version with the same SHA-256 -> "already published", exit 0;
#    with a different SHA-256 -> error (published versions are immutable; build a new version number);
#  - finally the server's latest.json is printed.
# Token store (shared by every program on this PC):
#   Windows: %APPDATA%\FileServer\credentials.json, token encrypted with DPAPI (this Windows user only)
#   other:   ~/.config/fileserver/credentials.json, mode 600
# Windows PowerShell 5.1 and PowerShell 7 (Windows / Linux). Same publish rules as `fsctl publish`.
$ErrorActionPreference='Stop'
$DefaultServer='http://211.231.121.41:9100'
$onWindows=($PSVersionTable.PSEdition -eq 'Desktop') -or $IsWindows

# ---------------------------------------------------------------- token store
function Get-StorePath{
    if($onWindows){return Join-Path $env:APPDATA 'FileServer\credentials.json'}
    return Join-Path $HOME '.config/fileserver/credentials.json'
}
function Read-Store{
    $path=Get-StorePath
    $store=[pscustomobject]@{lastServer='';tokens=@()}
    if(Test-Path -LiteralPath $path){
        try{
            $data=Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
            if($data.lastServer){$store.lastServer=[string]$data.lastServer}
            if($data.tokens){$store.tokens=@($data.tokens | Where-Object {$_ -and $_.server})}
        }catch{Write-Warning "Ignoring unreadable token store ${path}: $_"}
    }
    return $store
}
function Write-Store($Store){
    $path=Get-StorePath
    $dir=Split-Path -Parent $path
    if(-not(Test-Path -LiteralPath $dir)){New-Item -ItemType Directory -Path $dir -Force | Out-Null}
    $json=[pscustomobject]@{lastServer=$Store.lastServer;tokens=@($Store.tokens)} | ConvertTo-Json -Depth 5
    [IO.File]::WriteAllText($path,$json,(New-Object Text.UTF8Encoding($false)))
    if(-not $onWindows){& chmod 600 $path}
}
function Get-ServerKey([string]$Url){return $Url.Trim().TrimEnd('/').ToLowerInvariant()}
function Get-SavedToken($Store,[string]$Url){
    $entry=@($Store.tokens | Where-Object {$_.server -eq (Get-ServerKey $Url)}) | Select-Object -First 1
    if(-not $entry){return ''}
    try{
        if($entry.protection -eq 'dpapi'){
            $secure=ConvertTo-SecureString ([string]$entry.value)
            return (New-Object System.Net.NetworkCredential('',$secure)).Password
        }
        return [string]$entry.value
    }catch{
        Write-Warning "Saved token for $Url cannot be read (another Windows user or PC?): $_"
        return ''
    }
}
function Set-SavedToken($Store,[string]$Url,[string]$Value){
    $key=Get-ServerKey $Url
    if($onWindows){$entry=[pscustomobject]@{server=$key;protection='dpapi';value=(ConvertTo-SecureString $Value -AsPlainText -Force | ConvertFrom-SecureString)}}
    else{$entry=[pscustomobject]@{server=$key;protection='none';value=$Value}}
    $Store.tokens=@(@($Store.tokens | Where-Object {$_.server -ne $key})+$entry)
}

# ---------------------------------------------------------------- server and token
$store=Read-Store
if(-not $Server){$Server=$env:FS_SERVER}
if(-not $Server){$Server=$store.lastServer}
if(-not $Server){$Server=$DefaultServer}
$base=$Server.Trim().TrimEnd('/')

if($ForgetToken){
    $key=Get-ServerKey $base
    $store.tokens=@($store.tokens | Where-Object {$_.server -ne $key})
    Write-Store $store
    Write-Output "Forgot the token saved for $base"
    exit 0
}
if(-not $Program -and -not $SaveToken){throw '-Program is required (e.g. onetaskbar, ics-scm)'}
if($Program -and -not $File -and $FixedFile.Count -eq 0 -and -not $SaveToken){throw 'Nothing to publish: give -File and/or -FixedFile'}

$tokenSource='parameter'
if(-not $Token){$Token=$env:FS_TOKEN;$tokenSource='env'}
if(-not $Token){$Token=Get-SavedToken $store $base;$tokenSource='saved'}
if(-not $Token){
    if($SkipIfUnconfigured){Write-Output 'No file-server token (FS_TOKEN or saved token); skipping publish';exit 0}
    if($env:CI -or $env:GITHUB_ACTIONS -or -not [Environment]::UserInteractive){throw "No API token for $base. Pass -Token or set FS_TOKEN."}
    Write-Host "API 토큰을 입력하세요 ($base/#/admin → API 토큰). 이 PC의 현재 사용자 계정에 암호화해 저장하고 다음부터는 묻지 않습니다."
    $secure=Read-Host -AsSecureString 'API 토큰'
    $Token=(New-Object System.Net.NetworkCredential('',$secure)).Password.Trim()
    if(-not $Token){throw 'No token entered'}
    $tokenSource='prompt'
}

Add-Type -AssemblyName System.Net.Http
$http=New-Object System.Net.Http.HttpClient
$http.Timeout=[TimeSpan]::FromSeconds(30)
$http.DefaultRequestHeaders.Authorization=New-Object System.Net.Http.Headers.AuthenticationHeaderValue('Bearer',$Token)

# Returns @{Code;Body}; throws unless the status is 2xx or listed in -Allow.
function Invoke-Api([string]$Method,[string]$Path,$Content=$null,[int[]]$Allow=@()){
    $request=New-Object System.Net.Http.HttpRequestMessage((New-Object System.Net.Http.HttpMethod($Method)),"$base$Path")
    if($Content){$request.Content=$Content}
    try{$response=$http.SendAsync($request).GetAwaiter().GetResult()}
    catch{throw "Cannot reach the file server $base ($($_.Exception.Message)). Is this PC on the company network and the server running?"}
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
# Streams a multipart upload and shows progress (HttpClient has no upload progress in PS 5.1).
function Send-File([string]$Path,[string]$LocalFile,[string]$PlatformValue){
    $boundary='----fileserver'+[Guid]::NewGuid().ToString('N')
    $fileName=[IO.Path]::GetFileName($LocalFile)
    $utf8=New-Object Text.UTF8Encoding($false)
    $head=''
    if($PlatformValue){$head+="--$boundary`r`nContent-Disposition: form-data; name=`"platform`"`r`n`r`n$PlatformValue`r`n"}
    $head+="--$boundary`r`nContent-Disposition: form-data; name=`"file`"; filename=`"$fileName`"`r`nContent-Type: application/octet-stream`r`n`r`n"
    $headBytes=$utf8.GetBytes($head)
    $tailBytes=$utf8.GetBytes("`r`n--$boundary--`r`n")
    $stream=[IO.File]::OpenRead($LocalFile)
    $activity="Uploading $fileName"
    try{
        $total=$stream.Length
        $request=[Net.HttpWebRequest]::Create("$base$Path")
        $request.Method='POST'
        $request.ContentType="multipart/form-data; boundary=$boundary"
        $request.ContentLength=$headBytes.Length+$total+$tailBytes.Length
        $request.AllowWriteStreamBuffering=$false
        $request.Timeout=1800000
        $request.ReadWriteTimeout=1800000
        $request.Headers['Authorization']="Bearer $Token"
        try{$out=$request.GetRequestStream()}
        catch{throw "Cannot reach the file server $base ($($_.Exception.Message))"}
        $out.Write($headBytes,0,$headBytes.Length)
        $buffer=New-Object byte[] 1048576
        $sent=[long]0; $lastPercent=-1
        while(($read=$stream.Read($buffer,0,$buffer.Length)) -gt 0){
            $out.Write($buffer,0,$read)
            $sent+=$read
            $percent=[int](100*$sent/[Math]::Max($total,1))
            if($percent -ne $lastPercent){
                Write-Progress -Activity $activity -Status ("{0}% ({1:N1} / {2:N1} MB)" -f $percent,($sent/1MB),($total/1MB)) -PercentComplete $percent
                $lastPercent=$percent
            }
        }
        $out.Write($tailBytes,0,$tailBytes.Length)
        $out.Close()
        try{$request.GetResponse().Close()}
        catch [Net.WebException]{
            $response=$_.Exception.Response
            if(-not $response){throw}
            $body=(New-Object IO.StreamReader($response.GetResponseStream())).ReadToEnd()
            $message=$body
            try{$message=($body | ConvertFrom-Json).error}catch{}
            throw "POST $Path -> $([int]$response.StatusCode) $message"
        }
    }finally{
        $stream.Dispose()
        Write-Progress -Activity $activity -Completed
    }
}
function Set-CiOutput([string]$Key,[string]$Value){
    if($env:GITHUB_OUTPUT){Add-Content -LiteralPath $env:GITHUB_OUTPUT -Value "$Key=$Value" -Encoding utf8}
}
# Compares dotted versions with an optional "-pre" suffix (1.0.0.57, 2.0.0-beta.1). Returns -1, 0, 1.
function Compare-Version([string]$A,[string]$B){
    $ca,$pa=$A.Split('-',2); $cb,$pb=$B.Split('-',2)
    $na=@($ca.Split('.') | ForEach-Object {[long]$_}); $nb=@($cb.Split('.') | ForEach-Object {[long]$_})
    for($i=0;$i -lt 4;$i++){
        $x=0; $y=0
        if($i -lt $na.Count){$x=$na[$i]}
        if($i -lt $nb.Count){$y=$nb[$i]}
        if($x -ne $y){if($x -lt $y){return -1}else{return 1}}
    }
    if(-not $pa -and -not $pb){return 0}
    if(-not $pa){return 1}
    if(-not $pb){return -1}
    return [Math]::Sign([string]::CompareOrdinal($pa,$pb))
}

# 0. Token check (gives a clear message for a revoked token).
$r=Invoke-Api 'GET' '/api/v1/auth/me' -Allow @(401)
if($r.Code -eq 401){
    if($tokenSource -eq 'saved'){throw "The token saved for $base was rejected (401: revoked?). Run with -ForgetToken, then publish again to enter a new one."}
    throw "The API token was rejected by $base (401). Create one in $base/#/admin → API 토큰."
}
if($tokenSource -eq 'prompt' -or $SaveToken){
    Set-SavedToken $store $base $Token
    Write-Output "Token saved for $base ($(Get-StorePath))"
}
if($tokenSource -ne 'env' -or $SaveToken){
    $store.lastServer=$base
    Write-Store $store
}
if($SaveToken -and -not $File -and $FixedFile.Count -eq 0){$http.Dispose();exit 0}

$slug=[Uri]::EscapeDataString($Program)
$latestJson="$base/$Program/latest.json"
if(-not $Name){$Name=$Program}
$status=''

if($File){
    if(-not(Test-Path -LiteralPath $File)){throw "File not found: $File"}
    $filePath=(Resolve-Path -LiteralPath $File).Path
    if(-not $Version){
        $versionFile=Join-Path (Split-Path -Parent $filePath) 'version.txt'
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
    $size=(Get-Item -LiteralPath $filePath).Length

    # 1. What is on the server now?
    $r=Invoke-Api 'GET' "/api/v1/programs/$slug" -Allow @(404)
    $newest=''
    if($r.Code -eq 404){
        Write-Output "Server: $Program does not exist yet -> first deployment"
    }else{
        $info=$r.Body | ConvertFrom-Json
        foreach($rel in @($info.releases)){if($rel -and (-not $newest -or (Compare-Version $rel.version $newest) -gt 0)){$newest=$rel.version}}
        $stable='-'; $beta=''
        if($info.latest){$stable=$info.latest.version}
        if($info.latest_beta){$beta=", beta $($info.latest_beta.version)"}
        Write-Output "Server: $Program stable $stable$beta"
    }
    Write-Output ("Local:  {0} {1} ({2:N1} MB, sha256 {3})" -f [IO.Path]::GetFileName($filePath),$Version,($size/1MB),$hash.Substring(0,12))
    if($newest -and (Compare-Version $Version $newest) -lt 0 -and -not $AllowOlder){
        throw "Version $Version is older than $newest already published on the server. Build a newer version (or pass -AllowOlder for a maintenance build)."
    }

    # 2. Program: created on first deployment.
    $r=Invoke-Api 'POST' '/api/v1/admin/programs' (Json @{slug=$Program;name=$Name}) -Allow @(409)
    if($r.Code -eq 201){Write-Output "Created program $Program ($Name)"}

    # 3. Existing version?
    $releases="/api/v1/admin/programs/$slug/releases"
    $releasePath="$releases/$([Uri]::EscapeDataString($Version))"
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

    # 4. Decide by status.
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
    Set-CiOutput 'version' $Version
    Set-CiOutput 'status' $status
    Set-CiOutput 'latest_json' $latestJson
}else{
    $r=Invoke-Api 'POST' '/api/v1/admin/programs' (Json @{slug=$Program;name=$Name}) -Allow @(409)
    if($r.Code -eq 201){Write-Output "Created program $Program ($Name)"}
}

foreach($fixed in $FixedFile){
    if(-not(Test-Path -LiteralPath $fixed)){throw "File not found: $fixed"}
    $fixedName=[IO.Path]::GetFileName($fixed)
    Send-File "/api/v1/admin/programs/$slug/files" (Resolve-Path -LiteralPath $fixed).Path ''
    Write-Output "Uploaded fixed file $fixedName -> $base/$Program/$fixedName"
}

if($File){
    $r=Invoke-Api 'GET' "/$slug/latest.json" -Allow @(404)
    if($r.Code -eq 200){Write-Output "latest.json: $latestJson"; Write-Output $r.Body.Trim()}
    else{Write-Output "latest.json: $latestJson (none yet: no published stable Windows version)"}
}
$http.Dispose()
