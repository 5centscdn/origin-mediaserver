.PHONY: build run dev clean panel test tidy

BINARY := origin-mediaserver
CONFIG := config.toml

## Build single binary (no CGO, no external deps except pure-Go SQLite)
build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o $(BINARY) ./cmd/server

## Build a fully static Linux binary (suitable for Docker FROM scratch)
build-static:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	go build -ldflags="-s -w -extldflags=-static" -o $(BINARY)-linux-amd64 ./cmd/server

## Run in development mode (config.toml must exist)
run: build
	./$(BINARY) --config $(CONFIG)

## Create config.toml from example if it doesn't exist, then run
dev:
	@[ -f $(CONFIG) ] || cp config.example.toml $(CONFIG)
	@echo "⚠ Edit $(CONFIG) and set a real jwt_secret before production use."
	CGO_ENABLED=0 go run ./cmd/server --config $(CONFIG)

## Start Next.js panel dev server (proxies API to localhost:8080)
panel:
	cd panel && npm install && npm run dev

## Build panel for embedding (output goes to panel/out)
panel-build:
	cd panel && npm install && npm run build
	@echo "Panel built to panel/out/ — add go:embed directive in cmd/server/main.go"

## Run all Go tests
test:
	go test ./...

## Tidy go.mod
tidy:
	go mod tidy

## Remove build artifacts
clean:
	rm -f $(BINARY) $(BINARY)-linux-amd64
	rm -rf panel/.next panel/out

## Show streaming URLs for a stream named "test" on app "live"
urls:
	@echo "RTMP ingest:  rtmp://localhost:1935/live/test"
	@echo "RTMPS ingest: rtmps://localhost:1936/live/test"
	@echo "HLS (live):   http://localhost:8080/hls/live/test/index.m3u8"
	@echo "HLS (ABR):    http://localhost:8080/hls/live/camera/master.m3u8"
	@echo "DASH:         http://localhost:8080/dash/live/test/manifest.mpd"
	@echo "DVR rewind:   http://localhost:8080/dvr/live/test/index.m3u8?start=<unix_ms>"
	@echo "WHIP:         http://localhost:8080/whip/live/test"
	@echo "WHEP:         http://localhost:8080/whep/live/test"
	@echo "Metrics:      http://localhost:8080/metrics"
	@echo "API:          http://localhost:8080/api/v1/streams"
	@echo "Panel:        http://localhost:8080/ (or http://localhost:3000 in dev)"
