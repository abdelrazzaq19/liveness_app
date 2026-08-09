# Converts the MiniFASNet anti-spoofing checkpoint to ONNX.
#
# This image exists because upstream ships PyTorch weights and no ONNX export,
# and because reproducing a model architecture from memory is exactly the kind
# of guess that produces plausible numbers and wrong answers. The upstream
# repository is cloned and its own model definition and its own weights are
# used; nothing here reimplements them.
#
# It runs once, at setup. The service itself never needs Python.

FROM python:3.11-slim

ARG SFAS_REPO=https://github.com/minivision-ai/Silent-Face-Anti-Spoofing.git
ARG SFAS_REF=master

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# CPU-only wheels: this converts a model, it does not train one, and the CUDA
# build is several gigabytes larger for no benefit.
RUN pip install --no-cache-dir \
      --index-url https://download.pytorch.org/whl/cpu \
      torch==2.5.1 \
 && pip install --no-cache-dir onnx==1.17.0

RUN git clone --depth 1 --branch "${SFAS_REF}" "${SFAS_REPO}" /opt/sfas \
 && git -C /opt/sfas rev-parse HEAD > /opt/sfas-commit.txt \
 && cat /opt/sfas-commit.txt

COPY tools/antispoof/convert.py /opt/convert.py
# probe.py answers "is the score the model's own judgement, or something we do
# to it on the way in" by running the checkpoint on inputs the Go side can
# rebuild byte for byte.
COPY tools/antispoof/probe.py /opt/probe.py

WORKDIR /out
ENTRYPOINT ["python", "/opt/convert.py"]
