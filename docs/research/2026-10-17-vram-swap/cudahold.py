"""cudahold: a stand-in for a CUDA model server's VRAM, through the driver API
by ctypes, so it needs no toolkit. `plain` allocates with cuMemAlloc, as
llama.cpp does by default; `managed` with cuMemAllocManaged, as llama.cpp does
under GGML_CUDA_ENABLE_UNIFIED_MEMORY=1, and prefetches it onto the GPU.

Once a second it prefetches the whole allocation onto the GPU again and
prints how long that took: near zero while every page is resident, about
size / PCIe bandwidth for the pages another allocation evicted.
Throwaway measurement tool for docs/research/2026-10-17-vram-swap.md.

usage: python3 cudahold.py plain|managed <mb> <hold_s>
"""

import ctypes
import sys
import time

cuda = ctypes.CDLL("/run/opengl-driver/lib/libcuda.so.1")


class CUmemLocation(ctypes.Structure):
    _fields_ = [("type", ctypes.c_int), ("id", ctypes.c_int)]


def check(name, *args):
    r = getattr(cuda, name)(*args)
    if r != 0:
        msg = ctypes.c_char_p()
        cuda.cuGetErrorString(r, ctypes.byref(msg))
        raise RuntimeError(f"{name}: {r} {msg.value.decode() if msg.value else ''}")


def free_mb():
    free, total = ctypes.c_size_t(), ctypes.c_size_t()
    check("cuMemGetInfo_v2", ctypes.byref(free), ctypes.byref(total))
    return free.value >> 20


def main():
    mode, mb, hold = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
    check("cuInit", 0)
    dev = ctypes.c_int()
    check("cuDeviceGet", ctypes.byref(dev), 0)
    ctx = ctypes.c_void_p()
    check("cuDevicePrimaryCtxRetain", ctypes.byref(ctx), dev)
    check("cuCtxSetCurrent", ctx)
    print(f"{time.monotonic():.3f} {mode} context up, free {free_mb()} MB", flush=True)

    size = mb << 20
    ptr = ctypes.c_uint64()
    t = time.monotonic()
    if mode == "plain":
        check("cuMemAlloc_v2", ctypes.byref(ptr), ctypes.c_size_t(size))
        check("cuMemsetD8_v2", ptr, ctypes.c_ubyte(1), ctypes.c_size_t(size))
    else:
        check("cuMemAllocManaged", ctypes.byref(ptr), ctypes.c_size_t(size), 1)  # CU_MEM_ATTACH_GLOBAL
    loc = CUmemLocation(1, 0)  # CU_MEM_LOCATION_TYPE_DEVICE, device 0

    def prefetch():
        t0 = time.monotonic()
        if mode == "managed":
            check("cuMemPrefetchAsync_v2", ptr, ctypes.c_size_t(size), loc, 0, None)
        check("cuCtxSynchronize")
        return time.monotonic() - t0

    if mode == "managed":
        check("cuMemsetD8_v2", ptr, ctypes.c_ubyte(1), ctypes.c_size_t(size))
    dt = prefetch()
    print(f"{time.monotonic():.3f} {mb} MB {mode} resident in {time.monotonic() - t:.2f} s, free {free_mb()} MB", flush=True)
    for s in range(hold):
        time.sleep(1)
        try:
            dt = prefetch()
            print(f"{time.monotonic():.3f} t={s + 1}s prefetch {dt * 1000:.0f} ms ({mb / 1024 / dt if dt > 0 else 0:.1f} GB/s), free {free_mb()} MB", flush=True)
        except RuntimeError as e:
            print(f"{time.monotonic():.3f} t={s + 1}s {e}", flush=True)
    check("cuMemFree_v2", ptr)


if __name__ == "__main__":
    main()
