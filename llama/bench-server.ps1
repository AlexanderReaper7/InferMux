<#
bench-server.ps1 - server-side tok/s benchmark for the models on the router.

Usage:
  ./bench-server.ps1                                   # every chat model in models-preset.ini
  ./bench-server.ps1 -Models Qwen3.8-27B-MTP-Q4_K_M    # just these
  ./bench-server.ps1 -Reps 5 -Predict 512
  ./bench-server.ps1 -Scenario longctx                 # real 18.7k-token writer call
  ./bench-server.ps1 -Scenario all

Two scenarios:

  quick    Short prompt, 256 tokens out, plus a 4k filler prompt for prefill.
           Synthetic, but fast and stable - use it to compare models or builds.

  longctx  Replays an ACTUAL episteme writer call: the largest write/main call
           on record (llm_calls chain 478a66c9, story 988 - 19756 prompt + 1839
           completion tokens), rebuilt from the stored per-call deltas into a
           tool-free conversation of 18722 tokens. This is what the `main` role
           really does at its heaviest, so its numbers are the ones that predict
           nightly pipeline wall-clock. Slow by construction: prefill alone is
           ~18.7k tokens, and a slow model can take ~9 minutes per repetition.

Tools are deliberately absent from the longctx fixture. The research text stays
verbatim (that is what makes the context big), but the tool-call plumbing is
stripped, so the run measures the model rather than fetch latency or SearXNG.

Requires the router to be up (./launch-llama-v2.ps1 server). Each model is
loaded on demand; with --models-max 1 that evicts the previous one, so the wall
time is dominated by loads, not by decode.

Why not llama-bench: llama-bench drives llama_decode directly and never runs
speculative decoding, so it cannot see what MTP buys - which is most of the
point on these presets. This goes through /completion, the same path episteme
uses, and reads the server's own timings block.

Live output is transcripted to bench\live.log, so a run in another window or a
detached one is still watchable:  Get-Content -Wait llama-cpp\bench\live.log

Results append to bench\<timestamp>.json together with the llama.cpp build, the
free VRAM at start and the effective ctx-size. Without those a saved number is
uninterpretable later: the same model on the same box benches differently with a
browser open, and models-preset.ini warns that arg names drift between builds.
#>
[CmdletBinding()]
param(
    # Model names as served by the router. Default: the chat models, largest first.
    [string[]]$Models,

    [ValidateSet("quick", "longctx", "all")]
    [string]$Scenario = "quick",

    [int]$Reps    = 3,      # quick: timed generation runs per model (run 0 discarded as warmup)
    [int]$Predict = 256,    # quick: tokens to generate per run
    [int]$Port    = 5001,
    [switch]$NoSave,

    # longctx runs ONCE per model. Measured 2026-08-15: one repetition is ~7.5 min
    # on the 35B and ~24 min on the CPU-bound dense 27B, because prefill at 18.7k
    # runs 3-4x slower than the short-prompt figure suggests (54 tok/s vs 192).
    # Repetitions were also tight - 15.95 then 18.29 tok/s on the 35B - so the
    # second one bought a rounding difference for 7 minutes. Raise it deliberately
    # if you ever need error bars, but the default should not cost half an hour.
    [string]$Fixture     = "bench\fixtures\writer-longctx.json",
    [int]$LongReps       = 1,
    [int]$LongPredict    = 2048
)

$Base    = "http://127.0.0.1:$Port"
$LlamaDir = if ($env:LLAMA_CPP_DIR) { $env:LLAMA_CPP_DIR } else { "C:\selfhosting\llama-cpp" }
# The binaries and the run artifacts stay in the unpacked release; this script
# and the preset it reads are version-controlled in llama-warden.
$BenchDir = Join-Path $LlamaDir "bench"
New-Item -ItemType Directory -Path $BenchDir -Force | Out-Null

# Everything below is transcripted, so a run launched in its own window (or
# detached) is still watchable: Get-Content -Wait bench\live.log
$LiveLog = Join-Path $BenchDir "live.log"
Start-Transcript -Path $LiveLog -Force | Out-Null
trap { Stop-Transcript | Out-Null; break }

if (-not (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue)) {
    Stop-Transcript | Out-Null
    Write-Error "No router listening on :$Port - start it with ./launch-llama-v2.ps1 server"
    return
}

$Available = (Invoke-RestMethod "$Base/v1/models").data.id

if (-not $Models) {
    # Chat models only: the embedder and the VL model are not comparable workloads.
    $Models = $Available | Where-Object { $_ -notmatch '^embed$' -and $_ -notmatch 'Embedding' -and $_ -notmatch 'VL' }
}

