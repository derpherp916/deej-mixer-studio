# Publishes this folder as a new GitHub repository and turns on GitHub Pages (docs/ folder).
# Needs Git and the GitHub CLI: winget install Git.Git GitHub.cli ; then: gh auth login
param([string]$Name = "deej-mixer-studio", [switch]$Private)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)
foreach ($tool in "git", "gh") { if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "$tool is not installed. Run: winget install Git.Git GitHub.cli" } }
gh auth status | Out-Null
if ($LASTEXITCODE -ne 0) { gh auth login }
$visibility = if ($Private) { "--private" } else { "--public" }
gh repo create $Name $visibility --source . --remote origin --push --description "Five knobs, six macro buttons and LED lighting for per-app volume on Windows, with a built-in Arduino firmware editor. A fork of deej."
$owner = gh api user --jq .login
# GitHub Pages from the docs/ folder of main
gh api -X POST "repos/$owner/$Name/pages" -f "source[branch]=main" -f "source[path]=/docs" 2>$null | Out-Null
gh repo edit "$owner/$Name" --homepage "https://$owner.github.io/$Name/" --add-topic deej,arduino,volume-mixer,windows,ws2812b,macropad
# First release: GitHub Actions builds the installer and attaches it
git tag v2.0.0
git push origin v2.0.0
Write-Host ""
Write-Host "Repository: https://github.com/$owner/$Name"
Write-Host "Website:    https://$owner.github.io/$Name/  (ready in a minute or two)"
Write-Host "Release:    https://github.com/$owner/$Name/releases  (the Build workflow attaches the installer in ~5 minutes)"
