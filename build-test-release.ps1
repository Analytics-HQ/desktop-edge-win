# Sample invocations:
# .\build-test-release.ps1 -jsonOnly $true -version 1.1.1
# .\build-test-release.ps1 -jsonOnly $true -version 1.1.1 -stream "dev" -revertGitAfter $true
# .\build-test-release.ps1 -version 2.2.5 -stream "dev" -revertGitAfter $true
# .\build-test-release.ps1 -jsonOnly $true -version 2.2.5 -url https://lnxiskqx49x4.share.zrok.io/local -stream "dev" -revertGitAfter $true
# .\build-test-release.ps1 -version 1.2.3 -url https://lnxiskqx49x4.share.zrok.io/local -stream "dev" -published_at (Get-Date)
# .\build-test-release.ps1 -version 1.2.3 -url https://lnxiskqx49x4.share.zrok.io/local -stream "dev" -published_at "2023-11-02T14:30:00"
param(
    [Parameter(Mandatory = $true)]
    [string]$version,
    [string]$url = "http://localhost:8000/release-streams/local",
    [string]$stream = "local",
    [datetime]$published_at = (Get-Date -Format "yyyy-MM-ddTHH:mm:ssZ"),
    [bool]$jsonOnly = $false,
    [bool]$revertGitAfter = $true
)
echo ""
$env:ZITI_DESKTOP_EDGE_DOWNLOAD_URL="$url"
$env:ZITI_DESKTOP_EDGE_VERSION="$version"
$scriptDirectory = Split-Path -Path $MyInvocation.MyCommand.Path -Parent

$outputPath = "${version}.json"
& .\Installer\output-build-json.ps1 -version $version -url $url -stream $stream -published_at $published_at -outputPath $outputPath

Copy-Item -Force "${version}.json" "$scriptDirectory\release-streams\${stream}.json"
echo "json file written to: $scriptDirectory\release-streams\${stream}.json"

if(! $jsonOnly) {
  $scriptToExecute = Join-Path -Path $scriptDirectory -ChildPath "Installer\build.ps1"
  & $scriptToExecute
  $exitCode = $LASTEXITCODE
  if($exitCode -gt 0) {
    Write-Host "build.ps1 failed!"
    exit $exitCode
  }
  
  mkdir "$scriptDirectory\release-streams\local\${version}" -ErrorAction Ignore > $null
  #echo "$scriptDirectory/Installer/Output/Ziti Desktop Edge Client-${version}.exe" "$scriptDirectory\release-streams\local\${version}\Ziti.Desktop.Edge.Client-${version}.exe"
  Move-Item -Force "$scriptDirectory/Installer/Output/Ziti Desktop Edge Client-${version}.exe" "$scriptDirectory\release-streams\local\${version}\Ziti.Desktop.Edge.Client-${version}.exe"
  Move-Item -Force "$scriptDirectory/Installer/Output/Ziti Desktop Edge Client-${version}.exe.sha256" "$scriptDirectory\release-streams\local\${version}\Ziti.Desktop.Edge.Client-${version}.exe.sha256"
  Write-Host ""
  Write-Host "done."
  Write-Host "installer exists at $scriptDirectory\release-streams\local\${version}\Ziti.Desktop.Edge.Client-${version}.exe"
}

if($revertGitAfter) {
  git checkout DesktopEdge/Properties/AssemblyInfo.cs ZitiUpdateService/Properties/AssemblyInfo.cs Installer/ZitiDesktopEdge.aip
}