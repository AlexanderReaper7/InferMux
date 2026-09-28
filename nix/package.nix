# The version comes from pyproject.toml, so the two cannot disagree.
{
  lib,
  addDriverRunpath,
  buildPythonApplication,
  hatchling,
  fastapi,
  uvicorn,
  nvidia-ml-py,
  pytestCheckHook,
}:

let
  manifest = (lib.importTOML ../pyproject.toml).project;
in
buildPythonApplication {
  pname = manifest.name;
  inherit (manifest) version;
  pyproject = true;

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../pyproject.toml
      ../src
      ../tests
    ];
  };

  build-system = [ hatchling ];
  dependencies = [
    fastapi
    uvicorn
    nvidia-ml-py
  ];

  # pynvml dlopens libnvidia-ml.so.1 by name. The driver's libraries are under
  # /run/opengl-driver on NixOS, which is on no default search path.
  makeWrapperArgs = [ "--prefix LD_LIBRARY_PATH : ${addDriverRunpath.driverLink}/lib" ];

  nativeCheckInputs = [ pytestCheckHook ];
  pythonImportsCheck = [ "warden" ];

  meta = {
    description = manifest.description;
    homepage = "https://github.com/AlexanderReaper7/llama-warden";
    mainProgram = "warden";
    platforms = lib.platforms.linux;
  };
}
