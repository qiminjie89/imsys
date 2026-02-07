.PHONY: all build clean proto test lint

# 变量
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod
BINARY_DIR=bin

# 服务列表
SERVICES=gateway roomserver

all: proto build

# 编译所有服务
build: $(SERVICES)

gateway:
	$(GOBUILD) -o $(BINARY_DIR)/gateway ./cmd/gateway

roomserver:
	$(GOBUILD) -o $(BINARY_DIR)/roomserver ./cmd/roomserver

# 生成 proto 代码
proto:
	./scripts/proto_gen.sh

# 运行测试
test:
	$(GOTEST) -v ./...

# 代码检查
lint:
	golangci-lint run ./...

# 清理
clean:
	rm -rf $(BINARY_DIR)
	rm -rf api/proto/gen

# 依赖管理
deps:
	$(GOMOD) download
	$(GOMOD) tidy

# Docker 镜像构建
docker-gateway:
	docker build -f deployments/docker/Dockerfile.gateway -t imsys-gateway:latest .

docker-roomserver:
	docker build -f deployments/docker/Dockerfile.roomserver -t imsys-roomserver:latest .

docker: docker-gateway docker-roomserver

# 运行服务（开发环境）
run-gateway:
	$(GOBUILD) -o $(BINARY_DIR)/gateway ./cmd/gateway && ./$(BINARY_DIR)/gateway -config configs/gateway.yaml

run-roomserver:
	$(GOBUILD) -o $(BINARY_DIR)/roomserver ./cmd/roomserver && ./$(BINARY_DIR)/roomserver -config configs/roomserver.yaml
