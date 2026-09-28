"""llama-warden: who gets the GPU, and who is told to let go of it.

One systemd service on the NixOS host. It measures the card through NVML
(`probe`), decides whether somebody else's work is at stake (`policy`), unloads
the llama.cpp router and frees an idle ComfyUI (`watch`), and tells its
consumers to pause or resume (`consumers`).

    nix develop -c python -m warden
"""

__all__ = ["__version__"]

__version__ = "0.2.0"
