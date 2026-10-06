; Deej Mixer installer (NSIS 3). Build: makensis DeejMixer.nsi  ->  DeejMixer-Setup.exe
; Silent install:   DeejMixer-Setup.exe /S            (all components, then starts Deej Mixer)
; Silent uninstall: "%ProgramFiles%\Deej Mixer\Uninstall.exe" /S

Unicode true
ManifestDPIAware true
SetCompressor /SOLID lzma
RequestExecutionLevel admin

!define APP      "Deej Mixer"
!ifndef VERSION
  !define VERSION "2.0.0"
!endif
!define EXE      "DeejMixer.exe"
!define UNKEY    "Software\Microsoft\Windows\CurrentVersion\Uninstall\DeejMixer"
!define RUNKEY   "Software\Microsoft\Windows\CurrentVersion\Run"

!include "MUI2.nsh"
!include "x64.nsh"
!include "LogicLib.nsh"
!include "FileFunc.nsh"

Name "${APP}"
OutFile "DeejMixer-Setup.exe"
InstallDir "$PROGRAMFILES64\${APP}"
InstallDirRegKey HKLM "${UNKEY}" "InstallLocation"
BrandingText "${APP} ${VERSION}"

VIProductVersion "${VERSION}.0"
VIAddVersionKey "ProductName" "${APP}"
VIAddVersionKey "FileDescription" "${APP} Setup"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "LegalCopyright" "Deej Mixer"

!define MUI_ICON "app.ico"
!define MUI_UNICON "app.ico"
!define MUI_ABORTWARNING
!define MUI_WELCOMEPAGE_TITLE "Set up ${APP}"
!define MUI_WELCOMEPAGE_TEXT "This installs the ${APP} app and the USB driver for the mixer.$\r$\n$\r$\nThe driver is added to Windows' own driver store, so Windows loads it automatically whenever the mixer is plugged in, with nothing extra running in the background.$\r$\n$\r$\nClick Next to continue."
!define MUI_COMPONENTSPAGE_SMALLDESC
!define MUI_FINISHPAGE_RUN
!define MUI_FINISHPAGE_RUN_TEXT "Start ${APP} now"
!define MUI_FINISHPAGE_RUN_FUNCTION LaunchApp
!define MUI_FINISHPAGE_SHOWREADME "$INSTDIR\START-HERE.txt"
!define MUI_FINISHPAGE_SHOWREADME_TEXT "Show the quick-start guide"
!define MUI_FINISHPAGE_SHOWREADME_NOTCHECKED

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_COMPONENTS
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Function .onInit
  ${IfNot} ${RunningX64}
    MessageBox MB_ICONSTOP "${APP} needs 64-bit Windows 10 or 11." /SD IDOK
    Abort
  ${EndIf}
  SetRegView 64
FunctionEnd

Function un.onInit
  SetRegView 64
FunctionEnd

; Start the app as the signed-in user (not elevated like this installer).
Function LaunchApp
  Exec '"$WINDIR\explorer.exe" "$INSTDIR\${EXE}"'
FunctionEnd

!macro CloseApp
  ; Ask a running copy to quit cleanly (it switches the mixer LEDs off), then make sure.
  IfFileExists "$INSTDIR\${EXE}" 0 +2
    ExecWait '"$INSTDIR\${EXE}" --quit'
  nsExec::Exec 'taskkill /F /IM ${EXE}'
  Pop $0
  Sleep 300
!macroend

