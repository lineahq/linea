[CmdletBinding()]
param(
    [switch]$FunctionalCheck,
    [switch]$DedicatedEnvironment,
    [switch]$WithCaptureFault,
    [string]$CaptureCollectorImage='linea-fleet-livecapture-compat:collector',
    [ValidateRange(1,1000)][int]$Collectors=100,
    [ValidateRange(35,1800)][int]$DurationSeconds=120,
    [ValidatePattern('^[a-z0-9][a-z0-9.-]{0,40}$')][string]$ImageTag='local',
    [ValidateRange(1,254)][int]$NetworkOctet=245,
    [switch]$Build
)
$ErrorActionPreference='Stop'
# This guard must precede Docker, credentials, filesystem writes and provisioning.
if(-not $FunctionalCheck -and -not $DedicatedEnvironment){throw 'Capacity runs require -DedicatedEnvironment on an agreed dedicated host. Use -FunctionalCheck for at most two Collectors.'}
if($FunctionalCheck -and $DedicatedEnvironment){throw 'Select one execution mode'}
if($FunctionalCheck){$Collectors=$(if($WithCaptureFault){1}else{2});$DurationSeconds=35}
$capacityRoot=Split-Path $PSScriptRoot -Parent
$capacityID=[Guid]::NewGuid().ToString('N')
$capacityProject="fleet-capacity-$capacityID"
$capacityDir=Join-Path $capacityRoot ".local/$capacityProject"
$capacityEnv=Join-Path $capacityDir 'compose.env'
$capacityOverride=Join-Path $capacityDir 'collectors.json'
$capacityCompose=@('--parallel','4','--project-name',$capacityProject,'--env-file',$capacityEnv,'--file',(Join-Path $capacityRoot 'compose.yaml'))
if($WithCaptureFault){$capacityCompose+=@('--file',(Join-Path $capacityRoot 'compose.livecapture.yaml'))}
$capacityCompose+=@('--file',$capacityOverride)
$capacityCollectorImage='otel/opentelemetry-collector-contrib:0.162.0@sha256:39923a8e431bd1f57be82411999d389fcfe40857492e4365456d97a4c1f74be6'
$capacityOldToken=$env:LINEA_FLEET_AGENT_TOKEN
$capacityStarted=$false
$capacityResult=[ordered]@{mode=$(if($FunctionalCheck){'functional_two_collectors'}else{'dedicated_capacity'});status='failed';collectors=$Collectors;steady_seconds=$DurationSeconds;scope='Official reporting-only Collectors; livecapture capacity is a separate profile';source=(& git -C $capacityRoot rev-parse HEAD);harness_sha256=(Get-FileHash $PSCommandPath -Algorithm SHA256).Hash;images=@{};phases=@{};resource_limits=@{fleet_cpus=2;fleet_memory_mib=512;postgres_cpus=1;postgres_memory_mib=512;collector_cpus=0.25;collector_memory_mib=128};http_samples=@()}
$capacityResult.capture_fault=[bool]$WithCaptureFault
$capacityResult.collector_processes_total=$Collectors+$(if($WithCaptureFault){1}else{0})
if($WithCaptureFault){$capacityResult.resource_limits.capture_plugin_cpus=2;$capacityResult.resource_limits.capture_plugin_memory_mib=512;$capacityResult.resource_limits.capture_postgres_cpus=1;$capacityResult.resource_limits.capture_postgres_memory_mib=512;$capacityResult.resource_limits.capture_collector_cpus=1;$capacityResult.resource_limits.capture_collector_memory_mib=256}
$capacitySamples=[System.Collections.Generic.List[double]]::new()
function Capacity-Compose {
    & docker compose @capacityCompose @args
    if($LASTEXITCODE -ne 0){throw 'Capacity fixture Compose operation failed; request/configuration omitted'}
}
function Capacity-Request([string]$Method,[string]$Path,[string]$Token,[string]$Body='',[int]$Expected=200){
    $capacityOptions=@{Method=$Method;Uri="$capacityBase$Path";Headers=@{Authorization="Bearer $Token"};TimeoutSec=8;SkipHttpErrorCheck=$true}
    if($Body){$capacityOptions.Body=$Body;$capacityOptions.ContentType='application/json'}
    $capacityClock=[System.Diagnostics.Stopwatch]::StartNew()
    $response=Invoke-WebRequest @capacityOptions
    $capacitySamples.Add($capacityClock.Elapsed.TotalMilliseconds)
    if(($Expected -ne 0 -and $response.StatusCode -ne $Expected) -or ($Expected -eq 0 -and $response.StatusCode -notin @(200,204))){throw "Capacity API request returned HTTP $($response.StatusCode); body omitted"}
    if($response.StatusCode -eq 204){return}
    if($response.Headers['Content-Type'] -like 'application/json*'){return ($response.Content|ConvertFrom-Json)}
    return $response.Content
}
function Capacity-Wait([scriptblock]$Check,[string]$Failure,[int]$Seconds=120){
    $deadline=[DateTime]::UtcNow.AddSeconds($Seconds)
    do{if(& $Check){return};Start-Sleep -Milliseconds 500}while([DateTime]::UtcNow -lt $deadline)
    throw $Failure
}
function Capacity-Metric([string]$Text,[string]$Name){
    $match=[regex]::Match($Text,'(?m)^'+[regex]::Escape($Name)+' ([0-9.eE+-]+)\r?$')
    if(-not $match.Success){throw 'Required operational metric absent'}
    return [double]::Parse($match.Groups[1].Value,[Globalization.CultureInfo]::InvariantCulture)
}
function Capacity-Snapshot([string]$Phase){
    $text=Capacity-Request GET /metrics $capacityAdmin
    [System.IO.File]::WriteAllText((Join-Path $capacityDir "$Phase.prom"),$text)
    $stats=& docker stats --no-stream --format '{{json .}}' $capacityCore $capacityPostgres
    if($LASTEXITCODE -ne 0){throw 'Fixture resource snapshot failed'}
    [System.IO.File]::WriteAllLines((Join-Path $capacityDir "$Phase.resources.jsonl"),[string[]]$stats)
    return $text
}
function Capacity-Inventory {
    $items=[System.Collections.Generic.List[object]]::new();$after=''
    do{
        $path='/api/v1/agents?limit=500';if($after){$path+="&after=$after"}
        $page=Capacity-Request GET $path $capacityAdmin
        foreach($item in $page.items){$items.Add($item)}
        $after=$page.next_after
    }while($after)
    return $items.ToArray()
}
function Capacity-UUID7 {
    $bytes=[System.Security.Cryptography.RandomNumberGenerator]::GetBytes(16)
    $millis=[DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
    for($index=5;$index -ge 0;$index--){$bytes[$index]=[byte]($millis -band 255);$millis=$millis -shr 8}
    $bytes[6]=[byte](112 -bor ($bytes[6] -band 15));$bytes[8]=[byte](128 -bor ($bytes[8] -band 63))
    $hex=[Convert]::ToHexString($bytes).ToLowerInvariant()
    return $hex.Substring(0,8)+'-'+$hex.Substring(8,4)+'-'+$hex.Substring(12,4)+'-'+$hex.Substring(16,4)+'-'+$hex.Substring(20,12)
}
function Capacity-Capture([string]$Method,[string]$Path,[string]$Body='',[int]$Expected=200){Capacity-Request $Method "/api/v1/plugins/livecapture/api/$Path" $capacityAdmin $Body $Expected}
try{
    $capacityMachine=(& docker info --format '{{.OSType}}|{{.Architecture}}|{{.NCPU}}|{{.MemTotal}}|{{.ServerVersion}}') -split '\|'
    if($LASTEXITCODE -ne 0 -or $capacityMachine[0] -ne 'linux'){throw 'Linux Docker daemon required'}
    $capacityRequired=([long]$Collectors*128+$(if($WithCaptureFault){3072}else{2048}))*1MB
    if(-not $FunctionalCheck -and [long]$capacityMachine[3] -lt $capacityRequired){throw 'Dedicated Docker memory budget is below the configured Collector limits plus 2 GiB reserve'}
    $capacityResult.machine=@{os=$capacityMachine[0];architecture=$capacityMachine[1];cpus=[int]$capacityMachine[2];memory_bytes=[long]$capacityMachine[3];docker=$capacityMachine[4]}
    & (Join-Path $capacityRoot 'scripts/fleet/new-deployment-secrets.ps1') -Directory $capacityDir|Out-Null
    if($WithCaptureFault){
        & (Join-Path $capacityRoot 'scripts/fleet/livecapture/new-secrets.ps1') -Directory (Join-Path $capacityDir 'capture')|Out-Null
    }
    $capacityPath=$capacityDir.Replace('\','/')
    [System.IO.File]::WriteAllText($capacityEnv,"FLEET_SECRET_DIR='$capacityPath'`nFLEET_SERVER_ID=$capacityProject`nFLEET_API_PORT=0`nFLEET_AGENT_PORT=0`nFLEET_VERSION=$ImageTag`n")
    if($WithCaptureFault){[System.IO.File]::AppendAllText($capacityEnv,"FLEET_LIVECAPTURE_SECRET_DIR='$capacityPath/capture'`nCAPTURE_VERSION=$ImageTag`nCAPTURE_CONTROL_PORT=0`nCAPTURE_DELIVERY_PORT=0`n")}
    $env:LINEA_FLEET_AGENT_TOKEN='not-issued-yet'
    $capacityServices=[ordered]@{fleet=@{command=@('--server-id',$capacityProject,'--management-listen','0.0.0.0:8080','--agent-listen','0.0.0.0:4320','--allow-insecure-remote','--max-agents',[string]($Collectors*2),'--max-connections',[string]($Collectors+16))}}
    if($WithCaptureFault){
        $capacityServices.fleet.command+=@('--plugins-config','/etc/linea-fleet/plugins.json','--plugin-listen','0.0.0.0:8081')
        $capacityServices['capture-probe']=@{image=$CaptureCollectorImage;command=@('--config=/fixture.yaml');environment=@{GOMAXPROCS='1';LINEA_FLEET_AGENT_TOKEN='${LINEA_FLEET_AGENT_TOKEN}';POD_NAME='capacity-probe'};volumes=@("$($capacityRoot.Replace('\','/'))/plugins/livecapture/examples/collector.yaml:/fixture.yaml:ro","$capacityPath/capture/tls_cert.pem:/certs/ca.pem:ro");networks=@('capture-ingress');cpus=1;mem_limit='256m';pids_limit=128;read_only=$true;cap_drop=@('ALL');security_opt=@('no-new-privileges:true');restart='no'}
    }
    $capacityNames=[System.Collections.Generic.List[string]]::new()
    $capacityTemplate=[System.IO.File]::ReadAllText((Join-Path $capacityRoot 'examples/fleet-collector.yaml'))
    for($number=1;$number -le $Collectors;$number++){
        $name='collector-'+$number.ToString('D6');$capacityNames.Add($name)
        $uid=Capacity-UUID7
        $config=$capacityTemplate.Replace("  opamp:","  opamp:`n    instance_uid: $uid")
        $path=Join-Path $capacityDir "$name.yaml"
        [System.IO.File]::WriteAllText($path,$config)
        $capacityServices[$name]=@{image=$capacityCollectorImage;command=@('--config=/etc/collector.yaml');environment=@{GOMAXPROCS='1';GOMEMLIMIT='96MiB';LINEA_FLEET_AGENT_TOKEN='${LINEA_FLEET_AGENT_TOKEN}';LINEA_FLEET_OPAMP_ENDPOINT='ws://fleet:4320/v1/opamp'};volumes=@("$($path.Replace('\','/')):/etc/collector.yaml:ro");networks=@('ingress');cpus=0.25;mem_limit='128m';pids_limit=64;read_only=$true;cap_drop=@('ALL');security_opt=@('no-new-privileges:true');restart='no';logging=@{driver='local';options=@{'max-size'='1m';'max-file'='2'}}}
    }
    $capacityNetworks=@{database=@{ipam=@{config=@(@{subnet="10.$NetworkOctet.0.0/20"})}};ingress=@{ipam=@{config=@(@{subnet="10.$NetworkOctet.16.0/20"})}}}
    if($WithCaptureFault){
        $capacitySubnet=32
        foreach($network in @('plugins','capture-database','capture-ingress')){$capacityNetworks[$network]=@{ipam=@{config=@(@{subnet="10.$NetworkOctet.$capacitySubnet.0/24"})}};$capacitySubnet++}
    }
    [System.IO.File]::WriteAllText($capacityOverride,(@{services=$capacityServices;networks=$capacityNetworks}|ConvertTo-Json -Depth 20))
    if($Build){
        Capacity-Compose build fleet
        if($WithCaptureFault){
            Capacity-Compose build livecapture-plugin
            & docker build --file (Join-Path $capacityRoot 'deploy/docker/linea-fleet-livecapture.Dockerfile') --target fixture --tag "linea-fleet-livecapture-generator:$ImageTag" $capacityRoot
            if($LASTEXITCODE -ne 0){throw 'Synthetic input image build failed'}
        }
    }
    $capacityResult.images.collector=$capacityCollectorImage
    $capacityResult.images.fleet=& docker image inspect "ghcr.io/lineahq/linea-fleet:$ImageTag" --format '{{.Id}}'
    if($LASTEXITCODE -ne 0){throw 'Build the selected fleet image before running the profile'}
    if($WithCaptureFault){foreach($entry in @(@('capture_collector',$CaptureCollectorImage),@('capture_plugin',"ghcr.io/lineahq/linea-fleet-livecapture:$ImageTag"),@('synthetic_input',"linea-fleet-livecapture-generator:$ImageTag"))){$capacityResult.images[$entry[0]]=& docker image inspect $entry[1] --format '{{.Id}}';if($LASTEXITCODE -ne 0){throw 'Optional capture fault profile image missing; see capacity README'}}}
    $capacityStarted=$true
    Capacity-Compose up --detach --wait postgres
    Capacity-Compose run --rm -T migrate|Out-Null
    if($WithCaptureFault){Capacity-Compose up --detach --wait capture-postgres;Capacity-Compose run --rm -T capture-migrate|Out-Null}
    $capacityBootstrap=Capacity-Compose run --rm -T bootstrap|ConvertFrom-Json
    $capacityAdmin=$capacityBootstrap.token
    Capacity-Compose up --detach --wait fleet
    if($WithCaptureFault){Capacity-Compose up --detach --wait livecapture-plugin}
    $capacityCore=Capacity-Compose ps --quiet fleet
    $capacityPostgres=Capacity-Compose ps --quiet postgres
    $capacityResult.images.postgres=& docker inspect $capacityPostgres --format '{{.Image}}'
    $capacityPort=(& docker port $capacityCore 8080/tcp) -replace '^.*:',''
    $capacityBase="http://127.0.0.1:$capacityPort"
    # One fleet agent credential for all Collectors; the capture label admits the probe to livecapture.
    $capacityCredential=@{id=[Guid]::NewGuid().ToString('N');name='capacity-deployment';role='agent';expires_at=[DateTime]::UtcNow.AddHours(1).ToString('o')}
    if($WithCaptureFault){$capacityCredential.labels=@{capture='s4'}}
    $issued=Capacity-Request POST /api/v1/credentials $capacityAdmin ($capacityCredential|ConvertTo-Json) 201
    $env:LINEA_FLEET_AGENT_TOKEN=$issued.token
    if($WithCaptureFault){Capacity-Compose up --detach --no-deps capture-probe}
    $capacityPhase=[System.Diagnostics.Stopwatch]::StartNew()
    Capacity-Compose up --detach --no-deps @capacityNames
    Capacity-Wait {return (Capacity-Request GET /api/v1/summary $capacityAdmin).connected -eq $Collectors} 'Not all real Collectors registered' ([Math]::Max(120,$Collectors))
    $capacityResult.phases.registration_seconds=$capacityPhase.Elapsed.TotalSeconds
    $capacityBefore=@(Capacity-Inventory)
    $capacityGenerations=@{};foreach($item in $capacityBefore){$capacityGenerations[$item.id]=$item.generation}
    if($capacityBefore.Count -ne $Collectors){throw 'Inventory pagination lost or duplicated identities'}
    $before=Capacity-Snapshot 'registered'
    $capacityPhase.Restart()
    while($capacityPhase.Elapsed.TotalSeconds -lt $DurationSeconds){
        $summary=Capacity-Request GET /api/v1/summary $capacityAdmin
        if($summary.connected -ne $Collectors){throw 'Collector presence changed during steady reporting'}
        $null=Capacity-Request GET /readyz $capacityAdmin
        Start-Sleep -Milliseconds 1000
    }
    $steady=Capacity-Snapshot 'steady'
    $capacityResult.phases.steady_elapsed_seconds=$capacityPhase.Elapsed.TotalSeconds
    $capacityResult.phases.steady_reports=(Capacity-Metric $steady 'linea_fleet_report_duration_seconds_count')-(Capacity-Metric $before 'linea_fleet_report_duration_seconds_count')
    $capacityResult.phases.steady_report_errors=(Capacity-Metric $steady 'linea_fleet_report_errors_total')-(Capacity-Metric $before 'linea_fleet_report_errors_total')
    if($capacityResult.phases.steady_reports -lt $Collectors -or $capacityResult.phases.steady_report_errors -ne 0){throw 'Steady report progress/error gate failed'}
    if($WithCaptureFault){
        Capacity-Wait {try{return (Capacity-Capture GET v1/peers).items.Count -eq 1}catch{return $false}} 'Capture fault peer did not register'
        $capturePeer=(Capacity-Capture GET v1/peers).items[0]
        $captureChannel=Capacity-Capture POST v1/channels (@{expires_at=[DateTime]::UtcNow.AddSeconds(60).ToString('o')}|ConvertTo-Json) 201
        $captureBody=@{authority_id=$capturePeer.authority_id;plugin_instance=$capturePeer.plugin_instance;id=[Guid]::NewGuid().ToString('N');channel_id=$captureChannel.id;targets=@(@{peer_id=$capturePeer.id;generation=$capturePeer.generation});capture_points=@(@{pipeline='traces';point='egress'});signals=@('traces');filter=@{};expires_at=[DateTime]::UtcNow.AddSeconds(20).ToString('o')}|ConvertTo-Json -Depth 10
        $captureOp=Capacity-Capture POST v1/captures $captureBody 202
        Capacity-Wait {return (Capacity-Capture GET "v1/captures/$($captureOp.id)").state -eq 'active'} 'Capture fault activation not acknowledged'
        $capacityPhase.Restart()
        & docker run --rm --network "${capacityProject}_capture-ingress" --cpus 1 --memory 128m --pids-limit 64 --read-only --cap-drop ALL "linea-fleet-livecapture-generator:$ImageTag" --endpoint capture-probe:4317
        if($LASTEXITCODE -ne 0){throw 'Capture fault synthetic input failed'}
        $capacityFaultReads=0;$capacityPendingBytes=0;$capacityPendingReads=0
        do{
            $null=Capacity-Capture GET "v1/channels/$($captureChannel.id)/next" '' 0
            $observed=Capacity-Capture GET v1/metrics
            $capacityPendingBytes=[Math]::Max($capacityPendingBytes,$observed.gauges.pending_bytes)
            if((Capacity-Request GET /api/v1/summary $capacityAdmin).connected -ne $Collectors){throw 'Consumer fault disrupted fleet presence'}
            if((Capacity-Request GET /readyz $capacityAdmin).status -ne 'ready'){throw 'Consumer fault disrupted core readiness'}
            if($observed.gauges.pending_bytes -gt 0 -and $observed.gauges.metadata_snapshot_available -eq 1 -and $observed.gauges.metadata_snapshot_stale -eq 0){$capacityPendingReads++}
            $capacityFaultReads++
            if($observed.counters.receipt_uncertain_total -gt 0){break}
            Start-Sleep -Milliseconds 100
        }while($capacityPhase.Elapsed.TotalSeconds -lt 12)
        if($observed.counters.receipt_uncertain_total -lt 1 -or $capacityPendingBytes -le 0 -or $capacityPendingReads -lt 1){throw 'Unacknowledged real delivery was not observed as bounded uncertainty'}
        Capacity-Wait {return (Capacity-Capture GET "v1/captures/$($captureOp.id)").state -in @('stopped','expired')} 'Consumer loss did not withdraw capture'
        $capacityResult.phases.capture_fault=@{elapsed_seconds=$capacityPhase.Elapsed.TotalSeconds;core_read_pairs=$capacityFaultReads;core_read_pairs_while_pending=$capacityPendingReads;peak_observed_pending_bytes=$capacityPendingBytes;receipt_uncertain=$observed.counters.receipt_uncertain_total;payloads_recorded=$false}
        [System.IO.File]::WriteAllText((Join-Path $capacityDir 'plugin-consumer-fault.json'),($observed|ConvertTo-Json -Depth 16))
        $null=Capacity-Snapshot 'consumer-fault'
    }
    $restartCount=[Math]::Max(1,[int][Math]::Ceiling($Collectors/10))
    $restartNames=@($capacityNames|Select-Object -First $restartCount)
    $capacityPhase.Restart()
    Capacity-Compose restart --timeout 5 @restartNames
    Capacity-Wait {
        $current=@(Capacity-Inventory);$advanced=0
        foreach($item in $current){if($item.connected -and $capacityGenerations.ContainsKey($item.id) -and $item.generation -gt $capacityGenerations[$item.id]){$advanced++}}
        return $current.Count -eq $Collectors -and $advanced -ge $restartCount -and (Capacity-Request GET /api/v1/summary $capacityAdmin).connected -eq $Collectors
    } 'Stable-identity reconnects did not advance generations' ([Math]::Max(120,$Collectors))
    $capacityResult.phases.reconnect_count=$restartCount
    $capacityResult.phases.reconnect_seconds=$capacityPhase.Elapsed.TotalSeconds
    $null=Capacity-Snapshot 'reconnected'
    $capacityResult.status='passed'
    Write-Output "PASS $($capacityResult.mode): $Collectors real Collectors, registration/steady reporting/reconnect, metrics and resource snapshots."
}finally{
    try{
    if(Test-Path -LiteralPath $capacityDir){
        $capacitySorted=@($capacitySamples|Sort-Object)
        if($capacitySorted.Count){$capacityResult.http_samples=@{count=$capacitySorted.Count;p50_ms=$capacitySorted[[int][Math]::Floor(($capacitySorted.Count-1)*0.50)];p95_ms=$capacitySorted[[int][Math]::Floor(($capacitySorted.Count-1)*0.95)];max_ms=$capacitySorted[-1]}}
        try{
            if($capacityStarted){Capacity-Compose down --volumes --remove-orphans --timeout 5}
            $capacityResult.cleanup='passed'
        }catch{
            $capacityResult.cleanup='failed';$capacityResult.status='failed';throw
        }finally{
        [System.IO.File]::WriteAllText((Join-Path $capacityDir 'result.json'),($capacityResult|ConvertTo-Json -Depth 12))
        Write-Output "Result and bounded metric/resource snapshots: $capacityDir"
        }
    }
    }finally{
    $env:LINEA_FLEET_AGENT_TOKEN=$capacityOldToken
    }
}
