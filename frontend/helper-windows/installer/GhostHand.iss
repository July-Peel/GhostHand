#define AppVersion "0.4.0"
#ifndef Stage
#define Stage "..\dist"
#endif

[Setup]
AppId={{DA1E8410-9319-438F-A531-60C2081347B0}
AppName=GhostHand
AppVersion={#AppVersion}
DefaultDirName={autopf}\GhostHand
DefaultGroupName=GhostHand
PrivilegesRequired=admin
ArchitecturesAllowed=x64os
ArchitecturesInstallIn64BitMode=x64os
MinVersion=10.0.19041
DisableDirPage=yes
UsePreviousAppDir=no
OutputBaseFilename=GhostHand-Setup-x64
Compression=lzma2

[Files]
Source: "{#Stage}\ghosthand-launcher.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Stage}\ghosthand-core.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Stage}\GhostHand.Helper.exe"; DestDir: "{app}"; Flags: ignoreversion

[UninstallRun]
Filename: "{app}\ghosthand-core.exe"; Parameters: "--cleanup-browser"; Flags: runhidden skipifdoesntexist; RunOnceId: "GhostHandBrowserBridge"
Filename: "{app}\ghosthand-launcher.exe"; Parameters: "uninstall"; Flags: runhidden; RunOnceId: "GhostHandSvc"

[Code]
function HasDesktopRuntime(): Boolean;
var
  Versions: TArrayOfString;
  I: Integer;
begin
  Result := False;
  if RegGetSubkeyNames(HKLM64,
    'SOFTWARE\dotnet\Setup\InstalledVersions\x64\sharedfx\Microsoft.WindowsDesktop.App', Versions) then
    for I := 0 to GetArrayLength(Versions) - 1 do
      if Pos('8.', Versions[I]) = 1 then
        Result := True;
  if Result then
    Exit;
  if RegGetValueNames(HKLM64,
    'SOFTWARE\dotnet\Setup\InstalledVersions\x64\sharedfx\Microsoft.WindowsDesktop.App', Versions) then
    for I := 0 to GetArrayLength(Versions) - 1 do
      if Pos('8.', Versions[I]) = 1 then
        Result := True;
end;

function InitializeSetup(): Boolean;
begin
  Result := HasDesktopRuntime();
  if not Result then
    MsgBox('Install the .NET 8 Desktop Runtime (x64) before installing GhostHand.', mbError, MB_OK);
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  ExitCode: Integer;
begin
  if CurStep = ssPostInstall then
    if not Exec(ExpandConstant('{app}\ghosthand-launcher.exe'), 'install', ExpandConstant('{app}'),
      SW_HIDE, ewWaitUntilTerminated, ExitCode) or (ExitCode <> 0) then
      RaiseException('Could not install or start GhostHandSvc.');
end;
