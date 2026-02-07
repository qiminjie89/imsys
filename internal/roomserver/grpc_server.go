package roomserver

import (
	"github.com/qiminjie89/imsys/pkg/logger"
	"go.uber.org/zap"
)

// runGRPCServer 运行 gRPC 服务（供 Gateway 连接）
func (s *Server) runGRPCServer() {
	logger.Info("starting grpc server",
		zap.String("addr", s.cfg.Server.GRPCAddr),
	)

	// TODO: 实现 gRPC 服务
	// 1. 创建 gRPC server
	// 2. 注册 RoomServerGateway 服务
	// 3. 监听并服务

	// 暂时阻塞
	<-s.ctx.Done()
}

// TODO: 实现 gRPC Channel 方法
// func (s *Server) Channel(stream pb.RoomServerGateway_ChannelServer) error {
//     // 1. 注册 Gateway 连接
//     // 2. 启动收发循环
//     // 3. 处理各种消息类型
// }
