BINARY := agy-pro
INSTALL_DIR := $(HOME)/.local/bin
VERSION ?=
LDFLAGS := -s -w $(if $(VERSION),-X github.com/Tiggy-Chan/agy-pro/internal/vault.Version=$(VERSION),)

.PHONY: build build-windows install test clean

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/agy-pro

build-windows:
	GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(BINARY)-windows-amd64.exe ./cmd/agy-pro
	GOOS=windows GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(BINARY)-windows-arm64.exe ./cmd/agy-pro

install: build
	mkdir -p $(INSTALL_DIR)
	install -m 0755 $(BINARY) $(INSTALL_DIR)/$(BINARY)

test:
	go test -v ./...

clean:
	rm -f $(BINARY) $(BINARY)-windows-amd64.exe $(BINARY)-windows-arm64.exe
