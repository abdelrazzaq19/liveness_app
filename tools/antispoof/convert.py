"""Export the MiniFASNet anti-spoofing checkpoint to ONNX.

Upstream (Silent-Face-Anti-Spoofing) ships PyTorch weights and no ONNX build.
This script loads them with upstream's own model definition and exports the
graph, so that nothing here is a reimplementation that could quietly disagree
with the weights it is loading.

Run through docker compose:

    docker compose --profile setup run --rm antispoof-convert

The output lands in models/ and is then pinned like any other artifact:

    docker compose --profile setup run --rm modelctl pin
"""

import argparse
import hashlib
import os
import sys
from collections import OrderedDict

SFAS_ROOT = "/opt/sfas"
sys.path.insert(0, SFAS_ROOT)

import torch  # noqa: E402  (must follow the sys.path insert)

from src.model_lib.MiniFASNet import (  # noqa: E402
    MiniFASNetV1,
    MiniFASNetV1SE,
    MiniFASNetV2,
    MiniFASNetV2SE,
)
from src.utility import get_kernel, parse_model_name  # noqa: E402

MODELS = {
    "MiniFASNetV1": MiniFASNetV1,
    "MiniFASNetV1SE": MiniFASNetV1SE,
    "MiniFASNetV2": MiniFASNetV2,
    "MiniFASNetV2SE": MiniFASNetV2SE,
}

DEFAULT_CHECKPOINT = "2.7_80x80_MiniFASNetV2.pth"


def load_model(checkpoint_path):
    """Build the network upstream's way and load the weights into it."""
    name = os.path.basename(checkpoint_path)
    height, width, model_type, scale = parse_model_name(name)

    if model_type not in MODELS:
        raise SystemExit(f"unknown model type {model_type!r} in {name!r}")

    model = MODELS[model_type](conv6_kernel=get_kernel(height, width))

    state = torch.load(checkpoint_path, map_location="cpu", weights_only=True)

    # Checkpoints saved from DataParallel carry a "module." prefix on every key.
    first = next(iter(state))
    if first.startswith("module."):
        state = OrderedDict((k[len("module."):], v) for k, v in state.items())

    model.load_state_dict(state)
    model.eval()
    return model, height, width, scale


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--checkpoint", default=DEFAULT_CHECKPOINT,
                        help="checkpoint file name inside the upstream resources directory")
    parser.add_argument("--out", default="/out/minifasnet_v2.onnx",
                        help="where to write the ONNX graph")
    parser.add_argument("--opset", type=int, default=13)
    args = parser.parse_args()

    checkpoint = os.path.join(SFAS_ROOT, "resources", "anti_spoof_models", args.checkpoint)
    if not os.path.exists(checkpoint):
        available = sorted(os.listdir(os.path.dirname(checkpoint)))
        raise SystemExit(f"{checkpoint} not found; available: {available}")

    model, height, width, scale = load_model(checkpoint)

    # The name encodes how much wider than the detected face box the crop must
    # be. It is part of the contract with the Go side, so it is printed rather
    # than left for someone to rediscover.
    print(f"checkpoint  {args.checkpoint}")
    print(f"input       {width}x{height}")
    print(f"crop scale  {scale}")

    with open("/opt/sfas-commit.txt") as handle:
        print(f"upstream    {handle.read().strip()}")

    dummy = torch.randn(1, 3, height, width)

    with torch.no_grad():
        logits = model(dummy)
    print(f"output      {tuple(logits.shape)} logits")

    torch.onnx.export(
        model,
        dummy,
        args.out,
        input_names=["input"],
        output_names=["logits"],
        opset_version=args.opset,
        dynamic_axes={"input": {0: "batch"}, "logits": {0: "batch"}},
        do_constant_folding=True,
    )

    print(f"written     {args.out}  sha256={sha256(args.out)}")
    print()
    print("Next: docker compose --profile setup run --rm modelctl pin")


if __name__ == "__main__":
    main()
