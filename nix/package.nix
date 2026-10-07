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
      ../llama-swap.go
      ../llama-swap_test.go
      ../infermux.go
      ../infermux_stream_test.go
      ../config-schema.json
      ../config.example.yaml
      ../warden.example.yaml
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
    npmDepsHash = "sha256-6oDnOHJhVUXcnEpm0kyGr+LkRvByEVPXMoNgAJ2KP5I=";
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

  # The warden's and the UI's tests, and llama-swap's for the places InferMux
  # touches: the server, the router, and in the process only ours, since the
  # rest of its suite starts processes and is upstream's to run.
  checkPhase = ''
    runHook preCheck
    go test -count=1 ./internal/warden/ ./internal/remote/ ./internal/failover/ ./internal/catalog/ ./internal/muxui/ ./internal/adapter/ ./internal/stats/ ./internal/stream/... ./internal/server/ ./internal/router/... .
    go test -count=1 -run 'Session' ./internal/process/
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
    license = lib.licenses.mit;
    mainProgram = "infermux";
    platforms = lib.platforms.linux;
  };
}
