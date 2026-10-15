# InferMux: llama-swap with the warden built in. Modelled on nixpkgs'
# llama-swap derivation, but built from this checkout, which carries
# llama-swap's history merged in (0004).
{
  lib,
  buildGoModule,
  buildNpmPackage,
  go_1_27,
  addDriverRunpath,
  makeWrapper,
  git,
  sops,
  age,
  gnupg,
  bun,
  # llama-swap's web dashboard. Its npm dependencies come from
  # registry.npmjs.org; without it InferMux serves the API only.
  withUI ? true,
}:

let
  version = "0.4.0";
  # The llama-swap release last merged in. Bump it with the merge.
  upstream = "262";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      # Every Go file by its extension, the root's included, so a new one
      # is never left out of the build and its tests.
      (lib.fileset.fileFilter (file: file.hasExt "go") ../.)
      ../config-schema.json
      ../config.example.yaml
      ../warden.example.yaml
      # The e2e runs' check of the card, which e2e/loadcheck_test.go runs.
      ../e2e/load-check.sh
      ../keys.example.yaml
      ../failover.example.yaml
      ../docs
      ../internal
      ../cmd
      ../ui
      ../webui
    ];
  };

  # InferMux's own web UI, embedded in infermux-ui (0005).
  webui = buildNpmPackage {
    pname = "infermux-webui";
    inherit version src;
    sourceRoot = "${src.name}/webui";
    npmDepsHash = "sha256-7XHIRNtcyjQClyco5WcQAqP2Y9I0kFNP2fegjKsiFwk=";
    # The editor's completion and the add-model commands (webui/test).
    doCheck = true;
    nativeCheckInputs = [ bun ];
    checkPhase = ''
      runHook preCheck
      bun test
      runHook postCheck
    '';
    installPhase = ''
      runHook preInstall
      cp -r dist $out
      runHook postInstall
    '';
  };

  ui = buildNpmPackage {
    pname = "llama-swap-ui";
    inherit version src;
    sourceRoot = "${src.name}/ui";
    npmDepsHash = "sha256-lmhRJ8275PIQ+7vHdr9aZ31lYeXUkXrWnlvuwOadjRQ=";
    postPatch = ''
      substituteInPlace vite.config.ts \
        --replace-fail "../internal/server/ui_dist" "${placeholder "out"}/ui_dist"
    '';
    # The bundle needs no node_modules.
    postInstall = "rm -rf $out/lib";
  };
in
(buildGoModule.override { go = go_1_27; }) {
  pname = "infermux";
  inherit version src;

  vendorHash = "sha256-DLvALZ22lKj4uaqiMc58RR8ENe0zctAnDUB9UKBq0Hg=";

  subPackages = [
    "."
    "cmd/infermux-ui"
    "cmd/infermux-adapter"
    "cmd/infermux-config"
    "cmd/infermux-cpu"
    "cmd/infermux-hold"
  ];
  tags = lib.optionals withUI [ "embed_ui" ];
  # go-nvml's header declares deprecated vGPU calls, one warning each.
  env.CGO_CFLAGS = "-Wno-deprecated-declarations";
  # go-nvml leaves the NVML symbols unresolved until it dlopens the driver.
  # Binding everything at load (-z now) fails before main.
  hardeningDisable = [ "bindnow" ];

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}+llama-swap.${upstream}"
    "-X main.commit=infermux"
  ];

  nativeBuildInputs = [ makeWrapper ];
  # The UI's tests commit to a scratch repository, sign one commit with a
  # scratch GPG key, and encrypt keys with sops.
  nativeCheckInputs = [
    git
    sops
    age
    gnupg
  ];

  preBuild = ''
    cp -r ${webui}/. internal/muxui/dist/
  ''
  + lib.optionalString withUI ''
    cp -r ${ui}/ui_dist internal/server/
  '';

  # The warden's and the UI's tests, the e2e runs' check of the card, and
  # llama-swap's for the two places InferMux touches. The rest of upstream's
  # suite starts processes and is upstream's to run, internal/process's
  # included: only our start gate's test runs there.
  checkPhase = ''
    runHook preCheck
    go test -count=1 ./internal/warden/ ./internal/remote/ ./internal/failover/ ./internal/catalog/ ./internal/muxui/ ./internal/adapter/ ./internal/stats/ ./internal/stream/... ./internal/placement/ ./internal/configstore/ ./internal/cpurun/ ./internal/fetch/ ./internal/jobs/ ./internal/hold/ ./internal/drain/ ./cmd/infermux-config/ ./e2e/ ./internal/server/ .
    go test -count=1 -run '^TestTheStartGate' ./internal/process/
    runHook postCheck
  '';

  # go-nvml dlopens libnvidia-ml.so.1 by name. The driver's libraries are under
  # /run/opengl-driver on NixOS, which is on no default search path.
  postInstall = ''
    mv $out/bin/llama-swap $out/bin/infermux
    wrapProgram $out/bin/infermux --prefix LD_LIBRARY_PATH : ${addDriverRunpath.driverLink}/lib
  '';

  passthru = { inherit ui webui upstream; };

  meta = {
    description = "llama-swap with a GPU warden: model routing that yields the card to other work and never kills the user's own prompt";
    homepage = "https://github.com/AlexanderReaper7/InferMux";
    # InferMux's own code is AGPL-3.0-only; the llama-swap code it merges is MIT.
    license = with lib.licenses; [
      agpl3Only
      mit
    ];
    mainProgram = "infermux";
    platforms = lib.platforms.linux;
  };
}