Section "${APP} app (required)" SecApp
  SectionIn RO
  !insertmacro CloseApp
  SetOutPath "$INSTDIR"
  File /r "payload\*.*"
  WriteUninstaller "$INSTDIR\Uninstall.exe"

  SetShellVarContext all
  CreateShortcut "$SMPROGRAMS\${APP}.lnk" "$INSTDIR\${EXE}" "" "$INSTDIR\${EXE}" 0 SW_SHOWNORMAL "" "Volume knobs, macro buttons and lighting"

  WriteRegStr   HKLM "${UNKEY}" "DisplayName" "${APP}"
  WriteRegStr   HKLM "${UNKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr   HKLM "${UNKEY}" "Publisher" "${APP}"
  WriteRegStr   HKLM "${UNKEY}" "DisplayIcon" "$INSTDIR\${EXE}"
  WriteRegStr   HKLM "${UNKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr   HKLM "${UNKEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegStr   HKLM "${UNKEY}" "QuietUninstallString" '"$INSTDIR\Uninstall.exe" /S'
  WriteRegDWORD HKLM "${UNKEY}" "NoModify" 1
  WriteRegDWORD HKLM "${UNKEY}" "NoRepair" 1
  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  WriteRegDWORD HKLM "${UNKEY}" "EstimatedSize" $0
SectionEnd

Section "USB driver (CH340 / CH341)" SecDriver
  ; Adds WCH's signed driver package to the Windows driver store and installs it on any matching
  ; device. pnputil is 64-bit only, so file-system redirection is switched off for this 32-bit installer.
  DetailPrint "Adding the CH340/CH341 USB driver to Windows…"
  ${DisableX64FSRedirection}
  nsExec::ExecToLog '"$WINDIR\System32\pnputil.exe" /add-driver "$INSTDIR\drivers\ch341\CH341SER.INF" /install'
  Pop $0
  ${EnableX64FSRedirection}
  ${If} $0 == 0
    DetailPrint "USB driver installed."
  ${ElseIf} $0 == 3010
    DetailPrint "USB driver installed (Windows asks for a restart)."
    SetRebootFlag true
  ${ElseIf} $0 == 259
    DetailPrint "USB driver is already up to date."
  ${Else}
    DetailPrint "pnputil returned $0. The app can retry from Settings > Install CH340 USB driver."
  ${EndIf}
SectionEnd

Section "Desktop shortcut" SecDesktop
  SetShellVarContext all
  CreateShortcut "$DESKTOP\${APP}.lnk" "$INSTDIR\${EXE}" "" "$INSTDIR\${EXE}" 0
SectionEnd

Section "Start with Windows (runs quietly in the tray)" SecStartup
  WriteRegStr HKCU "${RUNKEY}" "DeejMixer" '"$INSTDIR\${EXE}" --background'
SectionEnd

Section "-Finish"
  ${If} ${Silent}
    Call LaunchApp
  ${EndIf}
SectionEnd

!insertmacro MUI_FUNCTION_DESCRIPTION_BEGIN
  !insertmacro MUI_DESCRIPTION_TEXT ${SecApp} "The Deej Mixer app, firmware uploader and Firmware Studio."
  !insertmacro MUI_DESCRIPTION_TEXT ${SecDriver} "Adds the USB-serial driver most Arduino Nano clones need to Windows' driver store. Windows then loads it by itself whenever the mixer is plugged in."
  !insertmacro MUI_DESCRIPTION_TEXT ${SecDesktop} "Puts a Deej Mixer shortcut on the desktop."
  !insertmacro MUI_DESCRIPTION_TEXT ${SecStartup} "Starts Deej Mixer in the system tray when you sign in, so the knobs work right away."
!insertmacro MUI_FUNCTION_DESCRIPTION_END

Section "Uninstall"
  !insertmacro CloseApp
  SetShellVarContext all
  Delete "$SMPROGRAMS\${APP}.lnk"
  Delete "$DESKTOP\${APP}.lnk"
  DeleteRegValue HKCU "${RUNKEY}" "DeejMixer"
  DeleteRegKey HKLM "${UNKEY}"
  RMDir /r "$INSTDIR"
  SetShellVarContext current
  MessageBox MB_YESNO|MB_ICONQUESTION "Also delete your Deej Mixer settings and profiles?" /SD IDNO IDNO keep
    RMDir /r "$APPDATA\DeejMixer"
  keep:
  ; The USB driver stays in Windows (other Arduino boards use it too). Remove it from Device Manager if wanted.
SectionEnd
