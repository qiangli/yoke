# Sprint 562 / e385262ac250: typed paired deployment through Outpost's existing lifecycle.
# Preview is read-only. Manifest paths are private deployment inputs, never repository constants.
param(
 [Parameter(Mandatory=$true)][string]$ManifestPath,
 [Parameter(Mandatory=$true)][string]$ManifestSha256,
 [switch]$Execute
)
$ErrorActionPreference='Stop';$ProgressPreference='SilentlyContinue'
Set-StrictMode -Version 2
function Hash([string]$p){(Get-FileHash -LiteralPath $p -Algorithm SHA256).Hash.ToLowerInvariant()}
function RequireHash([string]$p,[string]$h){if($h -cnotmatch '^[0-9a-f]{64}$' -or (Hash $p) -cne $h){throw "Hash mismatch: $p"}}
function NoReparse([string]$p){$p=[IO.Path]::GetFullPath($p);while($p){$i=Get-Item -LiteralPath $p -Force;if($i.Attributes -band [IO.FileAttributes]::ReparsePoint){throw "Reparse path: $p"};$p=[IO.Path]::GetDirectoryName($p)}}
RequireHash $ManifestPath $ManifestSha256
$m=Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json
if($m.schema -cne 's562-paired-telemetry-rollout/v1'){throw 'Unexpected manifest schema'}
$roles=@('outpost','bashy','bash','sh');$art=@{}
foreach($role in $roles){
 $a=$m.artifacts.$role;NoReparse $a.path;RequireHash $a.path $a.sha256
 if([IO.Path]::GetFileName($a.path) -cne ($role+'.exe')){throw "Wrong artifact filename for $role"}
 $art[$role]=$a
}
if($art.outpost.sha256 -ceq $art.bashy.sha256){throw 'Outpost and Bashy must be separate executables'}
RequireHash $m.archive.path $m.archive.sha256
# Inspect the exact ZIP bytes without extracting any path supplied by the archive.
Add-Type -AssemblyName System.IO.Compression.FileSystem
$z=[IO.Compression.ZipFile]::OpenRead($m.archive.path)
try{
 $names=@('outpost.exe','bashy.exe','bash.exe','sh.exe');$seen=@{}
 foreach($e in $z.Entries){
  if($e.FullName -cnotin $names -or $seen.ContainsKey($e.FullName)){throw 'Unexpected or duplicate ZIP member'}
  $stream=$e.Open();$sha=[Security.Cryptography.SHA256]::Create()
  try{$h=([BitConverter]::ToString($sha.ComputeHash($stream))).Replace('-','').ToLowerInvariant()}finally{$sha.Dispose();$stream.Dispose()}
  $role=[IO.Path]::GetFileNameWithoutExtension($e.FullName);$expected=$art[$role].sha256
  if($h -cne $expected){throw "Incorrect executable in ZIP member $($e.FullName)"};$seen[$e.FullName]=$true
 }
 if($seen.Count -ne 4){throw 'Paired ZIP must contain all four executable names'}
}finally{$z.Dispose()}
# Validation evidence is tied to this exact pair; neither source presence nor a staged packet qualifies.
RequireHash $m.validation.path $m.validation.sha256
$v=Get-Content -LiteralPath $m.validation.path -Raw | ConvertFrom-Json
if($v.schema -cne 's562-telemetry-native-acceptance/v1' -or $v.passed -ne $true -or $v.yoke_commit -cne $m.yoke_commit -or $v.bashy_commit -cne $m.bashy_commit -or $v.outpost_sha256 -cne $art.outpost.sha256 -or $v.bashy_sha256 -cne $art.bashy.sha256){throw 'Missing successful native validation for this exact binary pair'}
foreach($role in $roles){if($v.artifact_hashes.$role -cne $art[$role].sha256){throw "Validation artifact mismatch for $role"}}
# The existing upgrade command probes role, version pairing, platform, and never-downgrade order.
# We additionally pin every installed member and retain independently verified backups first.
$install=[IO.Path]::GetFullPath($m.install_dir);NoReparse $install
# Bind the MCP-selected daemon target before any backup or mutation. Dormant status is not a target.
$daemonRaw=& $art.outpost.path status --json
if($LASTEXITCODE -ne 0){throw 'Cannot read live daemon target'}
$daemon=$daemonRaw | ConvertFrom-Json
if(-not $daemon.status -or -not $daemon.status.binary_path){throw 'Live daemon binary_path missing'}
$daemonPath=[IO.Path]::GetFullPath([string]$daemon.status.binary_path)
if($daemonPath -ine (Join-Path $install 'outpost.exe')){throw 'Live daemon target differs from pinned install directory'}
NoReparse $daemonPath
$before=@();foreach($name in @('outpost.exe','bashy.exe','bash.exe','sh.exe')){
 $p=Join-Path $install $name;$expected=$m.deployed.$name
 if($expected -ceq 'absent'){if(Test-Path -LiteralPath $p){throw "Unexpected existing member: $name"};$before+=@{name=$name;path=$p;sha256='absent'};continue}
 NoReparse $p;RequireHash $p $expected;$before+=@{name=$name;path=$p;sha256=$expected}
}
$processes=@(Get-CimInstance Win32_Process | Where-Object {$_.ExecutablePath -and ([IO.Path]::GetDirectoryName($_.ExecutablePath) -ieq $install)} | Select-Object ProcessId,ParentProcessId,CreationDate,Name,ExecutablePath)
@{kind='rollout-preflight';execute=[bool]$Execute;members=$before;fresh_processes=$processes;yoke_commit=$m.yoke_commit;bashy_commit=$m.bashy_commit}|ConvertTo-Json -Depth 6 -Compress
if(-not $Execute){return}
$backup=Join-Path $m.backup_parent ('telemetry-'+[Guid]::NewGuid().ToString('N'))
[IO.Directory]::CreateDirectory($backup)|Out-Null;NoReparse $backup
foreach($b in $before){if($b.sha256 -ceq 'absent'){continue};$dest=Join-Path $backup $b.name;Copy-Item -LiteralPath $b.path -Destination $dest;RequireHash $dest $b.sha256;RequireHash $b.path $b.sha256}
$before|ConvertTo-Json -Depth 4|Set-Content -LiteralPath (Join-Path $backup 'before.json')
# Never stop guessed or stale PIDs. Complete the supported pair transaction before restart.
# Outpost owns this SSH channel: the manager must restart and reconcile on a fresh connection.
RequireHash $art.outpost.path $art.outpost.sha256;RequireHash $m.archive.path $m.archive.sha256
$upgradeStarted=$false
try{
 $upgradeStarted=$true
 & $art.outpost.path upgrade --local $m.archive.path --sha256 $m.archive.sha256 --force --no-restart 2>&1 | Tee-Object -FilePath (Join-Path $backup 'upgrade.log')
 if($LASTEXITCODE -ne 0){throw "Paired upgrade failed ($LASTEXITCODE)"}
 foreach($role in $roles){RequireHash (Join-Path $install ($role+'.exe')) $art[$role].sha256}
 @{kind='paired-swap-complete-restart-required';backup=$backup;outpost_sha256=$art.outpost.sha256;bashy_sha256=$art.bashy.sha256;restart_required=$true;fresh_connection_health_check_required=$true;storage_observation_required=$true}|ConvertTo-Json -Compress
}catch{
 $failure=$_.Exception.Message
 # Roll back only if installed bytes changed; a failed preflight must not roll back an unrelated generation.
 $changed=$false;foreach($b in $before){if($b.sha256 -ceq 'absent'){if(Test-Path -LiteralPath $b.path){$changed=$true}}elseif(-not(Test-Path -LiteralPath $b.path) -or (Hash $b.path) -cne $b.sha256){$changed=$true}}
 if($upgradeStarted -and $changed){
  & $art.outpost.path rollback 2>&1 | Tee-Object -FilePath (Join-Path $backup 'rollback.log')
  $rollbackExit=$LASTEXITCODE
  foreach($b in $before){if($b.sha256 -ceq 'absent'){if(Test-Path -LiteralPath $b.path){throw "Rollback left newly created member: $($b.name)"}}else{RequireHash $b.path $b.sha256}}
  if($rollbackExit -ne 0){throw "Upgrade failed: $failure; rollback returned $rollbackExit; backups: $backup"}
 }
 throw "Upgrade failed: $failure; backup: $backup"
}
