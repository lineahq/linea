[CmdletBinding()]
param(
    [switch]$SkipBuild,
    [ValidatePattern('^[a-z0-9][a-z0-9.-]{0,40}$')][string]$ImageTag='local',
    [ValidateRange(1,254)][int]$NetworkOctet=244
)
$ErrorActionPreference='Stop'
$captureFleetRoot=Split-Path (Split-Path (Split-Path $PSScriptRoot -Parent) -Parent) -Parent
$captureID=[Guid]::NewGuid().ToString('N')
$captureProject="capture-s4-$captureID"
$captureRunDir=Join-Path $captureFleetRoot ".local/$captureProject"
$captureEnv=Join-Path $captureRunDir 'compose.env'
$captureOldToken=$env:LINEA_FLEET_AGENT_TOKEN
$captureCreated=$false
$captureChildren=[System.Collections.Generic.List[string]]::new()
$captureSequence=0
$captureConsumerSequence=0
$captureCompose=@('--project-name',$captureProject,'--env-file',$captureEnv,'--file',(Join-Path $captureFleetRoot 'deploy/compose/fleet/compose.yaml'),'--file',(Join-Path $captureFleetRoot 'deploy/compose/fleet/compose.livecapture.yaml'))
function Capture-Compose{
    & docker compose @captureCompose @args
    if($LASTEXITCODE -ne 0){throw 'S4 Compose operation failed (sensitive details omitted)'}
}
function Capture-Wait([scriptblock]$Check,[string]$Failure,[int]$Seconds=40){
    $captureDeadline=[DateTime]::UtcNow.AddSeconds($Seconds)
    do{if(& $Check){return};Start-Sleep -Milliseconds 250}while([DateTime]::UtcNow -lt $captureDeadline)
    throw $Failure
}
function Capture-Request([string]$Method,[string]$Path,[string]$Token,[string]$Body='',[int]$Expected=200,[string]$Match=''){
    $captureHeaders=@{};if($Token){$captureHeaders.Authorization="Bearer $Token"};if($Match){$captureHeaders['If-Match']=$Match}
    $captureRequest=@{Method=$Method;Uri="$captureBase$Path";Headers=$captureHeaders;SkipHttpErrorCheck=$true;TimeoutSec=6}
    if($Body){$captureRequest.Body=$Body;$captureRequest.ContentType='application/json'}
    $captureResponse=Invoke-WebRequest @captureRequest
    if($captureResponse.StatusCode -ne $Expected){throw "S4 $Method $Path returned $($captureResponse.StatusCode), expected $Expected (body omitted)"}
    if($Expected -ne 204){return ($captureResponse.Content|ConvertFrom-Json)}
}
function Capture-API([string]$Method,[string]$Path,[string]$Body='',[int]$Expected=200,[string]$Token=$captureAdmin,[string]$Match=''){
    Capture-Request $Method "/api/v1/plugins/livecapture/api/$Path" $Token $Body $Expected $Match
}
function Capture-NewChannel([string]$Token=$captureAdmin){Capture-API POST v1/channels (@{expires_at=[DateTime]::UtcNow.AddSeconds(110).ToString('o')}|ConvertTo-Json) 201 $Token}
function Capture-StartConsumer([string]$Channel,[string]$TokenFile,[double]$Ratio=1){
    $script:captureConsumerSequence++
    $captureName="$captureProject-consumer-$captureConsumerSequence"
    $null=& docker run --detach --name $captureName --network "${captureProject}_ingress" --cpus 1 --memory 128m --pids-limit 64 --read-only --cap-drop ALL --security-opt no-new-privileges --mount "type=bind,source=$TokenFile,target=/consumer_token,readonly" "linea-fleet-livecapture-consumer:${ImageTag}" --endpoint http://fleet:8080/api/v1/plugins/livecapture/api --allow-insecure-http --token-file /consumer_token --channel $Channel --duration 100s --accept-ratio $Ratio --verify-redaction
    if($LASTEXITCODE -ne 0){throw 'Independent generic consumer startup failed'}
    $captureChildren.Add($captureName)
    Capture-Wait {return ((& docker logs $captureName 2>&1)-join "`n") -match 'consumer_ready'} 'Generic consumer did not start'
    return $captureName
}
function Capture-Counts([string]$Container){
    $captureLast=$null
    foreach($captureLine in (& docker logs $Container 2>&1)){try{$captureValue=$captureLine.ToString()|ConvertFrom-Json;if($captureValue.records){$captureLast=$captureValue}}catch{}}
    return $captureLast
}
function Capture-Body([string]$Channel,[object[]]$Peers,[int]$TTL=25,[bool]$Filtered=$true){
    $captureFilter=@{}
    if($Filtered){$captureFilter=@{traces=@{span=@('status.code == STATUS_CODE_ERROR')};logs=@{log=@('severity_number >= SEVERITY_NUMBER_ERROR')};metrics=@{datapoint=@('attributes["keep"] == true')}}}
    return (@{authority_id=$Peers[0].authority_id;plugin_instance=$Peers[0].plugin_instance;id=[Guid]::NewGuid().ToString('N');channel_id=$Channel;targets=@($Peers|ForEach-Object{@{peer_id=$_.id;generation=$_.generation}});capture_points=@(@{pipeline='traces';point='egress'},@{pipeline='logs';point='egress'},@{pipeline='metrics';point='egress'});signals=@('traces','logs','metrics');filter=$captureFilter;limits=@{};expires_at=[DateTime]::UtcNow.AddSeconds($TTL).ToString('o')}|ConvertTo-Json -Depth 12)
}
function Capture-Active([string]$ID,[string]$Token=$captureAdmin){Capture-Wait {return (Capture-API GET "v1/captures/$ID" '' 200 $Token).state -eq 'active'} 'Collector did not acknowledge activation'}
function Capture-Stop([string]$ID){
    $captureCurrent=Capture-API GET "v1/captures/$ID"
    $null=Capture-API POST "v1/captures/$ID/stop" '' 202 $captureAdmin ('"'+$captureCurrent.revision+'"')
    Capture-Wait {return (Capture-API GET "v1/captures/$ID").state -in @('stopped','completed')} 'Collector did not acknowledge stop'
}
function Capture-Generate([string]$Collector){
    & docker run --rm --network "${captureProject}_capture-ingress" --cpus 1 --memory 128m --pids-limit 64 --read-only --cap-drop ALL --security-opt no-new-privileges "linea-fleet-livecapture-generator:${ImageTag}" --endpoint "${Collector}:4317"
    if($LASTEXITCODE -ne 0){throw 'Synthetic OTLP input generation failed'}
}
function Capture-IngressRefusal([string]$TokenFile,[string]$Code,[string]$PeerName='',[bool]$Oversize=$false){
    $captureExtra=@();if($PeerName){$captureExtra+=@('--server-name',$PeerName)};if($Oversize){$captureExtra+='--oversize'}
    & docker run --rm --network "${captureProject}_capture-ingress" --cpus 1 --memory 128m --pids-limit 64 --read-only --cap-drop ALL --security-opt no-new-privileges --mount "type=bind,source=$captureRunDir/plugin/tls_cert.pem,target=/certs/ca.pem,readonly" --mount "type=bind,source=$TokenFile,target=/fixture_token,readonly" "linea-fleet-livecapture-generator:${ImageTag}" --endpoint livecapture-plugin:4341 --tls-ca /certs/ca.pem --token-file /fixture_token --expect-code $Code @captureExtra
    if($LASTEXITCODE -ne 0){throw 'Capture ingress identity/bounds check failed'}
}
try{
    & (Join-Path $captureFleetRoot 'scripts/fleet/new-plugin-secrets.ps1') -Directory $captureRunDir | Out-Null
    & (Join-Path $captureFleetRoot 'scripts/fleet/new-deployment-secrets.ps1') -Directory (Join-Path $captureRunDir 'fleet') | Out-Null
    & (Join-Path $PSScriptRoot 'new-secrets.ps1') -Directory (Join-Path $captureRunDir 'plugin') | Out-Null
    $capturePath=$captureRunDir.Replace('\','/')
    $captureObserve=Join-Path $captureRunDir 'observe.yaml'
    $captureObserveConfig=@{services=@{'livecapture-plugin'=@{ports=@('127.0.0.1::8090')}};networks=@{}}
    $captureSubnet=0
    foreach($captureNetwork in @('database','ingress','plugins','capture-database','capture-ingress')){
        $captureObserveConfig.networks[$captureNetwork]=@{ipam=@{config=@(@{subnet="10.$NetworkOctet.240.$captureSubnet/28"})}}
        $captureSubnet+=16
    }
    [System.IO.File]::WriteAllText($captureObserve,($captureObserveConfig|ConvertTo-Json -Depth 12))
    $captureCompose+=@('--file',$captureObserve)
    [System.IO.File]::WriteAllText($captureEnv,"FLEET_SECRET_DIR='$capturePath/fleet'`nFLEET_LIVECAPTURE_SECRET_DIR='$capturePath/plugin'`nFLEET_SERVER_ID=s4-$captureID`nFLEET_API_PORT=0`nFLEET_AGENT_PORT=0`nCAPTURE_CONTROL_PORT=0`nCAPTURE_DELIVERY_PORT=0`nFLEET_VERSION=${ImageTag}`nCAPTURE_VERSION=${ImageTag}`n")
    if(-not $SkipBuild){
        foreach($captureTarget in @(@('runtime','linea-fleet-livecapture-compat:collector'),@('capture-smoke','linea-fleet-livecapture-compat:capture-smoke'))){
            & docker build --file (Join-Path $captureFleetRoot 'deploy/docker/linea-collector.Dockerfile') --target $captureTarget[0] --tag $captureTarget[1] $captureFleetRoot
            if($LASTEXITCODE -ne 0){throw 'Real unchanged Collector build failed'}
        }
        Capture-Compose build fleet
        Capture-Compose build livecapture-plugin
        foreach($captureTarget in @(@('client',"linea-fleet-livecapture-consumer:${ImageTag}"),@('fixture',"linea-fleet-livecapture-generator:${ImageTag}"))){
            & docker build --file (Join-Path $captureFleetRoot 'deploy/docker/linea-fleet-livecapture.Dockerfile') --target $captureTarget[0] --tag $captureTarget[1] $captureFleetRoot
            if($LASTEXITCODE -ne 0){throw 'Generic client or synthetic generator build failed'}
        }
    }
    $captureBaseline=& docker run --rm --network none --cpus 2 --memory 512m --pids-limit 128 --read-only --cap-drop ALL --security-opt no-new-privileges -e GOMAXPROCS=2 linea-fleet-livecapture-compat:capture-smoke 2>&1
    if($LASTEXITCODE -ne 0 -or ($captureBaseline -join "`n") -notmatch 'Capture smoke passed:'){throw 'Unchanged Collector baseline failed (logs omitted)'}
    $captureCreated=$true
    Capture-Compose up --detach --wait postgres capture-postgres
    Capture-Compose run --rm -T migrate | Out-Null
    Capture-Compose run --rm -T capture-migrate | Out-Null
    $captureBootstrap=Capture-Compose run --rm -T bootstrap | ConvertFrom-Json
    $captureAdmin=$captureBootstrap.token
    if(-not $captureAdmin){throw 'Disposable fleet bootstrap failed'}
    $captureAdminFile=Join-Path $captureRunDir 'admin_token';[System.IO.File]::WriteAllText($captureAdminFile,$captureAdmin)
    if(-not $IsWindows){& chmod 444 $captureAdminFile}
    Capture-Compose up --detach --wait fleet
    $captureCore=Capture-Compose ps --quiet fleet
    $capturePort=(& docker port $captureCore 8080/tcp) -replace '^.*:',''
    $captureBase="http://127.0.0.1:$capturePort"
    if((Capture-Request GET /readyz '').status -ne 'ready'){throw 'Core failed without plugin'}
    if((Capture-Request GET /api/v1/plugins/livecapture $captureAdmin).state -ne 'unavailable'){throw 'Absent plugin advertised ready'}
    Capture-Compose up --detach --wait livecapture-plugin
    $capturePlugin=Capture-Compose ps --quiet livecapture-plugin
    $capturePluginPG=Capture-Compose ps --quiet capture-postgres
    $captureFleetPG=Capture-Compose ps --quiet postgres
    $null=Capture-API GET v1/openapi.json '' 401 ''
    $captureSpec=Capture-API GET v1/openapi.json
    if($captureSpec.info.title -ne 'Optional livecapture plugin API'){throw 'Plugin-owned schema was not routed opaquely'}
    # The unchanged extension presents a fleet agent credential whose capture
    # label allows the s4 collector group; the plugin introspects it via core.
    $captureUnlabeled=Capture-Request POST /api/v1/credentials $captureAdmin (@{id=[Guid]::NewGuid().ToString('N');name='s4 unlabeled';role='agent';expires_at=[DateTime]::UtcNow.AddDays(1).ToString('o')}|ConvertTo-Json) 201
    $captureUnlabeledFile=Join-Path $captureRunDir 'unlabeled_token';[System.IO.File]::WriteAllText($captureUnlabeledFile,$captureUnlabeled.token)
    $captureCredential=[Guid]::NewGuid().ToString('N')
    $captureIssued=Capture-Request POST /api/v1/credentials $captureAdmin (@{id=$captureCredential;name='s4 collectors';role='agent';expires_at=[DateTime]::UtcNow.AddDays(1).ToString('o');labels=@{capture='s4'}}|ConvertTo-Json) 201
    $captureAgentFile=Join-Path $captureRunDir 'agent_token';[System.IO.File]::WriteAllText($captureAgentFile,$captureIssued.token)
    if(-not $IsWindows){& chmod 444 $captureAgentFile $captureUnlabeledFile}
    $env:LINEA_FLEET_AGENT_TOKEN=$captureIssued.token
    foreach($capturePod in @('collector-a','collector-b')){
        $captureName="$captureProject-$capturePod"
        $null=& docker run --detach --init --name $captureName --network "${captureProject}_capture-ingress" --network-alias $capturePod --cpus 2 --memory 512m --pids-limit 128 --read-only --cap-drop ALL --security-opt no-new-privileges --mount "type=bind,source=$captureFleetRoot/plugins/livecapture/examples/collector.yaml,target=/fixture.yaml,readonly" --mount "type=bind,source=$captureRunDir/plugin/tls_cert.pem,target=/certs/ca.pem,readonly" --env LINEA_FLEET_AGENT_TOKEN --env "POD_NAME=$capturePod" -e GOMAXPROCS=2 linea-fleet-livecapture-compat:collector --config /fixture.yaml
        if($LASTEXITCODE -ne 0){throw 'Real Collector startup failed'}
        $captureChildren.Add($captureName)
    }
    Capture-Wait {$captureLive=(Capture-API GET v1/peers).items;return $captureLive.Count -eq 2 -and @($captureLive|Where-Object{$_.identity.capture_points.Count -eq 6}).Count -eq 2} 'TLS/bearer capture peers or catalogs did not register'
    $capturePeers=@((Capture-API GET v1/peers).items)
    $capturePage=Capture-API GET 'v1/peers?limit=1'
    $captureNextPage=Capture-API GET ('v1/peers?limit=1&cursor='+[Uri]::EscapeDataString($capturePage.next_cursor))
    if($capturePage.items.Count -ne 1 -or $captureNextPage.items.Count -ne 1 -or $capturePage.items[0].id -eq $captureNextPage.items[0].id){throw 'Public peer catalog pagination failed'}
    $null=Capture-API GET 'v1/peers?limit=1&limit=2' '' 400
    $captureMetrics=Capture-API GET v1/metrics
    if($captureMetrics.gauges.peers -ne 2 -or $captureMetrics.limits.control_connections -ne 272){throw 'Public plugin metric limits/counts mismatch'}
    $captureMetricsPort=(& docker port $capturePlugin 8090/tcp) -replace '^.*:',''
    $captureMetricsBearer=[System.IO.File]::ReadAllText((Join-Path $captureRunDir 'plugin/metrics_token'))
    $captureScrape=Invoke-WebRequest "http://127.0.0.1:$captureMetricsPort/metrics" -Headers @{Authorization="Bearer $captureMetricsBearer"}
    if($captureScrape.Content -notmatch 'linea_fleet_livecapture_peers 2'){throw 'Private authenticated plugin scrape failed'}
    $captureDenied=Invoke-WebRequest "http://127.0.0.1:$captureMetricsPort/metrics" -Headers @{Authorization="Bearer $captureAdmin"} -SkipHttpErrorCheck
    if($captureDenied.StatusCode -ne 401){throw 'General API token crossed private metrics credential scope'}
    $captureCoreMetrics=Invoke-WebRequest "$captureBase/metrics" -Headers @{Authorization="Bearer $captureAdmin"}
    if($captureCoreMetrics.Content -notmatch 'linea_fleet_database_owner_wait_seconds_count' -or $captureCoreMetrics.Content -notmatch 'linea_fleet_plugins\{state="ready"\} 1' -or $captureCoreMetrics.Content -match 'linea_fleet_livecapture_'){throw 'Core operational metrics missing or capture boundary crossed'}
    Write-Output 'PASS S6.1 public cursor pages, private/read-only scrape credentials, plugin effective limits and domain-free core metrics.'
    if(@((Capture-Request GET /api/v1/agents $captureAdmin).items).Count -ne 0){throw 'Capture peer identities were invented as OpAMP UIDs'}
    $captureChannel=Capture-NewChannel
    $captureConsumer=Capture-StartConsumer $captureChannel.id $captureAdminFile
    $captureBody=Capture-Body $captureChannel.id $capturePeers
    $captureOperation=Capture-API POST v1/captures $captureBody 202
    Capture-Active $captureOperation.id
    Capture-IngressRefusal (Join-Path $captureRunDir 'plugin/host_token') Unauthenticated
    Capture-IngressRefusal $captureUnlabeledFile Unauthenticated
    Capture-IngressRefusal $captureAgentFile PermissionDenied
    Capture-IngressRefusal $captureAgentFile ResourceExhausted '' $true
    Capture-IngressRefusal $captureAgentFile Unavailable wrong-host
    $captureRetry=Capture-API POST v1/captures $captureBody 202
    if($captureRetry.id -ne $captureOperation.id){throw 'Identical create did not reconcile'}
    $captureChanged=$captureBody|ConvertFrom-Json;$captureChanged.limits=@{total_records=1}
    $null=Capture-API POST v1/captures ($captureChanged|ConvertTo-Json -Depth 12) 409
    $captureOther=Capture-NewChannel
    $null=Capture-API POST v1/captures (Capture-Body $captureOther.id $capturePeers) 409
    $captureRaw=$captureBody|ConvertFrom-Json;$captureRaw.id=[Guid]::NewGuid().ToString('N');$captureRaw.capture_points=@(@{pipeline='traces';point='ingress'});$captureRaw.signals=@('traces')
    $null=Capture-API POST v1/captures ($captureRaw|ConvertTo-Json -Depth 12) 409
    # The same targets are busy above; raw authorization is tested after release.
    Capture-Generate collector-a
    Capture-Generate collector-b
    Capture-Wait {$captureCounts=Capture-Counts $captureConsumer;return $captureCounts -and $captureCounts.records.traces -eq 2 -and $captureCounts.records.logs -eq 2 -and $captureCounts.records.metrics -eq 2} 'Generic public consumer did not receive filtered redacted traces/logs/metrics from both Collectors'
    $null=Capture-API POST "v1/captures/$($captureOperation.id)/stop" '' 428
    $null=Capture-API POST "v1/captures/$($captureOperation.id)/stop" '' 412 $captureAdmin '"999999"'
    Capture-Stop $captureOperation.id
    $null=& docker stop --time 5 $captureConsumer
    $null=Capture-API POST v1/captures ($captureRaw|ConvertTo-Json -Depth 12) 403
    Write-Output 'PASS public API: real TLS/bearer peers, generic consumer receipts for all signals, OTTL filtering/redaction, fixed roster, overlap denial, raw denial, idempotency and acknowledged stop.'

    # Restart the same real Collector: its capture identity remains, generation advances.
    $captureOldPeer=@($capturePeers|Where-Object{$_.identity.pod -eq 'collector-a'})[0]
    $null=& docker restart "$captureProject-collector-a"
    if($LASTEXITCODE -ne 0){throw 'Collector restart failed'}
    Capture-Wait {$captureNewPeers=(Capture-API GET v1/peers).items;return @($captureNewPeers|Where-Object{$_.id -eq $captureOldPeer.id -and $_.generation -gt $captureOldPeer.generation -and $_.identity.capture_points.Count -eq 6}).Count -eq 1} 'Real Collector generation did not advance'
    $capturePeers=@((Capture-API GET v1/peers).items)
    $captureFreshChannel=Capture-NewChannel
    $null=Capture-API POST v1/captures (Capture-Body $captureFreshChannel.id @($captureOldPeer)) 409

    # Partial receipts are returned as OTLP partial success, rather than full ACK.
    $capturePartialConsumer=Capture-StartConsumer $captureFreshChannel.id $captureAdminFile 0.5
    $captureInvalid=Capture-Body $captureFreshChannel.id @($capturePeers[0]) | ConvertFrom-Json
    $captureInvalid.filter.traces.span=@('invalid OTTL expression (')
    $captureInvalidOperation=Capture-API POST v1/captures ($captureInvalid|ConvertTo-Json -Depth 12) 202
    Capture-Wait {return (Capture-API GET "v1/captures/$($captureInvalidOperation.id)").state -eq 'failed'} 'Collector filter rejection was falsely reported active or lost'
    $capturePartial=Capture-API POST v1/captures (Capture-Body $captureFreshChannel.id @($capturePeers[0]) 25 $false) 202
    Capture-Active $capturePartial.id
    Capture-Generate $capturePeers[0].identity.pod
    Capture-Wait {$captureCounts=Capture-Counts $capturePartialConsumer;return $captureCounts -and $captureCounts.accepted_records -eq 3 -and $captureCounts.rejected_records -eq 3} 'Consumer partial acceptance was not preserved'
    Capture-Stop $capturePartial.id
    $null=& docker stop --time 5 $capturePartialConsumer

    # A channel without a polling/acknowledging consumer must never ACK OTLP.
    $captureLostChannel=Capture-NewChannel
    $captureLost=Capture-API POST v1/captures (Capture-Body $captureLostChannel.id @($capturePeers[0]) 12) 202
    Capture-Active $captureLost.id
    Capture-Generate $capturePeers[0].identity.pod
    Capture-Wait {$captureOutcome=Capture-API GET "v1/captures/$($captureLost.id)";return $captureOutcome.state -in @('stopped','expired') -and $captureOutcome.reason -in @('receipt_uncertain','consumer_unavailable')} 'Lost consumer did not withdraw capture with visible uncertainty'

    $captureExpiryChannel=Capture-NewChannel
    $captureExpiryConsumer=Capture-StartConsumer $captureExpiryChannel.id $captureAdminFile
    $captureExpiry=Capture-API POST v1/captures (Capture-Body $captureExpiryChannel.id @($capturePeers[0]) 3) 202
    Capture-Active $captureExpiry.id
    Capture-Wait {return (Capture-API GET "v1/captures/$($captureExpiry.id)").state -eq 'expired'} 'Absolute capture expiry was not enforced'
    $null=& docker stop --time 5 $captureExpiryConsumer
    # The service's expiry guard may conservatively retain a lease for five seconds.
    Capture-Wait {return (& docker exec $capturePluginPG psql -U postgres -d livecapture -At -c "SELECT count(*) FROM capture.leases WHERE expires_at>now()") -eq '0'} 'Expiry guard did not elapse'
    Write-Output 'PASS reconnect generation, partial receipts, lost-consumer backpressure and absolute expiry.'
    $captureObserved=Capture-API GET v1/metrics
    if($captureObserved.counters.receipt_uncertain_total -lt 1 -or $captureObserved.counters.consumer_rejected_traces_records_total -lt 1){throw 'Real delivery partial/uncertain outcomes not observable'}

    $captureDrainChannel=Capture-NewChannel
    $captureDrainConsumer=Capture-StartConsumer $captureDrainChannel.id $captureAdminFile
    $captureDrainBody=Capture-Body $captureDrainChannel.id @($capturePeers[0]) 20
    $captureDrain=Capture-API POST v1/captures $captureDrainBody 202
    Capture-Active $captureDrain.id
    $captureInfo=Capture-Request GET /api/v1/plugins/livecapture $captureAdmin
    $captureInfo=Capture-Request POST /api/v1/plugins/livecapture/lifecycle $captureAdmin '{"action":"drain"}' 200 ('"'+$captureInfo.revision+'"')
    $null=Capture-API GET v1/peers '' 503
    $captureInfo=Capture-Request POST /api/v1/plugins/livecapture/lifecycle $captureAdmin '{"action":"disable"}' 200 ('"'+$captureInfo.revision+'"')
    $null=Capture-Request GET /readyz ''
    Start-Sleep -Seconds 2
    $captureInfo=Capture-Request POST /api/v1/plugins/livecapture/lifecycle $captureAdmin '{"action":"enable"}' 200 ('"'+$captureInfo.revision+'"')
    Capture-Wait {return (Capture-Request GET /api/v1/plugins/livecapture $captureAdmin).state -eq 'ready'} 'Plugin did not re-register after enable'
    Capture-Wait {try{return (Capture-API GET "v1/captures/$($captureDrain.id)").state -eq 'stopped'}catch{return $false}} 'Plugin failed to withdraw capture during drain/disable'
    $captureReconcile=Capture-API POST v1/captures $captureDrainBody 202
    if($captureReconcile.state -ne 'stopped'){throw 'Lifecycle recovery replayed a capture'}
    $null=& docker stop --time 5 $captureDrainConsumer
    Write-Output 'PASS generic lifecycle drain/disable withdraws real capture; re-enable does not replay it.'

    # One plugin authority even if an operator accidentally starts another replica.
    $captureShadow=& docker compose @captureCompose run --rm -T --no-deps livecapture-plugin 2>&1
    if($LASTEXITCODE -eq 0){throw 'A competing plugin authority acquired the database'}
    $captureGrantCheck=& docker exec $capturePluginPG psql -U postgres -d livecapture -At -c "SELECT has_schema_privilege('capture_app','capture','CREATE'),has_table_privilege('capture_app','capture.schema_version','UPDATE'),has_table_privilege('capture_app','capture.operations','DELETE')"
    if($LASTEXITCODE -ne 0 -or $captureGrantCheck -ne 'f|f|f'){throw 'Plugin runtime grants are not restricted'}
    $captureNoCoreTables=& docker exec $captureFleetPG psql -U postgres -d fleet -At -c "SELECT to_regclass('capture.operations') IS NULL"
    if($LASTEXITCODE -ne 0 -or $captureNoCoreTables -ne 't'){throw 'Capture schema leaked into fleet database'}

    # A lost management credential cannot keep a consumer channel alive.
    $captureOtherID=[Guid]::NewGuid().ToString('N')
    $captureOtherKey=Capture-Request POST /api/v1/credentials $captureAdmin (@{id=$captureOtherID;name='s4-other-client';role='admin';expires_at=[DateTime]::UtcNow.AddHours(1).ToString('o')}|ConvertTo-Json) 201
    $captureOtherFile=Join-Path $captureRunDir 'other_admin_token';[System.IO.File]::WriteAllText($captureOtherFile,$captureOtherKey.token)
    if(-not $IsWindows){& chmod 444 $captureOtherFile}
    $captureOtherChannel=Capture-NewChannel $captureOtherKey.token
    $captureOtherConsumer=Capture-StartConsumer $captureOtherChannel.id $captureOtherFile
    $captureOwned=Capture-API POST v1/captures (Capture-Body $captureOtherChannel.id @($capturePeers[0]) 14) 202 $captureOtherKey.token
    Capture-Active $captureOwned.id $captureOtherKey.token
    $null=Capture-API GET "v1/captures/$($captureOwned.id)" '' 403
    $null=Capture-Request DELETE "/api/v1/credentials/$captureOtherID" $captureAdmin '' 204
    $null=Capture-API GET "v1/channels/$($captureOtherChannel.id)/next" '' 401 $captureOtherKey.token
    Capture-Wait {$captureOwnerOutcome=& docker exec $capturePluginPG psql -U postgres -d livecapture -At -c "SELECT state FROM capture.operations WHERE id='$($captureOwned.id)'";return $captureOwnerOutcome -in @('stopped','expired')} 'Revoked client retained capture execution'
    $null=& docker stop --time 5 $captureOtherConsumer

    # Core crash removes host authority and consumer access; plugin withdraws control.
    $captureCoreChannel=Capture-NewChannel
    $captureCoreConsumer=Capture-StartConsumer $captureCoreChannel.id $captureAdminFile
    $captureCoreBody=Capture-Body $captureCoreChannel.id @($capturePeers[0]) 20
    $captureCoreOperation=Capture-API POST v1/captures $captureCoreBody 202
    Capture-Active $captureCoreOperation.id
    $null=& docker kill $captureCore
    if($LASTEXITCODE -ne 0){throw 'Core crash injection failed'}
    Capture-Wait {$captureCrashOutcome=& docker exec $capturePluginPG psql -U postgres -d livecapture -At -c "SELECT state FROM capture.operations WHERE id='$($captureCoreOperation.id)'";return $captureCrashOutcome -eq 'stopped'} 'Core loss did not withdraw capture'
    Capture-Compose up --detach --wait fleet
    $captureCore=Capture-Compose ps --quiet fleet
    $capturePort=(& docker port $captureCore 8080/tcp) -replace '^.*:',''
    $captureBase="http://127.0.0.1:$capturePort"
    Capture-Wait {try{return (Capture-API GET "v1/captures/$($captureCoreOperation.id)").state -eq 'stopped'}catch{return $false}} 'Core recovery did not preserve stopped outcome'
    $captureRetryAfterCore=Capture-API POST v1/captures $captureCoreBody 202
    if($captureRetryAfterCore.state -ne 'stopped'){throw 'Core recovery replayed a capture'}
    $null=& docker stop --time 5 $captureCoreConsumer
    Write-Output 'PASS competing-authority denial, separate/narrow storage, owner isolation, API-key revocation and core crash withdrawal.'

    # Plugin crash: no execution replay and no new lease while old work is uncertain.
    $captureCrashChannel=Capture-NewChannel
    $captureCrashConsumer=Capture-StartConsumer $captureCrashChannel.id $captureAdminFile
    $captureCrashBody=Capture-Body $captureCrashChannel.id @($capturePeers[0]) 12
    $captureCrashOperation=Capture-API POST v1/captures $captureCrashBody 202
    Capture-Active $captureCrashOperation.id
    $captureBeforeCrash=Capture-Request GET /api/v1/plugins/livecapture $captureAdmin
    $null=& docker kill $capturePlugin
    if($LASTEXITCODE -ne 0){throw 'Plugin crash injection failed'}
    Capture-Wait {return (Capture-Request GET /api/v1/plugins/livecapture $captureAdmin).state -eq 'unavailable'} 'Crashed plugin stayed ready'
    $null=Capture-Request GET /readyz ''
    Capture-Compose up --detach --wait livecapture-plugin
    Capture-Wait {try{return (Capture-API GET "v1/captures/$($captureCrashOperation.id)").state -eq 'interrupted'}catch{return $false}} 'Plugin recovery resumed or lost uncertain execution'
    Capture-Wait {$captureReconnected=(Capture-API GET v1/peers).items;return $captureReconnected.Count -eq 2 -and @($captureReconnected|Where-Object{$_.identity.capture_points.Count -eq 6}).Count -eq 2} 'Existing Collectors did not reconnect after plugin crash'
    $captureAfterCrash=Capture-Request GET /api/v1/plugins/livecapture $captureAdmin
    if($captureBeforeCrash.instance_id -eq $captureAfterCrash.instance_id -or $captureBeforeCrash.epoch -eq $captureAfterCrash.epoch){throw 'Plugin authority was not fenced across crash'}
    $captureRetryAfterPlugin=Capture-API POST v1/captures $captureCrashBody 202
    if($captureRetryAfterPlugin.state -ne 'interrupted'){throw 'Identical post-crash retry dispatched again'}
    $captureRecoveredPeers=@((Capture-API GET v1/peers).items)
    $captureReservedPeer=@($captureRecoveredPeers|Where-Object{$_.id -eq $captureCrashOperation.targets[0].peer_id})[0]
    $captureRecoveredChannel=Capture-NewChannel
    $captureRecoveredConsumer=Capture-StartConsumer $captureRecoveredChannel.id $captureAdminFile
    $captureBlockedBody=Capture-Body $captureRecoveredChannel.id @($captureReservedPeer) 20
    $null=Capture-API POST v1/captures $captureBlockedBody 409
    Capture-Wait {return (& docker exec $capturePluginPG psql -U postgres -d livecapture -At -c "SELECT count(*) FROM capture.leases WHERE expires_at>now()") -eq '0'} 'Uncertain lease expiry guard did not elapse'
    $captureAfterGuard=Capture-API POST v1/captures (Capture-Body $captureRecoveredChannel.id @($captureReservedPeer) 12) 202
    Capture-Active $captureAfterGuard.id
    Capture-Generate $captureReservedPeer.identity.pod
    Capture-Wait {$captureCounts=Capture-Counts $captureRecoveredConsumer;return $captureCounts -and $captureCounts.records.traces -eq 1 -and $captureCounts.records.logs -eq 1 -and $captureCounts.records.metrics -eq 1} 'Explicit new capture after recovery did not deliver'
    Capture-Stop $captureAfterGuard.id
    $null=& docker stop --time 5 $captureCrashConsumer
    $null=& docker stop --time 5 $captureRecoveredConsumer
    $capturePeers=$captureRecoveredPeers
    Write-Output 'PASS plugin crash: metadata and conservative leases survive, stale authority is fenced, no replay, explicit new capture works after expiry.'

    # Revoking the shared fleet agent credential in core closes every member.
    $captureRevocationChannel=Capture-NewChannel
    $captureRevocationConsumer=Capture-StartConsumer $captureRevocationChannel.id $captureAdminFile
    $captureRevocation=Capture-API POST v1/captures (Capture-Body $captureRevocationChannel.id $capturePeers 6) 202
    Capture-Active $captureRevocation.id
    $null=Capture-Request DELETE "/api/v1/credentials/$captureCredential" $captureAdmin '' 204
    Capture-Wait {return (Capture-API GET v1/peers).items.Count -eq 0} 'Revoked fleet credential retained control streams'
    Capture-Wait {return (Capture-API GET "v1/captures/$($captureRevocation.id)").state -eq 'expired'} 'Revoked execution did not remain bounded by expiry'
    $null=& docker stop --time 5 $captureRevocationConsumer
    $capturePersistedAuthority=$captureRevocation.authority_id
    Capture-Compose stop --timeout 5 livecapture-plugin
    $captureDump="/tmp/$captureProject.dump"
    $null=& docker exec $capturePluginPG pg_dump -U postgres -d livecapture -Fc -f $captureDump
    if($LASTEXITCODE -ne 0){throw 'Plugin fixture dump failed'}
    $captureRestore="$captureProject-restore"
    $null=& docker run --detach --name $captureRestore --network "${captureProject}_capture-database" --cpus 1 --memory 256m --pids-limit 128 --env POSTGRES_PASSWORD_FILE=/run/secrets/postgres_password --env POSTGRES_DB=livecapture --mount "type=bind,source=$captureRunDir/plugin/postgres_password,target=/run/secrets/postgres_password,readonly" --mount "type=bind,source=$captureRunDir/plugin/migrate_password,target=/run/secrets/capture_migrate_password,readonly" --mount "type=bind,source=$captureRunDir/plugin/runtime_password,target=/run/secrets/capture_runtime_password,readonly" --mount "type=bind,source=$captureFleetRoot/plugins/livecapture/deploy/init-db.sh,target=/docker-entrypoint-initdb.d/10-capture.sh,readonly" --tmpfs /var/lib/postgresql/data postgres:17@sha256:b8faeff7712663c2773a190181e1246e37e364b887611c04658c28b44e2408fe
    if($LASTEXITCODE -ne 0){throw 'Fresh plugin restore database startup failed'}
    $captureChildren.Add($captureRestore)
    Capture-Wait {& docker exec $captureRestore pg_isready -h 127.0.0.1 -U postgres -d livecapture *> $null;return $LASTEXITCODE -eq 0} 'Fresh restore database did not become ready'
    $captureDumpHost=Join-Path $captureRunDir 'plugin.dump'
    $null=& docker cp "${capturePluginPG}:$captureDump" $captureDumpHost
    if($LASTEXITCODE -ne 0){throw 'Fixture backup copy failed'}
    $null=& docker cp $captureDumpHost "${captureRestore}:/tmp/plugin.dump"
    if($LASTEXITCODE -ne 0){throw 'Restore archive copy failed'}
    $null=& docker exec $captureRestore pg_restore -U postgres --exit-on-error --dbname livecapture /tmp/plugin.dump
    if($LASTEXITCODE -ne 0){throw 'Fresh plugin restore failed'}
    $captureRestoreMetadata=& docker exec $captureRestore psql -U postgres -d livecapture -At -c "SELECT authority_id FROM capture.metadata; SELECT count(*) FROM capture.schema_version; SELECT state FROM capture.operations WHERE id='$($captureCrashOperation.id)'"
    if($LASTEXITCODE -ne 0 -or $captureRestoreMetadata[0] -ne $capturePersistedAuthority -or $captureRestoreMetadata[1] -ne '2' -or $captureRestoreMetadata[2] -ne 'interrupted'){throw 'Restored plugin lost authority, schema or interrupted metadata'}
    $captureRuntimeFile=Join-Path $captureRunDir 'plugin/runtime_url'
    $captureRuntimeValue=[System.IO.File]::ReadAllText($captureRuntimeFile)
    [System.IO.File]::WriteAllText($captureRuntimeFile,$captureRuntimeValue.Replace('@capture-postgres:5432/',"@${captureRestore}:5432/"))
    Capture-Compose up --detach --wait livecapture-plugin
    Capture-Wait {return (Capture-API GET v1/peers).items.Count -eq 0} 'Revoked fleet credential reconnected after plugin restart'
    if((Capture-API GET "v1/captures/$($captureCrashOperation.id)").state -ne 'interrupted'){throw 'Runtime did not reopen restored operation metadata'}
    $null=& docker stop --time 5 $captureRestore
    if($LASTEXITCODE -ne 0){throw 'Plugin database outage injection failed'}
    Capture-Wait {return (& docker inspect $capturePlugin --format '{{.State.Running}}') -eq 'false'} 'Plugin stayed active after losing its database authority'
    if((Capture-Request GET /readyz '').status -ne 'ready'){throw 'Plugin database outage affected core reporting'}
    Write-Output 'PASS core credential revocation, fresh-database metadata restore, and plugin database-outage failure isolation.'
    Write-Output "Real Collector source revision: $captureRevision"
}finally{
    foreach($captureName in $captureChildren){& docker logs $captureName *> (Join-Path $captureRunDir "$captureName.log");$null=& docker rm --force $captureName 2>&1}
    if($captureCreated){Capture-Compose down --volumes --remove-orphans --timeout 5}
    $env:LINEA_FLEET_AGENT_TOKEN=$captureOldToken
}
