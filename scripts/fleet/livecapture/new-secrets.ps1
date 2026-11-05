[CmdletBinding()]
param(
    [string]$Directory=(Join-Path (Split-Path (Split-Path (Split-Path $PSScriptRoot -Parent) -Parent) -Parent) '.local/livecapture')
)
$ErrorActionPreference='Stop'
$captureFleetRoot=Split-Path (Split-Path (Split-Path $PSScriptRoot -Parent) -Parent) -Parent
& (Join-Path $captureFleetRoot 'scripts/fleet/new-plugin-secrets.ps1') -Directory $Directory | Out-Null
function Capture-Write([string]$Name,[string]$Value){
    [System.IO.File]::WriteAllText((Join-Path $Directory $Name),$Value,[System.Text.UTF8Encoding]::new($false))
    if(-not $IsWindows){& chmod 444 (Join-Path $Directory $Name)}
}
function Capture-Token{return [Convert]::ToHexString([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLowerInvariant()}
$captureAdmin=Capture-Token
$captureMigrate=Capture-Token
$captureRuntime=Capture-Token
Capture-Write 'postgres_password' $captureAdmin
Capture-Write 'migrate_password' $captureMigrate
Capture-Write 'runtime_password' $captureRuntime
Capture-Write 'migrate_url' "postgres://capture_migrator:${captureMigrate}@capture-postgres:5432/livecapture?sslmode=disable"
Capture-Write 'runtime_url' "postgres://capture_app:${captureRuntime}@capture-postgres:5432/livecapture?sslmode=disable"
Capture-Write 'metrics_token' (Capture-Token)
$captureRSA=[System.Security.Cryptography.RSA]::Create(2048)
try{
    $captureRequest=[System.Security.Cryptography.X509Certificates.CertificateRequest]::new('CN=localhost',$captureRSA,[System.Security.Cryptography.HashAlgorithmName]::SHA256,[System.Security.Cryptography.RSASignaturePadding]::Pkcs1)
    $captureSAN=[System.Security.Cryptography.X509Certificates.SubjectAlternativeNameBuilder]::new()
    $captureSAN.AddDnsName('localhost')
    $captureSAN.AddDnsName('livecapture-plugin')
    $captureSAN.AddIpAddress([System.Net.IPAddress]::Loopback)
    $captureRequest.CertificateExtensions.Add($captureSAN.Build())
    $captureRequest.CertificateExtensions.Add([System.Security.Cryptography.X509Certificates.X509BasicConstraintsExtension]::new($true,$false,0,$true))
    $captureCert=$captureRequest.CreateSelfSigned([DateTimeOffset]::UtcNow.AddDays(-1),[DateTimeOffset]::UtcNow.AddDays(90))
    try{Capture-Write 'tls_cert.pem' $captureCert.ExportCertificatePem();Capture-Write 'tls_key.pem' $captureRSA.ExportRSAPrivateKeyPem()}finally{$captureCert.Dispose()}
}finally{$captureRSA.Dispose()}
$captureConfig=@{
    id='livecapture';version='0.4.0';host_endpoint='http://fleet:8081';allow_insecure_host=$true
    host_token_file='/run/secrets/capture_host_token';service_token_file='/run/secrets/capture_service_token'
    http_listen='0.0.0.0:8090';control_listen='0.0.0.0:4340';delivery_listen='0.0.0.0:4341'
    tls_cert_file='/run/secrets/capture_tls_cert';tls_key_file='/run/secrets/capture_tls_key';test_insecure=$false
    capture_label='capture'
    raw_principals=@()
    max_peers=256;metrics_token_file='/run/secrets/capture_metrics_token'
}
Capture-Write 'config.json' ($captureConfig|ConvertTo-Json -Depth 8)
Write-Output 'Created private plugin credentials, database secrets and a 90-day local TLS identity; values are not displayed. Use operator-managed PKI for remote deployments.'
Write-Output 'Collectors authenticate with fleet agent credentials. Issue one with the label capture=<group>[,<group>...] (or *) through POST /api/v1/credentials.'
