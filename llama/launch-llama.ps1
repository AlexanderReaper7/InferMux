[CmdletBinding()]
param(
    [Parameter(Position=0, Mandatory=$false)]
    [ValidateSet("server", "cli", "bench")]
    [string]$subCommand = "server",

    # Captures all arguments launched with the script that aren't 'subCommand'
    [Parameter(ValueFromRemainingArguments=$true)]
    [string[]]$PassthroughArgs
)

# --- Variables ---
$ModelPath = "C:\Users\Alexander\models\Qwopus3.6-35B-A3B-Coder-MTP-Q4_K_M.gguf"
#$HfRepo = "Jackrong/Qwopus3.6-35B-A3B-Coder-MTP-GGUF" 

# --- Argument Definitions ---

# Arguments universally supported by server, cli, and bench
$CommonArgs = @(
    "-m", $ModelPath,
    #"-hfr", $HfRepo, 
    "-ctk", "q5_0", 
    "-ctv", "q4_1", 
    "-fitt", "768",                   # Safety margin: Leaves 768 MiB of VRAM entirely untouched for windows/desktop stability
    "--hf-token", "***REMOVED-HF-TOKEN***",
    "-fa", "on"                       # Explicitly turns on Flash Attention
    "-ngl", "999", # autofit?
    "--reasoning-preserve"
)

# Speculative Decoding (MTP) & Autofit Settings - ONLY supported by server and cli
$DeviceArgs = @(
    # Autofit Settings
    "--fit", "on",                    # Instructs engine to dynamically compute layer allocation for unset parameters
    #"--fit-ctx", "16384",             # Tells the autofitter to reserve enough VRAM headroom for a 16K context window
    "-c", "262144"

    # MTP Settings
    "--spec-type", "draft-mtp",       # Instructs llama.cpp to utilize internal MTP heads
    "--spec-draft-n-max", "3",        # Predicts 3 tokens ahead per step
    "--spec-draft-p-min", "0.75"       # Critical filter to prevent throughput collapse over long context
)

$ServerArgs = @(
    "--port", "5001",
    "--metrics",
    "--models-dir", "C:\selfhosting\models",
    "--models-max", "2",
    "--sleep-idle-seconds", "1800"
) + $DeviceArgs                       # Append MTP and Autofit settings to server

$CliArgs = @(
    "--color",
    "-cnv" # Launches in conversational mode
) + $DeviceArgs                       # Append MTP and Autofit settings to CLI

$BenchArgs = @(
    #"-p", "512,1024", # Prompt processing batch sizes to test
    #"-n", "128"       # Number of tokens to generate during test
    # Add any specific Bench arguments here
)

# --- Logic ---

# 1. Start with the common arguments
$Arguments = $CommonArgs

# 2. Append the specific arguments based on the chosen subcommand
switch ($subCommand) {
    "server" { $Arguments += $ServerArgs }
    "cli"    { $Arguments += $CliArgs }
    "bench"  { $Arguments += $BenchArgs }
}

# 3. Append any raw runtime arguments passed into the script
if ($PassthroughArgs) { $Arguments += $PassthroughArgs }

# 4. Determine the correct executable name
$LlamaDir = if ($env:LLAMA_CPP_DIR) { $env:LLAMA_CPP_DIR } else { "C:\selfhosting\llama-cpp" }
# The binaries and the run artifacts stay in the unpacked release; this script
# and the preset it reads are version-controlled in llama-warden.
$Executable = Join-Path $LlamaDir "llama-$subCommand.exe"

# --- Execution ---
Write-Host "Starting $Executable in Windows Terminal..." -ForegroundColor Cyan

# Flatten the arguments into a clean space-separated string
$ArgumentString = ($Arguments -join ' ')

# Launch via Windows Terminal (wt), calling pwsh with -NoExit to keep it alive
Start-Process wt.exe -ArgumentList "nt", "pwsh.exe", "-NoExit", "-Command", "& $Executable $ArgumentString"