APP_NAME := deployer-server
TERM_NAME := terminal-agent
BIN_DIR  := ./bin
LDFLAGS  := -s -w

VPS    ?= vps
VPS_DIR ?= ~/bin

.PHONY: build build-linux build-all clean run test deploy \
	build-terminal build-terminal-windows-amd64 build-terminal-windows-arm64 \
	build-terminal-linux-amd64 build-terminal-darwin-arm64

build:
	GOOS=darwin GOARCH=arm64 go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(APP_NAME)-darwin-arm64 ./cmd/$(APP_NAME)

build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(APP_NAME)-linux-amd64 ./cmd/$(APP_NAME)

build-all:
	GOOS=darwin GOARCH=arm64 go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(APP_NAME)-darwin-arm64 ./cmd/$(APP_NAME) &\
	GOOS=linux GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(APP_NAME)-linux-amd64 ./cmd/$(APP_NAME) &\
	wait

run: build
	$(BIN_DIR)/$(APP_NAME)-darwin-arm64

test:
	go test ./...

deploy: build-linux
	ssh $(VPS) "rm -f $(VPS_DIR)/$(APP_NAME)-linux-amd64"
	scp $(BIN_DIR)/$(APP_NAME)-linux-amd64 $(VPS):$(VPS_DIR)/

build-terminal-windows-amd64:
	GOOS=windows GOARCH=amd64 go build -ldflags '$(LDFLAGS) -H windowsgui' -trimpath -o $(BIN_DIR)/$(TERM_NAME)-windows-amd64.exe ./cmd/$(TERM_NAME)

build-terminal-windows-arm64:
	GOOS=windows GOARCH=arm64 go build -ldflags '$(LDFLAGS) -H windowsgui' -trimpath -o $(BIN_DIR)/$(TERM_NAME)-windows-arm64.exe ./cmd/$(TERM_NAME)

build-terminal-linux-amd64:
	GOOS=linux GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -trimpath -o $(BIN_DIR)/$(TERM_NAME)-linux-amd64 ./cmd/$(TERM_NAME)

build-terminal-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -ldflags '$(LDFLAGS)' -trimpath -o $(BIN_DIR)/$(TERM_NAME)-darwin-arm64 ./cmd/$(TERM_NAME)

build-terminal: build-terminal-windows-amd64 build-terminal-windows-arm64 build-terminal-linux-amd64 build-terminal-darwin-arm64

clean:
	rm -rf $(BIN_DIR)
