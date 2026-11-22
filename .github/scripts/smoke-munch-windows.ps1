[CmdletBinding(DefaultParameterSetName = 'Binary')]
param(
    [Parameter(Mandatory, ParameterSetName = 'Binary')][string]$Binary,
    [Parameter(Mandatory, ParameterSetName = 'Release')][string]$AssetDirectory,
    [Parameter(Mandatory, ParameterSetName = 'Release')][string]$Version
)

$ErrorActionPreference = 'Stop'
if ($PSCmdlet.ParameterSetName -eq 'Release') {
    $AssetDirectory = (Resolve-Path -LiteralPath $AssetDirectory).ProviderPath
} else {
    $Binary = (Resolve-Path -LiteralPath $Binary).ProviderPath
}
$root = Join-Path ([IO.Path]::GetTempPath()) ('munch smoke ' + [Guid]::NewGuid())
$null = New-Item -ItemType Directory -Path $root
$savedEnv = @{}
$listen = $null
$needsStop = $false

function Invoke-Munch {
    & $Binary @args
    if ($LASTEXITCODE -ne 0) {
        throw "munch $($args[0]) failed with exit code $LASTEXITCODE"
    }
}

Push-Location -LiteralPath $root
try {
    if ($PSCmdlet.ParameterSetName -eq 'Release') {
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $checksums = Get-Content -LiteralPath (Join-Path $AssetDirectory 'SHA256SUMS')
        foreach ($arch in 'amd64', 'arm64') {
            $name = "munch-$Version-windows-$arch.zip"
            $path = Join-Path $AssetDirectory $name
            $hash = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash
            if ($checksums -notcontains "$hash  $name") {
                throw "SHA256SUMS does not match $name"
            }
            $zip = [IO.Compression.ZipFile]::OpenRead($path)
            try {
                if ($zip.Entries.Count -ne 1 -or $zip.Entries[0].FullName -cne 'munch.exe') {
                    throw "$name must contain only munch.exe at its root"
                }
            } finally {
                $zip.Dispose()
            }
            if ($arch -eq 'amd64') {
                Expand-Archive -LiteralPath $path -DestinationPath $root
                $Binary = Join-Path $root 'munch.exe'
            }
        }
    }

    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $listener.Start()
    try {
        $listen = '127.0.0.1:' + $listener.LocalEndpoint.Port
    } finally {
        $listener.Stop()
    }

    $overrides = @{
        APPDATA = Join-Path $root 'Roaming'
        LOCALAPPDATA = Join-Path $root 'Local'
        USERPROFILE = Join-Path $root 'Profile'
        TMPROXY_CONFIG_DIR = $null
        TMPROXY_STATE_DIR = $null
        TMPROXY_LISTEN = $listen
        TOKENMUNCHERS_KEY = 'tm_' + ('a' * 43)
        TOKENMUNCHERS_URL = "http://$listen/ingest"
        # Do not discover or run the runner's Oh My Pi, if installed.
        PATH = Join-Path $root 'EmptyPath'
    }
    foreach ($name in 'APPDATA', 'LOCALAPPDATA', 'USERPROFILE', 'PATH') {
        $null = New-Item -ItemType Directory -Path $overrides[$name]
    }
    foreach ($name in $overrides.Keys) {
        $savedEnv[$name] = [Environment]::GetEnvironmentVariable($name)
        [Environment]::SetEnvironmentVariable($name, $overrides[$name], 'Process')
    }

    $reportedVersion = Invoke-Munch version
    if ($reportedVersion -notlike 'munch * (windows/amd64)' -or
        ($Version -and $reportedVersion -cne "munch $Version (windows/amd64)")) {
        throw "Unexpected version: $reportedVersion"
    }

    $needsStop = $true
    Invoke-Munch install --yes --no-shell --no-codex
    $env:TOKENMUNCHERS_KEY = $null
    if (-not (Test-Path -LiteralPath (Join-Path $env:APPDATA 'tmproxy\config.json') -PathType Leaf)) {
        throw 'Install did not save its configuration under APPDATA'
    }
    $status = Invoke-Munch status --json | ConvertFrom-Json
    if (-not $status.running -or $status.service.backend -ne 'pidfile' -or $status.service.installed) {
        throw 'Install did not start the fallback daemon without registering a service'
    }
    Invoke-Munch stop
    $needsStop = $false
    $status = Invoke-Munch status --json | ConvertFrom-Json
    if ($status.running -or $status.service.running) {
        throw 'Daemon is still responding after stop'
    }

    $needsStop = $true
    Invoke-Munch start
    $childOutput = Invoke-Munch exec '--' "$env:SystemRoot\System32\cmd.exe" /d /c set OPENAI_BASE_URL
    if ($childOutput -notmatch ('^OPENAI_BASE_URL=http://' + [regex]::Escape($listen) + '/_tm/[a-f0-9]{64}/openai/v1$')) {
        throw 'munch exec did not give the child the local proxy route'
    }
    Invoke-Munch stop
    $needsStop = $false
} finally {
    try {
        if ($needsStop) {
            Invoke-Munch stop
        }
    } catch {
        Write-Warning "Smoke cleanup could not stop munch: $_"
    } finally {
        foreach ($name in $savedEnv.Keys) {
            [Environment]::SetEnvironmentVariable($name, $savedEnv[$name], 'Process')
        }
        Pop-Location
        Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
    }
}
