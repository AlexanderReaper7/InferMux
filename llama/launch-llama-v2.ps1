<#
launch-llama-v2.ps1 - llama.cpp launcher, v2 (router mode + per-model presets).

Usage:
  ./launch-llama-v2.ps1                        # server on :5001 in a new Windows Terminal tab
  ./launch-llama-v2.ps1 server -Foreground     # same, but in the current console
  ./launch-llama-v2.ps1 cli -Model Qwopus3.6-35B-A3B-Coder-MTP-Q4_K_M
  ./launch-llama-v2.ps1 bench -Model Octen-Embedding-4B.Q8_0 [extra llama-bench args]

Per-model settings (context size, KV-cache quant, MTP, embedding mode) live in
models-preset.ini next to this script - edit that file, not this one. The
server discovers every *.gguf in $ModelsDir and applies the matching preset
section when a model is loaded.

This script and the preset are version-controlled here, in llama-warden. The
llama.cpp BINARIES are not: they are an unpacked upstream release living in
$LlamaDir, which also owns the logs the servers write. Set LLAMA_CPP_DIR to
point at a different unpack.

Replaces launch-llama.ps1 (kept for reference).
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet("server", "cli", "bench")]
    [string]$SubCommand = "server",

    # cli/bench only: model name = file name in $ModelsDir without ".gguf"
    [string]$Model = "Qwopus3.6-35B-A3B-Coder-MTP-Q4_K_M",

    # Run in the current console (blocking) instead of a new Windows Terminal tab
    [switch]$Foreground = $true,

    # No console at all: hidden process, output goes to the log file only. This is
    # how the warden (llama-warden, src/warden/agent.py) starts the server, since
    # a service-started process has no console to attach to.
    [switch]$Detached,

    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$PassthroughArgs
)

$ModelsDir  = "C:\selfhosting\models"
# The preset travels with this script; the executables and the logs do not. A
# release unpack is somebody else's tree and nothing here belongs in it.
$PresetFile = Join-Path $PSScriptRoot "models-preset.ini"
$LlamaDir   = if ($env:LLAMA_CPP_DIR) { $env:LLAMA_CPP_DIR } else { "C:\selfhosting\llama-cpp" }
if (-not (Test-Path $LlamaDir)) { throw "No llama.cpp build at $LlamaDir (set LLAMA_CPP_DIR)" }
$Port       = 5001

# llama-server writes to its console only unless told otherwise, so without this
# there is no artifact for anything — the admin panel, a post-mortem, or a human
# in another room — to read. Truncated on every start: llama-server does not
# rotate, and a verbose multi-hour pipeline run would grow this without bound.
$LogDir     = Join-Path $LlamaDir "logs"
$RouterLog  = Join-Path $LogDir "router.log"
$EmbedLog   = Join-Path $LogDir "embed.log"
New-Item -ItemType Directory -Path $LogDir -Force | Out-Null

# Dedicated embedding server (port 5002): the router cap counts models globally
# with no per-model exemption, so a cap of 1 (wanted: never two decode models in
# VRAM at once) would also evict the embed model. Embeds are CPU/RAM-only: -ngl 0
# alone is NOT enough on a CUDA build (op-offload streams large-batch matmuls to
# the GPU, allocating a multi-GB compute buffer at ubatch 4096), so --device none
# is required too. The embed GGUFs live in
# $ModelsDir\embed\ - a subfolder is invisible to router discovery (not recursive).
$EmbedPort  = 5002
$EmbedModel = Join-Path $ModelsDir "embed\Octen-Embedding-4B.Q8_0.gguf"

switch ($SubCommand) {
    "server" {
        if (-not (Get-NetTCPConnection -LocalPort $EmbedPort -State Listen -ErrorAction SilentlyContinue)) {
            Write-Host "Starting embed server on :$EmbedPort ($EmbedModel)..." -ForegroundColor Cyan
            Remove-Item $EmbedLog -ErrorAction SilentlyContinue
            # Hidden, not Minimized: this server runs for weeks and nobody ever
            # interacts with its console, but Minimized still costs a permanent
            # taskbar button. Everything it prints is in $EmbedLog, which the host
            # agent's console and /admin both read.
            Start-Process (Join-Path $LlamaDir "llama-server.exe") -WindowStyle Hidden -ArgumentList @(
                "-m", $EmbedModel,
                "--port", "$EmbedPort",
                "--embeddings", "--metrics",
                "--ctx-size", "4096", "--batch-size", "4096", "--ubatch-size", "4096",
                "-ngl", "0", "-fa", "on", "--device", "none",
                "--log-file", $EmbedLog, "--log-timestamps"
            )
        }
        Remove-Item $RouterLog -ErrorAction SilentlyContinue
        # All model-specific tuning comes from the preset file.
        $Arguments = @(
            "--port", "$Port",
            "--metrics",
            "--models-dir", $ModelsDir,
            "--models-preset", $PresetFile,
            "--models-max", "1",            # strictly one decode model in VRAM (fast/main
                                            # must never co-reside; embed lives on :5002)
            "--sleep-idle-seconds", "1800",
            "--log-file", $RouterLog, "--log-timestamps"
        )
    }
    "cli" {
        $Arguments = @(
            "-m", (Join-Path $ModelsDir "$Model.gguf"),
            "--color", "-cnv",
            "-fa", "on", "-ngl", "999", "--fit", "on", "-fitt", "768"
        )
    }
    "bench" {
        $Arguments = @(
            "-m", (Join-Path $ModelsDir "$Model.gguf"),
            "-fa", "on", "-ngl", "999"
        )
    }
}

if ($PassthroughArgs) { $Arguments += $PassthroughArgs }

$Executable = Join-Path $LlamaDir "llama-$SubCommand.exe"

if ($Detached) {
    # -Detached wins over -Foreground (which defaults to $true, so a caller
    # asking for detached would otherwise never get it).
    Write-Host "Starting $Executable detached; logging to $RouterLog" -ForegroundColor Cyan
    Start-Process $Executable -ArgumentList $Arguments -WindowStyle Hidden
    exit 0
}

if ($Foreground) {
    & $Executable @Arguments
    exit $LASTEXITCODE
}

# Launch pwsh directly rather than through wt.exe: wt re-parses its command
# line and mangles quoted/multi-token commands (0x80070002). On Windows 11 a
# new console opens in Windows Terminal anyway.
Write-Host "Starting $Executable in a new terminal window..." -ForegroundColor Cyan
Start-Process pwsh.exe -ArgumentList @("-NoExit", "-Command", "& $Executable $($Arguments -join ' ')")
