# Publishes this folder as a new GitHub repository and turns on GitHub Pages (docs/ folder).
# Needs Git and the GitHub CLI: winget install Git.Git GitHub.cli GoLang.Go ; then: gh auth login
# Works whether or not the repository was already created on github.com.
param([string]$Name = "deej-mixer-studio", [switch]$Private)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)
foreach ($tool in "git", "gh") { if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "$tool is not installed. Run: winget install Git.Git GitHub.cli" } }
gh auth status | Out-Null
if ($LASTEXITCODE -ne 0) { gh auth login }
$visibility = if ($Private) { "--private" } else { "--public" }
$owner = gh api user --jq .login
gh repo view "$owner/$Name" 2>$null | Out-Null
if ($LASTEXITCODE -eq 0) {
  # The repository was already created on github.com: just connect this folder to it and push.
  git remote remove origin 2>$null
  git remote add origin "https://github.com/$owner/$Name.git"
  git push -u origin main
} else {
  gh repo create $Name $visibility --source . --remote origin --push --description "Five knobs, six macro buttons and LED lighting for per-app volume on Windows, with a built-in Arduino firmware editor. A fork of deej."
}
if ($LASTEXITCODE -ne 0) { throw "Pushing to GitHub failed." }
# GitHub Pages from the docs/ folder of main
gh api -X POST "repos/$owner/$Name/pages" -f "source[branch]=main" -f "source[path]=/docs" 2>$null | Out-Null
gh repo edit "$owner/$Name" --homepage "https://$owner.github.io/$Name/" --add-topic deej,arduino,volume-mixer,windows,ws2812b,macropad
# Release signing: the app only installs updates signed with this key. The private key goes straight
# into a GitHub secret and is never written to disk.
if (Get-Command go -ErrorAction SilentlyContinue) {
  Push-Location tools/updsign; $keys = go run . keygen; Pop-Location
  $priv = ($keys | Select-String "PRIVATE").ToString().Split(":")[1].Trim()
  $pub  = ($keys | Select-String "PUBLIC").ToString().Split(":")[1].Trim()
  $priv | gh secret set UPDATE_SIGNING_KEY --repo "$owner/$Name"
  gh variable set UPDATE_PUBKEY --body $pub --repo "$owner/$Name"
  Write-Host "Release signing is on."
} else {
  Write-Host "Go is not installed, so release signing was skipped (updates are still checked with SHA-256)."
  Write-Host "To turn it on later: winget install GoLang.Go, then see tools/updsign/main.go."
}
# First release: GitHub Actions builds the installer and attaches it
git tag v2.0.0
git push origin v2.0.0
Write-Host ""
Write-Host "Repository: https://github.com/$owner/$Name"
Write-Host "Website:    https://$owner.github.io/$Name/  (ready in a minute or two)"
Write-Host "Release:    https://github.com/$owner/$Name/releases  (the Build workflow attaches the installer in ~5 minutes)"
