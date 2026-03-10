# DiegoC Agent - 构建与安装
BINARY := diegoc-agent
INSTALL_PATH ?= /usr/local/bin

.PHONY: build install
build:
	go build -o $(BINARY) ./cmd/diegoc-agent

# 安装到 PATH：make install 或 make install INSTALL_PATH=~/.local/bin
install: build
	cp $(BINARY) $(INSTALL_PATH)/
	@echo "Installed to $(INSTALL_PATH)/$(BINARY)"
	@echo "Run '$(BINARY) --version' from any directory to verify."
