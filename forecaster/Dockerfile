# Forecaster image.
#
# The CPU torch wheel is installed explicitly: the default index resolves to a
# CUDA build that is several gigabytes larger and useless on a CPU-only node.
# Override PRESAGE_TORCH_INDEX to build a GPU image.
FROM python:3.14-slim AS build

ARG PRESAGE_TORCH_INDEX=https://download.pytorch.org/whl/cpu

ENV PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /src
RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

RUN pip install --index-url "${PRESAGE_TORCH_INDEX}" torch

COPY pyproject.toml README.md ./
COPY presage_forecaster/ presage_forecaster/
RUN pip install '.[torch]'

FROM python:3.14-slim
ENV PATH="/opt/venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    HF_HOME=/cache/huggingface
COPY --from=build /opt/venv /opt/venv

# The checkpoint is pulled at startup rather than baked in, so the image stays
# small and the model can be changed without a rebuild. Mount /cache on a
# volume to avoid re-downloading on every restart.
RUN mkdir -p /cache && chown 65532:65532 /cache
VOLUME /cache
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["presage-forecaster"]
