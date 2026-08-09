"""Run the PyTorch checkpoint on fixed inputs and print the logits.

This exists to settle one question: when the Go side scores a real face at
0.006 for the live class, is that the model's own judgement or is it something
this project does to the model on the way in?

The inputs are built from an index formula rather than from an image, so the Go
side can construct byte-identical tensors without a shared file. If both sides
print the same logits, the export and the preprocessing are faithful and the
score is the model's own answer. If they differ, the fault is on this side.

    docker compose --profile setup run --rm antispoof-convert python /work/probe.py
"""

import os
import sys

SFAS_ROOT = "/opt/sfas"
sys.path.insert(0, SFAS_ROOT)

import torch  # noqa: E402

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from convert import DEFAULT_CHECKPOINT, load_model  # noqa: E402


def fixed_tensor(kind, size=80):
    """Build a deterministic input the Go side can reproduce exactly."""
    t = torch.zeros(1, 3, size, size)
    for c in range(3):
        for y in range(size):
            for x in range(size):
                if kind == "zeros":
                    v = 0.0
                elif kind == "ones":
                    v = 1.0
                elif kind == "half":
                    v = 0.5
                elif kind == "ramp":
                    v = ((c * size * size + y * size + x) % 255) / 255.0
                else:
                    raise SystemExit(f"unknown input {kind!r}")
                t[0, c, y, x] = v
    return t


def main():
    checkpoint = os.path.join(SFAS_ROOT, "resources", "anti_spoof_models", DEFAULT_CHECKPOINT)
    model, height, width, scale = load_model(checkpoint)
    print(f"checkpoint {DEFAULT_CHECKPOINT}  input {width}x{height}  crop scale {scale}")

    for kind in ("zeros", "half", "ones", "ramp"):
        with torch.no_grad():
            logits = model(fixed_tensor(kind, height))
            probs = torch.softmax(logits, dim=1)
        raw = [f"{v: .4f}" for v in logits[0].tolist()]
        p = [f"{v:.4f}" for v in probs[0].tolist()]
        print(f"{kind:6s} logits [{' '.join(raw)}]  probs [{' '.join(p)}]")


if __name__ == "__main__":
    main()
