#Requires -Version 5.1
<#
.SYNOPSIS
    Installs kute, a TUI Kubernetes console.

.DESCRIPTION
    The Windows counterpart to https://kute.dev/install.sh. Downloads the
    release archive for this machine's architecture, verifies its cosign
    signature when cosign is installed, verifies it against the release's
    checksums.txt, and installs kute.exe.

        irm https://kute.dev/install.ps1 | iex

    Environment overrides, matching install.sh:
        $env:KUTE_VERSION       release to install (default: latest)
        $env:KUTE_INSTALL_DIR   install location
                                (default: %LOCALAPPDATA%\Programs\kute)
#>

$ErrorActionPreference = 'Stop'
# Windows PowerShell 5.1 renders a progress bar for every Invoke-WebRequest
# and it costs roughly an order of magnitude in download time.
$ProgressPreference = 'SilentlyContinue'

# The checksum verified below proves the archive matches a manifest that came
# down the same wire as the archive; only the signature says the release is
# ours. cosign is not a dependency of this script, so a machine without it
# gets a note and the checksum — a stronger check that nobody can run is not
# stronger. Same for a release predating signing (before v0.4.0-beta.1):
# don't fail an install that used to work — but only those.
function Confirm-KuteSignature {
    param(
        [string]$BaseUrl,
        [string]$Archive,
        [string]$ArchivePath,
        [string]$Version,
        [string]$VerifyDocs
    )

    if (-not (Get-Command cosign -ErrorAction SilentlyContinue)) {
        Write-Host 'note: cosign not found; verified checksum only.'
        Write-Host "      $VerifyDocs explains how to check the signature."
        return
    }

    $bundlePath = "$ArchivePath.sigstore.json"
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/$Archive.sigstore.json" -OutFile $bundlePath
    } catch {
        # Only the releases that predate signing may lack a bundle. From
        # v0.4.0-beta.1 on, a missing one is exactly what someone who
        # swapped the archive and its checksum manifest would also arrange,
        # so with cosign present it fails rather than quietly downgrading.
        if ($Version -match '^v0\.[123]\.') {
            Write-Host "note: $Version predates release signing; verified checksum only."
            return
        }
        throw "kute install: could not download the signature for $Archive (every release from v0.4.0-beta.1 is signed) — do not run it; see $VerifyDocs"
    }

    # The keyless signing identity is the release workflow itself. Matched by
    # regexp rather than by exact tag so this script never needs to know which
    # version it just fetched; the anchor is what keeps it to that one
    # workflow in that one repo.
    $identity = '^https://github\.com/kute-dev/kute/\.github/workflows/release\.yml@refs/tags/'
    $issuer = 'https://token.actions.githubusercontent.com'

    # cosign writes "Verified OK" to stderr on *success*, and PowerShell 5.1
    # turns a native command's stderr into error records — which under the
    # script's ErrorActionPreference = 'Stop' throws NativeCommandError on a
    # verification that passed. Drop to 'Continue' for the call itself and
    # judge the result by $LASTEXITCODE, which is the only honest signal here.
    #
    # cosign 3 reads the bundle format by default and has no
    # --new-bundle-format flag; cosign 2 needs it and rejects a bundle
    # without it. Rather than parse `cosign version`, try the modern form and
    # fall back — a bundle that is genuinely bad fails both ways, so the
    # retry can only rescue an old cosign, never hide a bad signature.
    #
    # A non-zero $LASTEXITCODE is not always a verdict. cosign bootstraps
    # Sigstore's TUF trust root before it looks at a byte of the bundle, and
    # when that bootstrap fails (the tuf-repo-cdn.sigstore.dev 403/404
    # outage, DNS, a proxy) it exits without judging the signature — the
    # cosign-not-found case with cosign present. Only a failure whose output
    # is not a trust-bootstrap failure means "do not run it".
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $unverifiable = $false
    try {
        foreach ($extra in @(@(), @('--new-bundle-format'))) {
            $output = (& cosign verify-blob `
                --certificate-identity-regexp $identity `
                --certificate-oidc-issuer $issuer `
                --bundle $bundlePath `
                @extra `
                $ArchivePath 2>&1 | Out-String)
            if ($LASTEXITCODE -eq 0) { break }
            if ($output -match 'setting trusted material|getting trusted root|failed to create TUF client|tuf refresh failed') {
                $unverifiable = $true
            }
        }
    } finally {
        $ErrorActionPreference = $prevEap
    }
    if ($LASTEXITCODE -eq 0) {
        Write-Host 'Verified signature (cosign, keyless).'
        return
    }
    if ($unverifiable) {
        Write-Host 'note: cosign could not verify — Sigstore trust bootstrap failed; verified checksum only.'
        Write-Host "      $VerifyDocs explains how to check the signature."
        return
    }
    throw "kute install: signature verification failed for $Archive — do not run it; see $VerifyDocs"
}

function Install-Kute {
    $repo = 'kute-dev/kute'
    $bin = 'kute.exe'
    $verifyDocs = 'https://kute.dev/verify.html'

    # 5.1 still negotiates TLS 1.0 by default, which github.com refuses.
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    # PROCESSOR_ARCHITECTURE reports the *process* architecture, so a 32-bit
    # PowerShell on 64-bit Windows says x86; PROCESSOR_ARCHITEW6432 is the
    # machine's real one and is only set in that WOW64 case. Same class of
    # lie as uname -m under Rosetta, which install.sh corrects the same way.
    $hostArch = $env:PROCESSOR_ARCHITEW6432
    if (-not $hostArch) { $hostArch = $env:PROCESSOR_ARCHITECTURE }
    switch ($hostArch) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { throw "kute install: unsupported architecture: $hostArch" }
    }

    $version = $env:KUTE_VERSION
    if (-not $version) { $version = 'latest' }
    if ($version -eq 'latest') {
        Write-Host 'Resolving latest kute release...'
        # Follow the /releases/latest redirect rather than calling the REST
        # API: the API is rate-limited to 60 requests/hour per unauthenticated
        # IP, so behind a corporate NAT or on a shared CI runner this step
        # fails intermittently. install.sh resolves it the same way.
        $latestUrl = "https://github.com/$repo/releases/latest"
        $resp = Invoke-WebRequest -UseBasicParsing -Method Head -Uri $latestUrl
        # BaseResponse is an HttpWebResponse on Windows PowerShell 5.1 and an
        # HttpResponseMessage on PowerShell 6+; the final URL after the
        # redirect lives under a different property on each.
        if ($resp.BaseResponse.PSObject.Properties['ResponseUri']) {
            $finalUri = $resp.BaseResponse.ResponseUri
        } else {
            $finalUri = $resp.BaseResponse.RequestMessage.RequestUri
        }
        $version = ($finalUri.AbsolutePath -split '/')[-1]
    }
    if ($version -match '^\d') { $version = "v$version" }
    if ($version -notmatch '^v\d') {
        throw "kute install: could not resolve release version (got: '$version')"
    }

    $archiveVersion = $version.TrimStart('v')
    $archive = "kute_${archiveVersion}_windows_${arch}.zip"
    $baseUrl = "https://github.com/$repo/releases/download/$version"

    $installDir = $env:KUTE_INSTALL_DIR
    if (-not $installDir) {
        # Under LOCALAPPDATA rather than Program Files: writable without
        # elevation, and there is no sudo here to fall back on the way
        # install.sh does.
        $installDir = Join-Path $env:LOCALAPPDATA 'Programs\kute'
    }

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("kute-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp -Force | Out-Null
    try {
        Write-Host "Installing kute $version for windows/$arch..."

        $archivePath = Join-Path $tmp $archive
        $sumsPath = Join-Path $tmp 'checksums.txt'
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/$archive" -OutFile $archivePath
            Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/checksums.txt" -OutFile $sumsPath
        } catch {
            throw "kute install: download failed from ${baseUrl}: $($_.Exception.Message)"
        }

        Confirm-KuteSignature -BaseUrl $baseUrl -Archive $archive `
            -ArchivePath $archivePath -Version $version -VerifyDocs $verifyDocs

        # checksums.txt is "<sha256>  <filename>"; the binary-mode marker is
        # a "*" prefix on the name, which sha256sum writes and goreleaser
        # does not — tolerate both.
        $pattern = "\s\*?$([regex]::Escape($archive))$"
        $line = Get-Content -LiteralPath $sumsPath |
            Where-Object { $_ -match $pattern } |
            Select-Object -First 1
        if (-not $line) {
            throw "kute install: checksums.txt has no entry for $archive"
        }
        $want = ($line -split '\s+')[0]
        $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash
        # -ne on strings is case-insensitive, which is what we want:
        # Get-FileHash returns upper case, checksums.txt is lower.
        if ($got -ne $want) {
            throw "kute install: checksum verification failed for $archive"
        }

        Expand-Archive -LiteralPath $archivePath -DestinationPath $tmp -Force
        $staged = Join-Path $tmp $bin
        if (-not (Test-Path -LiteralPath $staged)) {
            throw "kute install: archive did not contain $bin"
        }

        New-Item -ItemType Directory -Path $installDir -Force | Out-Null
        $dest = Join-Path $installDir $bin
        $old = "$dest.old"
        # Windows locks a running executable against writes, but not against
        # a rename — so move the old one aside first. This is the same
        # upgrade-while-running case install.sh handles by replacing the
        # inode to dodge ETXTBSY.
        if (Test-Path -LiteralPath $dest) {
            Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue
            Move-Item -LiteralPath $dest -Destination $old -Force
        }
        Copy-Item -LiteralPath $staged -Destination $dest -Force
        # Still locked if the old kute is running; it goes on the next run.
        Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue

        Write-Host "kute installed to $dest"

        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $entries = @($userPath -split ';' | Where-Object { $_ })
        if ($entries -notcontains $installDir) {
            $updated = (@($entries) + $installDir) -join ';'
            [Environment]::SetEnvironmentVariable('Path', $updated, 'User')
            Write-Host "Added $installDir to your PATH - restart your terminal to pick it up."
        }
        # Make kute resolvable in *this* session too, so the check below and
        # anything the user types next both work without a restart.
        $env:Path = "$env:Path;$installDir"

        & $dest --version
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Install-Kute
