BINARY := agy-pool
INSTALL_DIR := $(HOME)/.local/bin

.PHONY: build install test clean

build:
	go build -ldflags="-s -w" -o $(BINARY) ./cmd/agy-pool

install: build
	mkdir -p $(INSTALL_DIR)
	install -m 0755 $(BINARY) $(INSTALL_DIR)/$(BINARY)

test:
	go test -v ./...

clean:
	rm -f $(BINARY)
