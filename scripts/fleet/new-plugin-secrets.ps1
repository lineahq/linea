[CmdletBinding()]
param([string]$Directory=(Join-Path (Split-Path (Split-Path $PSScriptRoot -Parent) -Parent) '.local\plugin-example'))
$ErrorActionPreference='Stop'
if(Test-Path -LiteralPath $Directory){throw 'Plugin secret directory exists; preserve it or choose a new directory'}
if($IsWindows){
    $fleetPluginSID=[System.Security.Principal.WindowsIdentity]::GetCurrent().User
    $fleetPluginACL=[System.Security.AccessControl.DirectorySecurity]::new()
    $fleetPluginACL.SetOwner($fleetPluginSID)
    $fleetPluginACL.SetAccessRuleProtection($true,$false)
    foreach($fleetSID in @($fleetPluginSID,[System.Security.Principal.SecurityIdentifier]::new('S-1-5-18'))){
        $fleetRule=[System.Security.AccessControl.FileSystemAccessRule]::new($fleetSID,[System.Security.AccessControl.FileSystemRights]::FullControl,([System.Security.AccessControl.InheritanceFlags]::ContainerInherit -bor [System.Security.AccessControl.InheritanceFlags]::ObjectInherit),[System.Security.AccessControl.PropagationFlags]::None,[System.Security.AccessControl.AccessControlType]::Allow)
        $fleetPluginACL.AddAccessRule($fleetRule)
    }
    $null=[System.IO.FileSystemAclExtensions]::CreateDirectory($fleetPluginACL,[System.IO.Path]::GetFullPath($Directory))
}else{
    $fleetPrivateMode=[System.IO.UnixFileMode]::UserRead -bor [System.IO.UnixFileMode]::UserWrite -bor [System.IO.UnixFileMode]::UserExecute
    $null=[System.IO.Directory]::CreateDirectory([System.IO.Path]::GetFullPath($Directory),$fleetPrivateMode)
}
foreach($fleetName in @('host_token','service_token')){
    $fleetToken=[Convert]::ToHexString([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLowerInvariant()
    [System.IO.File]::WriteAllText((Join-Path $Directory $fleetName),$fleetToken,[System.Text.UTF8Encoding]::new($false))
    if(-not $IsWindows){& chmod 444 (Join-Path $Directory $fleetName)}
}
Write-Output 'Created private plugin host/service tokens; their values are not displayed.'
