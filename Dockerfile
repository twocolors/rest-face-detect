FROM debian:bookworm AS native-build

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates git cmake build-essential \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN git clone --recursive --depth 1 https://github.com/mudler/face-detect.cpp.git
WORKDIR /src/face-detect.cpp
RUN cmake -B build \
      -DCMAKE_BUILD_TYPE=Release \
      -DFACEDETECT_SHARED=ON \
      -DFACEDETECT_BUILD_CLI=OFF \
      -DFACEDETECT_BUILD_TESTS=OFF \
      -DBUILD_SHARED_LIBS=OFF  \
      -DCMAKE_POSITION_INDEPENDENT_CODE=ON \
      -DGGML_NATIVE=OFF \
    && cmake --build build -j"$(nproc)"
RUN mkdir -p /install/lib \
    && find build -name '*.so*' -exec cp -av {} /install/lib/ \;

FROM golang:1.27-bookworm AS go-build

RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc \
    g++ \
    libsqlite3-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY . .

RUN CGO_ENABLED=1 go build \
  -trimpath \
  -ldflags="-s -w" \
  -o /out/rest-face-detect \
  ./cmd

FROM debian:bookworm-slim

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    ca-certificates curl libstdc++6 libsqlite3-0 libgomp1 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=native-build /install/lib/ /usr/local/lib/
COPY --from=go-build /out/rest-face-detect /usr/local/bin/rest-face-detect
COPY entrypoint.sh /entrypoint.sh

RUN chmod 0755 /entrypoint.sh \
    && ldconfig

ENV DATA_PATH=/data \
    MODEL_PATH=/models \
    MODEL=buffalo_sc \
    FACEDETECT_LIB=/usr/local/lib/libfacedetect.so \
    ADDR=:8000 \
    DEBUG=false \
    UI=false \
    IMAGE_SIZE=10

VOLUME ["/data", "/models"]
EXPOSE 8000

ENTRYPOINT ["/entrypoint.sh"]