# `pwsh -File bench.ps1 -Models a,b` hands the whole list over as ONE string -
# -File does no PowerShell parsing of its arguments. Split here so both calling
# conventions work; without this the run dies ~25s into a bogus model load.
$Models = $Models -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ }

$unknown = $Models | Where-Object { $_ -notin $Available }
if ($unknown) {
    Stop-Transcript | Out-Null
    Write-Error "Unknown model(s): $($unknown -join ', ')`nRouter serves:`n  $($Available -join "`n  ")"
    return
}

# ~4k tokens of filler. Prompt processing is compute-bound where generation is
# bandwidth-bound, so the two numbers move independently and both are worth having.
$para = "The router keeps exactly one decode model resident in VRAM at a time, evicting the previous one when a request names a different model. Speculative decoding drafts several tokens ahead and verifies them in a single forward pass. "
$LongPrompt = ($para * 90)
$TgPrompt   = "Write a detailed technical explanation of how paged attention works in an LLM inference server."

function Invoke-Completion($model, $prompt, $n) {
    $body = @{
        model        = $model
        prompt       = $prompt
        n_predict    = $n
        temperature  = 0      # greedy: reproducible, and the same draft-acceptance policy for every model
        cache_prompt = $false # otherwise run 2 reuses run 1's KV and prompt_per_second is meaningless
    } | ConvertTo-Json -Compress
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $r  = Invoke-RestMethod -Uri "$Base/completion" -Method Post -Body $body -ContentType "application/json" -TimeoutSec 3600
    $sw.Stop()
    [pscustomobject]@{ Wall = $sw.Elapsed.TotalSeconds; T = $r.timings }
}

# /v1/chat/completions rather than /completion: the fixture is a message list, and
# llama-server returns the same `timings` block on both. Prefill and generation are
# reported separately, which is the whole point here - a writer call spends most of
# its wall clock on the ~18.7k-token prefill, not on the draft.
function Invoke-Chat($model, $messages, $n) {
    $body = @{
        model        = $model
        messages     = $messages
        max_tokens   = $n
        temperature  = 0
        cache_prompt = $false   # every real writer call is a different story: prefill is always cold
    } | ConvertTo-Json -Compress -Depth 6
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $r  = Invoke-RestMethod -Uri "$Base/v1/chat/completions" -Method Post -Body $body -ContentType "application/json" -TimeoutSec 7200
    $sw.Stop()
    [pscustomobject]@{ Wall = $sw.Elapsed.TotalSeconds; T = $r.timings }
}

$FixturePath = if ([IO.Path]::IsPathRooted($Fixture)) { $Fixture } else { Join-Path $PSScriptRoot $Fixture }
$LongMessages = $null
if ($Scenario -in 'longctx', 'all') {
    if (-not (Test-Path $FixturePath)) {
        Stop-Transcript | Out-Null
        Write-Error "Fixture not found: $FixturePath"
        return
    }
    $LongMessages = (Get-Content $FixturePath -Raw | ConvertFrom-Json).messages
}

$env0 = [pscustomobject]@{
    Timestamp   = (Get-Date).ToString("o")
    LlamaBuild  = (& (Join-Path $LlamaDir "llama-server.exe") --version 2>&1 | Select-String 'version:' | ForEach-Object { $_.Line.Trim() })
    GpuFreeMiB  = [int]((nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits) | Select-Object -First 1)
    Scenario    = $Scenario
    Reps        = $Reps
    Predict     = $Predict
    Fixture     = if ($LongMessages) { Split-Path $FixturePath -Leaf } else { $null }
}
Write-Host "$($env0.LlamaBuild) | $($env0.GpuFreeMiB) MiB VRAM free" -ForegroundColor DarkGray

$rows = @()
foreach ($m in $Models) {
    Write-Host "`n=== $m" -ForegroundColor Cyan

    # The first call pays the model load (weights off disk, then the --fit pass).
    # Timed separately so it never contaminates tok/s.
    Write-Host "  loading + warmup..." -NoNewline
    $warm = Invoke-Completion $m "Hello." 8
    Write-Host (" {0:N1}s wall" -f $warm.Wall)

    # Free VRAM with THIS model resident. Environment.GpuFreeMiB is sampled at
    # script start, which is whatever the router happened to hold then - it read
    # 339 MiB on 2026-08-15 because the previous run's model was still cached, and
    # a load time of 0.5s means the "load" was a no-op. Per-model is the honest one.
    $freeMiB = [int]((nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits) | Select-Object -First 1)
    Write-Host ("  resident: {0} MiB VRAM free" -f $freeMiB) -ForegroundColor DarkGray

  if ($Scenario -in 'quick', 'all') {
    # Run 0 is discarded. The 8-token warmup above pays the model load, but the
    # first full-length generation still runs ~30% slow (26.9 vs 39-40 tok/s on
    # the 35B, 2026-08-15), so averaging it in understates steady state.
    $tg = @(); $accept = @()
    for ($i = 0; $i -le $Reps; $i++) {
        $r = Invoke-Completion $m $TgPrompt $Predict
        if ($i -eq 0) {
            Write-Host ("  tg warmup: {0,6:N2} tok/s (discarded)" -f $r.T.predicted_per_second)
            continue
        }
        $tg += $r.T.predicted_per_second
        # draft_n / draft_n_accepted appear only when speculative decoding actually ran.
        if ($r.T.PSObject.Properties.Name -contains 'draft_n' -and $r.T.draft_n -gt 0) {
            $accept += 100.0 * $r.T.draft_n_accepted / $r.T.draft_n
        }
        Write-Host ("  tg run {0}: {1,6:N2} tok/s  ({2} tokens)" -f $i, $r.T.predicted_per_second, $r.T.predicted_n)
    }

    $pp = @()
    for ($i = 1; $i -le 2; $i++) {
        $r = Invoke-Completion $m $LongPrompt 8
        $pp += $r.T.prompt_per_second
        Write-Host ("  pp run {0}: {1,6:N1} tok/s  ({2} prompt tokens)" -f $i, $r.T.prompt_per_second, $r.T.prompt_n)
    }

    $rows += [pscustomobject]@{
        Scenario  = 'quick'
        Model     = $m
        LoadSec   = [math]::Round($warm.Wall, 1)
        FreeMiB   = $freeMiB
        PromptTok = 4051
        TG        = [math]::Round(($tg | Measure-Object -Average).Average, 2)
        TGmax     = [math]::Round(($tg | Measure-Object -Maximum).Maximum, 2)
        PP        = [math]::Round(($pp | Measure-Object -Average).Average, 1)
        WallSec   = $null
        AcceptPct = if ($accept.Count) { [math]::Round(($accept | Measure-Object -Average).Average, 1) } else { $null }
    }
  }

  if ($Scenario -in 'longctx', 'all') {
    Write-Host "  -- longctx: replaying the real writer call --" -ForegroundColor DarkGray
    $ltg = @(); $lpp = @(); $lwall = @(); $lacc = @(); $ptok = 0
    for ($i = 1; $i -le $LongReps; $i++) {
        $r = Invoke-Chat $m $LongMessages $LongPredict
        $ltg   += $r.T.predicted_per_second
        $lpp   += $r.T.prompt_per_second
        $lwall += $r.Wall
        $ptok   = $r.T.prompt_n
        if ($r.T.PSObject.Properties.Name -contains 'draft_n' -and $r.T.draft_n -gt 0) {
            $lacc += 100.0 * $r.T.draft_n_accepted / $r.T.draft_n
        }
        Write-Host ("  longctx run {0}: prefill {1,7:N1} tok/s ({2} tok) | gen {3,6:N2} tok/s ({4} tok) | {5,6:N1}s wall" -f `
            $i, $r.T.prompt_per_second, $r.T.prompt_n, $r.T.predicted_per_second, $r.T.predicted_n, $r.Wall)
    }
    $rows += [pscustomobject]@{
        Scenario  = 'longctx'
        Model     = $m
        LoadSec   = [math]::Round($warm.Wall, 1)
        FreeMiB   = $freeMiB
        PromptTok = $ptok
        TG        = [math]::Round(($ltg | Measure-Object -Average).Average, 2)
        TGmax     = [math]::Round(($ltg | Measure-Object -Maximum).Maximum, 2)
        PP        = [math]::Round(($lpp | Measure-Object -Average).Average, 1)
        WallSec   = [math]::Round(($lwall | Measure-Object -Average).Average, 1)
        AcceptPct = if ($lacc.Count) { [math]::Round(($lacc | Measure-Object -Average).Average, 1) } else { $null }
    }
  }
}

"`n"
$rows | Format-Table -AutoSize
Write-Host "TG/PP = generation / prompt-processing tok/s. WallSec = one full call end to end." -ForegroundColor DarkGray
Write-Host "AcceptPct = MTP draft tokens accepted." -ForegroundColor DarkGray

if (-not $NoSave) {
    $out = Join-Path $BenchDir ("{0:yyyy-MM-dd_HHmm}.json" -f (Get-Date))
    [pscustomobject]@{ Environment = $env0; Results = $rows } | ConvertTo-Json -Depth 4 | Set-Content $out -Encoding utf8
    Write-Host "Saved $out" -ForegroundColor Green
}

Stop-Transcript | Out-Null
