[CmdletBinding()]
param(
    # SSH target of the Docker server, e.g. deploy@211.231.121.41
    [string]$Server='',
    # Folder on the server holding docker-compose.yml and .env
    [string]$RemoteDir='~/file-server',
    # Target CPU of the server
    [string]$Platform='linux/amd64',
    # Extra SSH/SCP options, e.g. @('-i','C:\Users\me\.ssh\id_ed25519','-p','2222')
    [string[]]$SshOptions=@(),
    # Only build and save file-server-image.tar (no copy / deploy)
    [switch]$SkipUpload
)
# Builds the file-server Docker image on this PC, saves it to a file, copies it to the
# server and starts it there (no registry, no internet needed on the server).
#
#   .\tools\docker\Deploy-Image.ps1 -Server deploy@211.231.121.41
#
# First run: copies docker-compose.yml and .env.example; when the server has no .env yet it
# creates one from .env.example and stops so you can edit it, then run the script again.
# Every run keeps the previous image as file-server:before-update (rollback:
#   docker tag file-server:before-update file-server:latest && docker compose up -d --no-build).
$ErrorActionPreference='Stop'
$root=Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Push-Location $root
try{
    if(-not $SkipUpload -and -not $Server){throw '-Server is required (e.g. deploy@211.231.121.41), or use -SkipUpload'}
    $tag=Get-Date -Format 'yyyyMMdd-HHmm'
    $tar=Join-Path $root 'file-server-image.tar'

    Write-Output "== build file-server:$tag ($Platform)"
    & docker build --platform $Platform -t file-server:latest -t "file-server:$tag" .
    if($LASTEXITCODE -ne 0){throw 'docker build failed'}

    Write-Output "== save $tar"
    # -o, not a pipe: PowerShell pipes would corrupt the binary stream.
    & docker save -o $tar file-server:latest "file-server:$tag"
    if($LASTEXITCODE -ne 0){throw 'docker save failed'}
    Write-Output ("   {0:N1} MB" -f ((Get-Item -LiteralPath $tar).Length/1MB))
    if($SkipUpload){Write-Output "Image saved; copy it yourself or run without -SkipUpload."; return}

    Write-Output "== copy to ${Server}:$RemoteDir"
    & ssh @SshOptions $Server "mkdir -p $RemoteDir"
    if($LASTEXITCODE -ne 0){throw "ssh $Server failed"}
    $scpOptions=@($SshOptions | ForEach-Object {if($_ -ceq '-p'){'-P'}else{$_}})   # scp uses -P for the port
    & scp @scpOptions $tar (Join-Path $root 'docker-compose.yml') (Join-Path $root '.env.example') "${Server}:$RemoteDir/"
    if($LASTEXITCODE -ne 0){throw 'scp failed'}

    & ssh @SshOptions $Server "test -f $RemoteDir/.env"
    if($LASTEXITCODE -ne 0){
        & ssh @SshOptions $Server "cp $RemoteDir/.env.example $RemoteDir/.env"
        Write-Warning "Created $RemoteDir/.env on the server from .env.example. Edit it (FS_PORT, FS_ADMIN_PASSWORD, FS_BASE_URL), then run this script again."
        Write-Output "   ssh $Server   then   vi $RemoteDir/.env"
        return
    }

    Write-Output '== load and restart on the server'
    # One line, no double quotes: Windows PowerShell 5.1 mangles embedded quotes in native arguments.
    $remote='set -e; cd '+$RemoteDir+'; ' +
        'if docker image inspect file-server:latest >/dev/null 2>&1; then docker tag file-server:latest file-server:before-update; fi; ' +
        'docker load -i file-server-image.tar; docker compose up -d --no-build; rm -f file-server-image.tar; ' +
        'for i in $(seq 1 30); do s=$(docker inspect -f {{.State.Health.Status}} file-server 2>/dev/null || true); [ x$s = xhealthy ] && break; sleep 2; done; ' +
        'docker compose ps; [ x$s = xhealthy ]'
    & ssh @SshOptions $Server $remote
    if($LASTEXITCODE -ne 0){
        throw "Deployment did not become healthy. Check: ssh $Server 'cd $RemoteDir && docker compose logs --tail 50'. Roll back: docker tag file-server:before-update file-server:latest && docker compose up -d --no-build"
    }
    Remove-Item -LiteralPath $tar -Force
    Write-Output "Deployed file-server:$tag to $Server (previous image kept as file-server:before-update)"
}finally{
    Pop-Location
}
