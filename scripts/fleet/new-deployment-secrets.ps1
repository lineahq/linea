[CmdletBinding()]
param([string]$Directory=(Join-Path (Split-Path (Split-Path $PSScriptRoot -Parent) -Parent) '.local\deployment'))
$ErrorActionPreference='Stop'
if(Test-Path -LiteralPath $Directory){throw 'Secret directory already exists; preserve existing database credentials'}
if($IsWindows){
    # Supply the private DACL at creation. Changing an inherited DACL afterward
    # can be denied on managed workspace drives even when creation is allowed.
    $fleetOwnerSID=[System.Security.Principal.WindowsIdentity]::GetCurrent().User
    $fleetSecurity=[System.Security.AccessControl.DirectorySecurity]::new()
    $fleetSecurity.SetOwner($fleetOwnerSID)
    $fleetSecurity.SetAccessRuleProtection($true,$false)
    foreach($fleetSID in @($fleetOwnerSID,[System.Security.Principal.SecurityIdentifier]::new('S-1-5-18'))){
        $fleetRule=[System.Security.AccessControl.FileSystemAccessRule]::new($fleetSID,[System.Security.AccessControl.FileSystemRights]::FullControl,([System.Security.AccessControl.InheritanceFlags]::ContainerInherit -bor [System.Security.AccessControl.InheritanceFlags]::ObjectInherit),[System.Security.AccessControl.PropagationFlags]::None,[System.Security.AccessControl.AccessControlType]::Allow)
        $fleetSecurity.AddAccessRule($fleetRule)
    }
    $null=[System.IO.FileSystemAclExtensions]::CreateDirectory($fleetSecurity,[System.IO.Path]::GetFullPath($Directory))
}else{
    $fleetPrivateMode=[System.IO.UnixFileMode]::UserRead -bor [System.IO.UnixFileMode]::UserWrite -bor [System.IO.UnixFileMode]::UserExecute
    $null=[System.IO.Directory]::CreateDirectory([System.IO.Path]::GetFullPath($Directory),$fleetPrivateMode)
}
$fleetAdminPassword=[Convert]::ToHexString([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
$fleetMigratePassword=[Convert]::ToHexString([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
$fleetRuntimePassword=[Convert]::ToHexString([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
$fleetReplicationPassword=[Convert]::ToHexString([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
# compose.ha.yaml: try both servers and use whichever accepts writes.
$fleetHAHosts='postgres:5432,postgres-standby:5432'
$fleetSecretValues=@{
    postgres_password=$fleetAdminPassword
    migrate_password=$fleetMigratePassword
    runtime_password=$fleetRuntimePassword
    migrate_url="postgres://fleet_migrator:${fleetMigratePassword}@postgres:5432/fleet?sslmode=disable"
    runtime_url="postgres://fleet_app:${fleetRuntimePassword}@postgres:5432/fleet?sslmode=disable"
    replication_password=$fleetReplicationPassword
    migrate_ha_url="postgres://fleet_migrator:${fleetMigratePassword}@${fleetHAHosts}/fleet?sslmode=disable&target_session_attrs=read-write"
    runtime_ha_url="postgres://fleet_app:${fleetRuntimePassword}@${fleetHAHosts}/fleet?sslmode=disable&target_session_attrs=read-write"
}
foreach($fleetEntry in $fleetSecretValues.GetEnumerator()){
    [System.IO.File]::WriteAllText((Join-Path $Directory $fleetEntry.Key),$fleetEntry.Value,[System.Text.UTF8Encoding]::new($false))
}
if(-not $IsWindows){
    # File-based Compose secrets retain host modes. The private parent directory
    # protects host access while these mounts remain readable by container UID 65532.
    foreach($fleetEntry in $fleetSecretValues.GetEnumerator()){& chmod 444 (Join-Path $Directory $fleetEntry.Key)}
}
Write-Output 'Created deployment secrets. Restrict this directory to the deployment operator and preserve it with the database volume.'
