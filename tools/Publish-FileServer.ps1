[CmdletBinding()]
param(
    # File server base URL, e.g. http://211.231.121.41:9100
    [Parameter(Mandatory=$true)][string]$Server,
    # API token created in the admin page (관리자 → API 토큰). Can also come from $env:FS_TOKEN.
    [string]$Token=$env:FS_TOKEN,
    # Program slug = folder in the update URL (onetaskbar, ics-scm).
    [Parameter(Mandatory=$true)][string]$Program,
    # Display name; when given, the program is created if it does not exist yet.
    [string]$Name='',
    # The executable / setup to publish as a new version.
    [string]$File='',
    # Version; by default read from version.txt next to the file, else from the exe's FileVersion.
    [string]$Version='',
    [string]$Notes='',
    [string]$NotesFile='',
    [string]$Platform='windows-amd64',
    [ValidateSet('stable','beta')][string]$Channel='stable',
    [switch]$Mandatory,
    # Upload only; publish later from the admin page.
    [switch]$Draft,
    # Files that are not versioned and are replaced on every upload (e.g. defaults.json).
    [string[]]$FixedFile=@()
)
# Publishes a build to the file server (replaces copying into the IIS folder).
# Works with Windows PowerShell 5.1 and PowerShell 7.
#
#   .\Publish-FileServer.ps1 -Server http://211.231.121.41:9100 -Token fst_... -Program onetaskbar `
#       -File release\OneTaskBar-Setup.exe -Notes "알림 개선"
#   .\Publish-FileServer.ps1 -Server http://211.231.121.41:9100 -Program onetaskbar -FixedFile defaults.json
$ErrorActionPreference='Stop'
Add-Type -AssemblyName System.Net.Http
if(-not $Token){throw 'API token required: -Token or $env:FS_TOKEN (관리자 페이지 → API 토큰)'}
if(-not $File -and $FixedFile.Count -eq 0){throw 'Nothing to publish: give -File and/or -FixedFile'}
$base=$Server.TrimEnd('/')
$slug=[Uri]::EscapeDataString($Program)

$http=New-Object System.Net.Http.HttpClient
$http.Timeout=[TimeSpan]::FromMinutes(30)
$http.DefaultRequestHeaders.Authorization=New-Object System.Net.Http.Headers.AuthenticationHeaderValue('Bearer',$Token)

function Invoke-Api([string]$Method,[string]$Path,$Content,[int[]]$Allow=@()){
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
    return $code
}
function Json($Object){
    $text=$Object | ConvertTo-Json -Compress
    return New-Object System.Net.Http.StringContent($text,[Text.Encoding]::UTF8,'application/json')
}
function Upload([string]$Path,[string]$LocalFile,[string]$PlatformValue){
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

if($Name){
    $code=Invoke-Api 'POST' '/api/v1/admin/programs' (Json @{slug=$Program;name=$Name}) @(409)
    if($code -eq 201){Write-Output "Created program $Program ($Name)"}
}

if($File){
    if(-not(Test-Path -LiteralPath $File)){throw "File not found: $File"}
    $filePath=(Resolve-Path -LiteralPath $File).Path
    if(-not $Version){
        $versionFile=Join-Path (Split-Path -Parent $filePath) 'version.txt'
        if(Test-Path -LiteralPath $versionFile){$Version=(Get-Content -LiteralPath $versionFile -Raw).Trim()}
        else{$Version=[Diagnostics.FileVersionInfo]::GetVersionInfo($filePath).FileVersion}
    }
    if($Version -notmatch '^\d+(\.\d+){1,3}(-[0-9A-Za-z.-]+)?$'){throw "Invalid version '$Version' (expected e.g. 1.0.0.57)"}
    if($Version -match '^\d+\.\d+\.\d+\.0$'){Write-Warning "Version $Version has no build number; clients only update to a higher version than they run."}
    if($NotesFile){$Notes=[IO.File]::ReadAllText((Resolve-Path -LiteralPath $NotesFile).Path)}
    $release="/api/v1/admin/programs/$slug/releases"
    $code=Invoke-Api 'POST' $release (Json @{version=$Version;channel=$Channel;notes=$Notes;mandatory=[bool]$Mandatory}) @(409)
    if($code -eq 409){Write-Warning "Version $Version already exists; uploading into it works only while it is a draft."}
    $releasePath="$release/$([Uri]::EscapeDataString($Version))"
    Upload "$releasePath/assets" $filePath $Platform
    Write-Output "Uploaded $([IO.Path]::GetFileName($filePath)) ($Platform) as $Program $Version"
    if($Draft){Write-Output "Left as draft: publish it in the admin page"}
    else{
        Invoke-Api 'POST' "$releasePath/publish" $null | Out-Null
        Write-Output "Published $Program $Version -> $base/$Program/latest.json"
    }
}

foreach($fixed in $FixedFile){
    if(-not(Test-Path -LiteralPath $fixed)){throw "File not found: $fixed"}
    Upload "/api/v1/admin/programs/$slug/files" (Resolve-Path -LiteralPath $fixed).Path ''
    Write-Output "Uploaded fixed file $([IO.Path]::GetFileName($fixed)) -> $base/$Program/$([IO.Path]::GetFileName($fixed))"
}
$http.Dispose()
