"""llama-warden: who gets the GPU, and who is told to let go of it.

One process on the Windows host. It runs llama.cpp's servers (`agent`), watches
what else is using the card (`watch`), decides whether that other work is at
stake (`policy`), and tells its consumers to pause or resume (`consumers`).

    uv run python -m warden
"""

__all__ = ["__version__"]

__version__ = "0.1.0"
