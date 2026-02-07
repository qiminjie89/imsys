#!/bin/bash

# Proto 代码生成脚本

set -e

PROTO_DIR="api/proto"
GEN_DIR="api/proto/gen"

# 创建输出目录
mkdir -p $GEN_DIR

# 生成 Go 代码
protoc \
    --proto_path=$PROTO_DIR \
    --go_out=$GEN_DIR \
    --go_opt=paths=source_relative \
    --go-grpc_out=$GEN_DIR \
    --go-grpc_opt=paths=source_relative \
    $PROTO_DIR/*.proto

echo "Proto generation completed: $GEN_DIR"
