param (
    [string]$Version = "latest"
)

$ErrorActionPreference = "Stop"

# Paths
$SourceDir = $PSScriptRoot
$DistDir = Join-Path $SourceDir "dist"
$StagingDir = Join-Path $DistDir "staging"

Write-Host "Starting build process for ChatOps version: $Version"

# Clean dist directory
if (Test-Path $DistDir) {
    Remove-Item -Path $DistDir -Recurse -Force
}
New-Item -ItemType Directory -Path $DistDir | Out-Null
New-Item -ItemType Directory -Path $StagingDir | Out-Null

# Function to build and package
function Build-Component {
    param (
        [string]$ComponentName,
        [string]$Os,
        [string]$Arch,
        [string]$PackageType
    )

    $StagingFolder = Join-Path $StagingDir "$ComponentName-$Os-$Arch"
    New-Item -ItemType Directory -Path $StagingFolder | Out-Null

    $BinaryExt = if ($Os -eq "windows") { ".exe" } else { "" }
    $BinaryPath = Join-Path $StagingFolder "$ComponentName$BinaryExt"
    $CmdPath = Join-Path $SourceDir "cmd\$ComponentName"

    Write-Host "Building $ComponentName for $Os/$Arch..."
    $env:GOOS = $Os
    $env:GOARCH = $Arch
    
    # Run go build
    $ErrorActionPreference = "Continue"
    & go build -ldflags="-s -w" -o $BinaryPath $CmdPath
    if ($LASTEXITCODE -ne 0) {
        $ErrorActionPreference = "Stop"
        Write-Error "Build failed for $ComponentName ($Os/$Arch)"
    }
    $ErrorActionPreference = "Stop"

    # Copy common assets
    if ($ComponentName -eq "gopass-master") {
        Copy-Item -Path (Join-Path $SourceDir "master.yaml.example") -Destination $StagingFolder
        if ($Os -eq "linux") {
            Copy-Item -Path (Join-Path $SourceDir "build\gopass-master.service") -Destination $StagingFolder
            Copy-Item -Path (Join-Path $SourceDir "install-master.sh") -Destination $StagingFolder
        }
    } elseif ($ComponentName -eq "gopass-agent") {
        Copy-Item -Path (Join-Path $SourceDir "agent.yaml.example") -Destination $StagingFolder
        if ($Os -eq "linux") {
            Copy-Item -Path (Join-Path $SourceDir "build\gopass-agent.service") -Destination $StagingFolder
        }
    }

    # Create archive
    $ArchiveName = "$ComponentName-$Version-$Os-$Arch"
    Write-Host "Packaging $ArchiveName..."

    if ($PackageType -eq "zip") {
        $ZipPath = Join-Path $DistDir "$ArchiveName.zip"
        Compress-Archive -Path "$StagingFolder\*" -DestinationPath $ZipPath
    } elseif ($PackageType -eq "tar.gz") {
        $TarballPath = Join-Path $DistDir "$ArchiveName.tar.gz"
        Push-Location $StagingFolder
        # Using built-in tar in Windows 10+
        tar -czf $TarballPath *
        Pop-Location
    }

    Write-Host "Created $ArchiveName"
}

# Build Master
Build-Component -ComponentName "gopass-master" -Os "linux" -Arch "amd64" -PackageType "tar.gz"
Build-Component -ComponentName "gopass-master" -Os "windows" -Arch "amd64" -PackageType "zip"

# Build Agent
Build-Component -ComponentName "gopass-agent" -Os "linux" -Arch "amd64" -PackageType "tar.gz"
Build-Component -ComponentName "gopass-agent" -Os "windows" -Arch "amd64" -PackageType "zip"

# Cleanup staging
Remove-Item -Path $StagingDir -Recurse -Force

Write-Host "Build complete. Assets are in $DistDir"
