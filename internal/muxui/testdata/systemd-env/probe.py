"""Records how this host's systemd reads each environment file in a generated
list, through systemd's own load_env_file, the parser behind a unit's
EnvironmentFile. TestReadEnvFileReadsAsSystemdDoes compares ReadEnvFile with
the record (sol, review 15, M3). Run it again after a systemd upgrade:

    uv run --no-project python internal/muxui/testdata/systemd-env/probe.py

It writes cases.json beside it. The library is found under
/run/current-system unless its path is the one argument."""

import ctypes
import glob
import json
import pathlib
import subprocess
import sys
import tempfile

here = pathlib.Path(__file__).parent
lib_path = sys.argv[1] if len(sys.argv) > 1 else glob.glob("/run/current-system/systemd/lib/systemd/libsystemd-shared-*.so")[0]
lib = ctypes.CDLL(lib_path)
lib.load_env_file.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.POINTER(ctypes.POINTER(ctypes.c_char_p))]
lib.load_env_file.restype = ctypes.c_int
lib.strv_free.argtypes = [ctypes.POINTER(ctypes.c_char_p)]

cases = []
for b in range(256):
    c = bytes([b])
    cases += [b"A=x" + c + b"y\n", b"A=" + c + b"\n", c + b"A=x\n", b"#" + c + b"\nA=x\n", b"A" + c + b"=x\n"]
cases += [
    b"A=one\rB=two\n",
    b"A=one\r\n",
    b"#c\rA=x\n",
    b"A=x\n;c\rB=y\n",
    b"#c\\\nA=x\n",
    b";c\\\nA=x\n",
    b"#c\\\\\nA=x\n",
    b"A=one\\\nB=two\n",
    b'A="q"\n',
    b"A='q'\n",
    b'A=a"b"c\n',
    b"A=a'b'c\n",
    b"A=a b\n",
    b"A=a  b\n",
    b"A=x \n",
    b"A= x\n",
    b" A=x\n",
    b"A =x\n",
    b"export A=x\n",
    b"A=one\nA=two\n",
    b"A=b=c\n",
    b"A=\n",
    b"A\n",
    b"=x\n",
    b"A=x",
    b"\n\nA=x\n\n",
    b"A=x\n;c\nB=y\n",
    b"A=x#y\n",
    b"A=x #y\n",
    b"A=x;y\n",
    b"A=$B\n",
    b"A=${B}\n",
    b"B=1\nA=${B}\n",
    b"A=sk-or-v1-0123456789abcdef\n",
    b"A=x\xc3\xa9y\n",
    b"A=x\xc2\xa0y\n",
    b"A=x\xe2\x80\xa8y\n",
    b"A=hello\xef\xbb\xbfworld\n",
    b"\xef\xbb\xbfA=x\n",
    b"A=hello\x00\nB=second\n",
    b"A=x\n\x00\n",
    b"A_1=x\nB2=y\n_C=z\n",
    b"A.B=x\n",
    b"A-B=x\n",
]

out = []
with tempfile.TemporaryDirectory() as tmp:
    path = pathlib.Path(tmp) / "case"
    for case in cases:
        path.write_bytes(case)
        got = ctypes.POINTER(ctypes.c_char_p)()
        rc = lib.load_env_file(None, str(path).encode(), ctypes.byref(got))
        values = []
        if got:
            i = 0
            while got[i] is not None:
                values.append(got[i].hex())
                i += 1
            lib.strv_free(got)
        out.append({"file": case.hex(), "rc": rc, "vars": values})

version = subprocess.run(["systemctl", "--version"], capture_output=True, text=True).stdout.splitlines()[0]
(here / "cases.json").write_text(json.dumps({"systemd": version, "cases": out}, indent=0) + "\n")
print(version, len(out), "cases")
