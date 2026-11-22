# Install the latest stable Munch CLI for the current Windows user.
# One-liner (paste into a PowerShell window):
#   irm https://raw.githubusercontent.com/hndrxc/tokenmunchers/main/install-munch.ps1 | iex
# Or download the script and run it directly:
#   & .\install-munch.ps1
# -DownloadOnly installs the binary and updates PATH without running setup:
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/hndrxc/tokenmunchers/main/install-munch.ps1))) -DownloadOnly
param([switch]$DownloadOnly)

# Run everything in a child scope so helper functions, preference variables, and
# working variables do not leak into the caller's session (important with `irm ... | iex`).
& {
    param([switch]$DownloadOnly)

    $ErrorActionPreference = 'Stop'
    # Keep the native setup exit code available on PowerShell versions that support this preference.
    $PSNativeCommandUseErrorActionPreference = $false
    $ProgressPreference = 'SilentlyContinue'

    function Get-MunchPath([string]$PathValue, [string]$Directory) {
        $entries = @(foreach ($entry in ($PathValue -split ';')) {
            if ($entry.Length -eq 0) { continue }
            $expanded = [Environment]::ExpandEnvironmentVariables($entry.Trim().Trim('"'))
            if ($expanded.Replace('/', '\').TrimEnd('\') -ine $Directory.TrimEnd('\')) {
                $entry
            }
        })
        return (@($Directory) + $entries) -join ';'
    }

    function Find-MunchTag {
        $api = 'https://api.github.com/repos/hndrxc/tokenmunchers/releases'
        $headers = @{
            Accept = 'application/vnd.github+json'
            'X-GitHub-Api-Version' = '2022-11-28'
        }
        $pattern = '\Amunch-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\z'
        for ($page = 1; ; $page++) {
            $response = Invoke-WebRequest -UseBasicParsing -Headers $headers -Uri "${api}?per_page=100&page=$page"
            if ($response.Content -notmatch '\A\s*\[') { throw 'Invalid release list from GitHub.' }
            $releases = ConvertFrom-Json -InputObject $response.Content
            if (-not $releases) { throw 'No stable Munch release found on GitHub.' }
            foreach ($release in $releases) {
                if ($release.draft -is [bool] -and -not $release.draft -and
                    $release.prerelease -is [bool] -and -not $release.prerelease -and
                    $release.tag_name -is [string] -and $release.tag_name -cmatch $pattern) {
                    return $release.tag_name
                }
            }
        }
    }

    $temp = $null
    $stage = $null
    $pathKey = $null
    $userPathChanged = $false
    $installed = $false
    $oldTls = [Net.ServicePointManager]::SecurityProtocol
    $step = 'detecting Windows architecture'
    try {
        if (-not [Environment]::Is64BitOperatingSystem) { throw 'Munch requires 64-bit Windows.' }
        # CIM reports the processor architecture even in an emulated PowerShell process.
        $architectures = @(Get-CimInstance -ClassName Win32_Processor |
            Select-Object -ExpandProperty Architecture -Unique)
        if ($architectures.Count -ne 1) { throw 'Could not determine the native Windows architecture.' }
        switch ($architectures[0]) {
            9 { $arch = 'amd64' }
            12 { $arch = 'arm64' }
            default { throw "Unsupported Windows processor architecture: $($architectures[0])." }
        }

        # Windows PowerShell 5.1 may otherwise try TLS 1.0. Restore the caller's setting below.
        [Net.ServicePointManager]::SecurityProtocol = $oldTls -bor [Net.SecurityProtocolType]::Tls12
        $step = 'resolving the latest stable Munch release'
        $tag = Find-MunchTag
        $version = $tag.Substring('munch-v'.Length)
        $archive = "munch-$version-windows-$arch.zip"
        $baseUrl = "https://github.com/hndrxc/tokenmunchers/releases/download/$tag"
        Write-Host "Installing Munch $version (windows-$arch)..."

        $step = 'preparing a temporary download directory'
        $temp = Join-Path ([IO.Path]::GetTempPath()) ('munch-install-' + [Guid]::NewGuid().ToString('N'))
        $null = [IO.Directory]::CreateDirectory($temp)
        $zipPath = Join-Path $temp $archive
        $sumsPath = Join-Path $temp 'SHA256SUMS'

        $step = "downloading $archive"
        Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/$archive" -OutFile $zipPath
        $step = 'downloading SHA256SUMS'
        Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/SHA256SUMS" -OutFile $sumsPath

        $step = "verifying $archive"
        $hashes = @(foreach ($line in [IO.File]::ReadLines($sumsPath)) {
            $fields = $line.Trim() -split '\s+', 2
            if ($fields.Count -eq 2 -and $fields[1].TrimStart('*') -ceq $archive) {
                $fields[0]
            }
        })
        if ($hashes.Count -ne 1 -or $hashes[0] -notmatch '\A[0-9a-f]{64}\z') {
            throw "Expected exactly one valid checksum for $archive in SHA256SUMS."
        }
        if ((Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash -ine $hashes[0]) {
            throw "Checksum mismatch for $archive. The download was not installed."
        }

        $step = 'staging the verified executable'
        $localAppData = $env:LOCALAPPDATA
        if (-not $localAppData) { throw 'The current user has no LocalAppData directory.' }
        $installDir = Join-Path $localAppData 'Programs\Munch'
        $exe = Join-Path $installDir 'munch.exe'
        if ([IO.Directory]::Exists($exe)) { throw "The target is a directory: $exe" }
        $null = [IO.Directory]::CreateDirectory($installDir)
        # Stage on the same volume so the final move/replace does not copy over the old binary.
        $stage = Join-Path $installDir ('.munch-install-' + [Guid]::NewGuid().ToString('N'))
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $zip = [IO.Compression.ZipFile]::OpenRead($zipPath)
        try {
            if ($zip.Entries.Count -ne 1 -or $zip.Entries[0].FullName -cne 'munch.exe' -or $zip.Entries[0].Length -le 0) {
                throw 'The verified ZIP must contain exactly one nonempty file named munch.exe at its root.'
            }
            [IO.Compression.ZipFileExtensions]::ExtractToFile($zip.Entries[0], $stage)
        } finally {
            $zip.Dispose()
        }

        $step = 'updating the current user PATH'
        # Preserve unexpanded references such as %USERPROFILE% and the existing registry value type.
        $pathKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
        if ($null -eq $pathKey) { $pathKey = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment') }
        $oldUserPath = $pathKey.GetValue('Path', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        $pathKind = if ($null -eq $oldUserPath) { [Microsoft.Win32.RegistryValueKind]::String } else { $pathKey.GetValueKind('Path') }
        if ($pathKind -notin @([Microsoft.Win32.RegistryValueKind]::String, [Microsoft.Win32.RegistryValueKind]::ExpandString)) {
            throw 'The current user PATH is not a registry string.'
        }
        $newUserPath = Get-MunchPath $oldUserPath $installDir
        $newProcessPath = Get-MunchPath $env:Path $installDir
        if (($newUserPath.Length -gt 32766) -or ($newProcessPath.Length -gt 32766)) {
            throw 'Adding Munch would exceed the Windows PATH length limit.'
        }
        if (-not ('MunchBootstrap.Windows' -as [type])) {
            Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
namespace MunchBootstrap {
    public static class Windows {
        private const uint ReplaceExisting = 1;
        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        private static extern bool MoveFileEx(string source, string destination, uint flags);
        public static void ReplaceFile(string source, string destination) {
            // Same-volume rename only; never fall back to copy/delete.
            if (!MoveFileEx(source, destination, ReplaceExisting))
                throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error());
        }

        [DllImport("user32.dll", CharSet = CharSet.Unicode)]
        private static extern IntPtr SendMessageTimeout(IntPtr window, uint message, UIntPtr wParam,
            string lParam, uint flags, uint timeout, out UIntPtr result);
        public static void Broadcast() {
            UIntPtr result;
            SendMessageTimeout(new IntPtr(0xffff), 0x001a, UIntPtr.Zero, "Environment", 2, 1000, out result);
        }
    }
}
'@
        }
        if ($newUserPath -cne $oldUserPath) {
            $pathKey.SetValue('Path', $newUserPath, $pathKind)
            $userPathChanged = $true
        }

        $step = "installing $exe"
        if ([IO.File]::Exists($exe)) {
            try {
                [MunchBootstrap.Windows]::ReplaceFile($stage, $exe)
            } catch {
                throw "Could not replace the existing executable: $($_.Exception.Message) If Munch is running, run 'munch stop', close other running Munch commands, and retry this script."
            }
        } else {
            [IO.File]::Move($stage, $exe)
        }
        $installed = $true
        $env:Path = $newProcessPath
        if ($userPathChanged) { [MunchBootstrap.Windows]::Broadcast() }
        Write-Host "Installed to $exe"
    } catch {
        if ($userPathChanged -and -not $installed) {
            try {
                $currentUserPath = $pathKey.GetValue('Path', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
                if ($currentUserPath -ceq $newUserPath) {
                    if ($null -eq $oldUserPath) { $pathKey.DeleteValue('Path') }
                    else { $pathKey.SetValue('Path', $oldUserPath, $pathKind) }
                }
            } catch {
                Write-Warning "Could not restore the previous user PATH: $($_.Exception.Message)"
            }
        }
        $Host.UI.WriteErrorLine("Munch bootstrap failed while ${step}: $($_.Exception.Message)")
        # Never `exit`: under `irm | iex` it would close the caller's window.
        $global:LASTEXITCODE = 1
        return
    } finally {
        [Net.ServicePointManager]::SecurityProtocol = $oldTls
        if ($null -ne $pathKey) { $pathKey.Dispose() }
        if ($stage) { Remove-Item -LiteralPath $stage -Force -ErrorAction SilentlyContinue }
        if ($temp) { Remove-Item -LiteralPath $temp -Recurse -Force -ErrorAction SilentlyContinue }
    }

    if ($DownloadOnly) {
        Write-Host "Setup skipped. Run 'munch install' when ready."
        return
    }

    Write-Host 'Starting interactive Munch setup...'
    try {
        # Keep this call unpiped and uncaptured so munch talks to the console for its prompts.
        & $exe install
    } catch {
        $Host.UI.WriteErrorLine("Could not run Munch setup: $($_.Exception.Message)")
        $global:LASTEXITCODE = 1
    }
    if ($LASTEXITCODE -ne 0) {
        Write-Host "Munch is installed, but setup did not finish. Run 'munch install' to try again."
    }
} -DownloadOnly:$DownloadOnly
