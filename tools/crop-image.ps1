param(
    [Parameter(Mandatory)][string]$In,
    [Parameter(Mandatory)][string]$Out,
    [int]$X = 0, [int]$Y = 0, [int]$W = 0, [int]$H = 0,
    [double]$Scale = 1
)

# Crop and zoom a capture. A 3840 px wide screenshot arrives downscaled to about
# 2000 px wherever it is being looked at, which is under half a pixel per pixel:
# anything the size of a taskbar icon or a scrollbar thumb is gone. Nearest
# neighbour on purpose, so what comes out is the pixels and not an interpolation
# of them.
Add-Type -AssemblyName System.Drawing
$src = [System.Drawing.Bitmap]::FromFile((Resolve-Path $In))
if ($W -le 0) { $W = $src.Width - $X }
if ($H -le 0) { $H = $src.Height - $Y }
$cut = $src.Clone((New-Object System.Drawing.Rectangle($X, $Y, $W, $H)), $src.PixelFormat)
$ow = [int]($W * $Scale); $oh = [int]($H * $Scale)
# Not $out: PowerShell variables are case insensitive, so that would overwrite the
# parameter with a bitmap and save the image to a path named "System.Drawing.Bitmap".
$dst = New-Object System.Drawing.Bitmap($ow, $oh)
$g = [System.Drawing.Graphics]::FromImage($dst)
$g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::NearestNeighbor
$g.DrawImage($cut, 0, 0, $ow, $oh)
$dst.Save($Out, [System.Drawing.Imaging.ImageFormat]::Png)
$g.Dispose(); $dst.Dispose(); $cut.Dispose(); $src.Dispose()
"$In [$X,$Y ${W}x$H] x$Scale -> $Out (${ow}x${oh})"
