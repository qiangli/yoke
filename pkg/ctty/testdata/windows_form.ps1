
param([string]$Text,[string]$Title,[string]$Kind,[string]$Choices,[string]$Pattern,[string]$Category,[string]$Peek)
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$labels = ConvertFrom-Json -InputObject $Choices
$f = New-Object System.Windows.Forms.Form
$f.Text=$Title; $f.Width=800; $f.Height=350; $f.TopMost=$true; $f.StartPosition='CenterScreen'
$l=New-Object System.Windows.Forms.Label; $l.Text=$Text+[Environment]::NewLine+'Clipboard history apps and Universal Clipboard may keep a copy.'; $l.SetBounds(15,15,760,145)
$t=New-Object System.Windows.Forms.TextBox; $t.SetBounds(15,170,760,28)
if($Kind -eq 'secret') { $t.UseSystemPasswordChar=$true }
$show=New-Object System.Windows.Forms.CheckBox; $show.Text='Show'; $show.SetBounds(15,205,80,24)
$show.Add_CheckedChanged({ $t.UseSystemPasswordChar = -not $show.Checked })
$show.Visible=($Kind -eq 'secret')
$status=New-Object System.Windows.Forms.Label; $status.SetBounds(105,205,670,24)
$f.Controls.AddRange(@($l,$t,$show,$status))
$script:chosen=''; $script:value=''; $script:clipButton=$null
function Read-Clipboard { try { return [System.Windows.Forms.Clipboard]::GetText() } catch { return '' } }
function Clipboard-Reason([string]$v) {
 if([string]::IsNullOrEmpty($v)) { return 'clipboard is empty' }
 if(-not [regex]::IsMatch($v,$Pattern)) { return "clipboard doesn't look like a $Category" }
 return ''
}
function Refresh-Clipboard {
 if($null -eq $script:clipButton) { return }
 if($Peek -eq 'false') { $script:clipButton.Enabled=$true; $status.Text='Clipboard checked when clicked'; return }
 $why=Clipboard-Reason (Read-Clipboard)
 $script:clipButton.Enabled=($why -eq ''); $status.Text=$why
}
for($i=0;$i -lt $labels.Count;$i++) {
 $b=New-Object System.Windows.Forms.Button; $b.Text=$labels[$i]; $b.SetBounds((15+$i*155),245,150,32)
 if($b.Text -eq 'Use clipboard') { $script:clipButton=$b }
 $b.Add_Click({
  if($this.Text -eq 'Use clipboard') {
   $v=Read-Clipboard; $why=Clipboard-Reason $v
   if($why -ne '') { $status.Text=$why; Refresh-Clipboard; return }
   if((Read-Clipboard) -ceq $v) { [System.Windows.Forms.Clipboard]::Clear() }
   $script:value=$v
  } elseif($this.Text -eq $labels[0]) { $script:value=$t.Text }
  $script:chosen=$this.Text; $f.Close()
 })
 $f.Controls.Add($b)
}
$timer=New-Object System.Windows.Forms.Timer; $timer.Interval=350
$timer.Add_Tick({ Refresh-Clipboard }); if($Peek -ne 'false') { $timer.Start() }
$f.Add_Shown({ Refresh-Clipboard; $f.Activate(); $t.Focus() }); [void]$f.ShowDialog(); $timer.Stop(); $timer.Dispose()
@{choice=$script:chosen; value=$script:value} | ConvertTo-Json -Compress
