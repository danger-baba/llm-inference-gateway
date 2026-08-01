# Since Phase 7, the gateway links onnxruntime via cgo for local
# embedding inference, so the build stage needs a real C toolchain and a
# glibc-based runtime image (a prebuilt onnxruntime shared library isn't
# guaranteed compatible with musl/Alpine) — hence bookworm, not alpine,
# on both stages now.
FROM golang:1.24-bookworm AS build
WORKDIR /src
RUN apt-get update && apt-get install -y --no-install-recommends gcc ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# Fetch the Linux ONNX Runtime release and the all-MiniLM-L6-v2 embedding
# assets at build time, so the final image is self-contained and doesn't
# need network access to start. See docs/adr/0012.
ARG ONNXRUNTIME_VERSION=1.28.0
RUN curl -sL -o /tmp/ort.tgz "https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/onnxruntime-linux-x64-${ONNXRUNTIME_VERSION}.tgz" \
    && mkdir -p /assets/onnxruntime \
    && tar -xzf /tmp/ort.tgz -C /assets/onnxruntime --strip-components=1 \
    && rm /tmp/ort.tgz
RUN mkdir -p /assets/minilm \
    && curl -sL -o /assets/minilm/model.onnx "https://huggingface.co/Xenova/all-MiniLM-L6-v2/resolve/main/onnx/model_quantized.onnx" \
    && curl -sL -o /assets/minilm/vocab.txt "https://huggingface.co/Xenova/all-MiniLM-L6-v2/resolve/main/vocab.txt"

COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o /out/gateway ./cmd/gateway

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/gateway /usr/local/bin/gateway
COPY --from=build /assets /assets
ENV ONNXRUNTIME_LIB_PATH=/assets/onnxruntime/lib/libonnxruntime.so
ENV EMBEDDING_MODEL_PATH=/assets/minilm/model.onnx
ENV EMBEDDING_VOCAB_PATH=/assets/minilm/vocab.txt
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gateway"]
