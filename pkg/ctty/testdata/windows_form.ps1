
param([string]$Text,[string]$Title,[string]$Kind,[string]$Choices)
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$labels = ConvertFrom-Json -InputObject $Choices
$f = New-Object System.Windows.Forms.Form
$f.Text=$Title; $f.Width=660; $f.Height=310; $f.TopMost=$true; $f.StartPosition='CenterScreen'
$l=New-Object System.Windows.Forms.Label; $l.Text=$Text; $l.SetBounds(15,15,620,140)
$t=New-Object System.Windows.Forms.TextBox; $t.SetBounds(15,160,620,28)
if($Kind -eq 'secret') { $t.UseSystemPasswordChar=$true }
$show=New-Object System.Windows.Forms.CheckBox; $show.Text='Show'; $show.SetBounds(15,195,80,24)
$show.Add_CheckedChanged({ $t.UseSystemPasswordChar = -not $show.Checked })
$f.Controls.AddRange(@($l,$t,$show))
$script:chosen=''
for($i=0;$i -lt $labels.Count;$i++) {
 $b=New-Object System.Windows.Forms.Button; $b.Text=$labels[$i]; $b.SetBounds((15+$i*155),230,150,30)
 $choice=$labels[$i]; $b.Add_Click({ $script:chosen=$this.Text; $f.Close() })
 $f.Controls.Add($b)
}
$f.Add_Shown({ $f.Activate(); $t.Focus() }); [void]$f.ShowDialog()
@{choice=$script:chosen; value=$t.Text} | ConvertTo-Json -Compress
