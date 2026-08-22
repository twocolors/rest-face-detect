# rest-face-detect

A small Go REST wrapper around the unchanged native C API from
[`mudler/face-detect.cpp`](https://github.com/mudler/face-detect.cpp) thx @mudler
It provides face detection, analysis, registration and identification, with SQLite used for stored embeddings. Written with love and Copilot, ChatGPT for [`alexxit/go2rtc`](https://github.com/alexxit/go2rtc) and [`Node-RED`](https://github.com/node-red/node-red)

## Environment

| Variable | Default | Description |
| --- | --- | --- |
| `DATA_PATH` | `/data` | SQLite database and persistent data directory |
| `MODEL_PATH` | `/models` | Directory where `${MODEL}.gguf` is downloaded and loaded |
| `MODEL` | `buffalo_sc` | GGUF model name, without the `.gguf` suffix |
| `FACEDETECT_LIB` | `/usr/local/lib/libfacedetect.so` | Native shared library path |
| `ADDR` | `:8000` | HTTP listen address |
| `DEBUG` | `false` | Log request details and internal error details when set to `true` |
| `UI` | `false` | Serve the optional web UI at `/` when set to `true` |
| `IMAGE_SIZE` | `10` | Maximum request image size in MB for image endpoints |

The entrypoint downloads `${MODEL}.gguf` from the
[`mudler/face-detect-gguf`](https://huggingface.co/mudler/face-detect-gguf)
collection on Hugging Face into `${MODEL_PATH}`. The exact download URL is:

```text
https://huggingface.co/mudler/face-detect-gguf/resolve/main/${MODEL}.gguf
```

Available models in the collection:

```text
buffalo_l
buffalo_m
buffalo_s
buffalo_sc
antelopev2
yunet-sface
```

The default model is `buffalo_sc`. Check the upstream model license before use.

## Docker

Pull the latest image from GitHub Container Registry:

```bash
docker pull ghcr.io/twocolors/rest-face-detect:latest
```

Build the image:

```bash
docker build --no-cache -t rest-face-detect .
```

Run it:

```bash
docker run -d \
  --name rest-face-detect \
  --restart unless-stopped \
  -p 8000:8000 \
  -e MODEL=buffalo_sc \
  -e IMAGE_SIZE=10 \
  -v "$(pwd)/data:/data" \
  -v "$(pwd)/models:/models" \
  --tmpfs /tmp:rw,noexec,nosuid,size=64m \
  rest-face-detect
```

The database persists in `./data` and the model persists in `./models`. The first
start downloads the model.

### Docker Compose

```yaml
services:
  face-detect:
    build: .
    image: rest-face-detect
    restart: unless-stopped
    ports:
      - "8000:8000"
    environment:
      DATA_PATH: /data
      MODEL_PATH: /models
      MODEL: buffalo_sc
      ADDR: :8000
      DEBUG: "false"
      UI: "false"
      IMAGE_SIZE: "10"
    volumes:
      - ./data:/data
      - ./models:/models
    tmpfs:
      - /tmp:rw,noexec,nosuid,size=64m
```

Start it with `docker compose up -d --build`.

## REST API

All image endpoints accept raw JPEG, PNG or BMP bytes. They also accept JSON:

```json
{"image":"<base64 image>"}
```

The `image` value can be either a pure Base64 string:

```bash
curl -X POST \
  -H 'Content-Type: application/json' \
  -d "{\"image\":\"$(base64 -i person.jpg | tr -d '\\n')\"}" \
  http://localhost:8000/register/Alice
```

or a complete data URL, which is convenient in a browser:

```json
{"image":"data:image/jpeg;base64,/9j/4AAQSkZJRgABAQ..."}
```

Person IDs must match `^[a-zA-Z0-9_-]{1,64}$`.

### Health

```bash
curl http://localhost:8000/health
```

```json
{"model":"buffalo_sc","persons":1,"embeddings":1}
```

### Register a person

Exactly one face is required. Multiple registrations for the same ID add another
embedding.

```bash
curl -X POST \
  -H 'Content-Type: image/jpeg' \
  --data-binary @person.jpg \
  http://localhost:8000/register/Alice
```

```json
{"id":"Alice","created_at":"2026-08-21T12:00:00Z","embeddings":1}
```

List or delete registered people:

```bash
curl http://localhost:8000/register
```

```json
[{"id":"Alice","created_at":"2026-08-21T12:00:00Z","embeddings":1}]
```

```bash
curl -X DELETE http://localhost:8000/register/Alice
```

```json
{"id":"Alice","deleted":true}
```

### Identify faces

Returns matches for every detected face, sorted by similarity descending.

```bash
curl -X POST \
  -H 'Content-Type: image/jpeg' \
  --data-binary @group.jpg \
  http://localhost:8000/identify
```

```json
[{"face":1,"matches":[{"id":"Alice","similarity":0.87}]}]
```

### Analyze faces

Returns the native detector result with the bounding box, confidence, estimated
age and gender. With no detected faces, the response is `[]`.

```bash
curl -X POST \
  -H 'Content-Type: image/jpeg' \
  --data-binary @person.jpg \
  http://localhost:8000/analyze
```

```json
[{"face":1,"box":[120,80,310,340],"score":0.98,"age":34,"gender":"male"}]
```

Errors use the following shape:

```json
{"error":"invalid image"}
```

Registration stores embeddings and a small PNG face preview in SQLite; the
original input image is not stored. Temporary path-based native API inputs are
written to `/tmp`, which is why the Docker examples mount it as tmpfs.
