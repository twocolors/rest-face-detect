#!/bin/bash
set -Eeuo pipefail

MODEL_PATH="${MODEL_PATH:-/models}"
MODEL="${MODEL:-buffalo_sc}"

case "$MODEL" in
  ""|*[!A-Za-z0-9_-]*)
    echo "ERROR: invalid MODEL value: '$MODEL'" >&2
    exit 2
    ;;
esac

MODEL_FILE="${MODEL_PATH}/${MODEL}.gguf"
MODEL_URL="https://huggingface.co/mudler/face-detect-gguf/resolve/main/${MODEL}.gguf"

mkdir -p "${MODEL_PATH}"

if [ ! -s "${MODEL_FILE}" ]; then
  echo "INFO: downloading model '${MODEL}'..."
  TMP_FILE="$(mktemp "${MODEL_FILE}.tmp.XXXXXX")"

  curl -fsSL --retry 5 --retry-all-errors --connect-timeout 10 \
    -o "${TMP_FILE}" "${MODEL_URL}"

  mv -f "${TMP_FILE}" "${MODEL_FILE}"
  echo "INFO: model downloaded successfully"
else
  echo "INFO: model '${MODEL}' already exists"
fi

echo "INFO: starting rest-face-detect service"
exec /usr/local/bin/rest-face-detect