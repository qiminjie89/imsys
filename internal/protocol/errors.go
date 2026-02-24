// Package protocol 定义错误码
package protocol

// 错误码定义
const (
	// 通用错误
	ErrCodeSuccess       = 0    // 成功
	ErrCodeInternalError = 1    // 内部错误
	ErrCodeInvalidRequest = 2   // 无效请求

	// 认证相关 (1xxx)
	ErrCodeAuthFailed   = 1001 // 认证失败
	ErrCodeTokenExpired = 1002 // Token 过期

	// 房间相关 (2xxx)
	ErrCodeRoomNotFound  = 2001 // 房间不存在
	ErrCodeUserNotInRoom = 2002 // 用户不在房间
	ErrCodeRoomClosed    = 2003 // 房间已关闭
	ErrCodeRoomMismatch  = 2004 // Resume 时 room_id 不匹配

	// 路由相关 (3xxx)
	ErrCodeOwnerUnavailable = 3001 // 目标 Roomserver 不可用

	// 心跳相关 (4xxx)
	ErrCodeHeartbeatMismatch  = 4001 // 心跳检测到 room_id 不一致
	ErrCodeHeartbeatNotInRoom = 4002 // 心跳检测到用户不在房间

	// 服务状态相关 (5xxx)
	ErrCodeServiceRecovering = 5001 // 服务恢复中
	ErrCodeServiceDegraded   = 5002 // 服务降级中
	ErrCodeServiceOverloaded = 5003 // 服务过载
)

// ErrCodeMessage 错误码对应的消息
var ErrCodeMessage = map[int]string{
	ErrCodeSuccess:            "success",
	ErrCodeInternalError:      "internal_error",
	ErrCodeInvalidRequest:     "invalid_request",
	ErrCodeAuthFailed:         "auth_failed",
	ErrCodeTokenExpired:       "token_expired",
	ErrCodeRoomNotFound:       "room_not_found",
	ErrCodeUserNotInRoom:      "user_not_in_room",
	ErrCodeRoomClosed:         "room_closed",
	ErrCodeRoomMismatch:       "room_mismatch",
	ErrCodeOwnerUnavailable:   "owner_unavailable",
	ErrCodeHeartbeatMismatch:  "heartbeat_mismatch",
	ErrCodeHeartbeatNotInRoom: "heartbeat_not_in_room",
	ErrCodeServiceRecovering:  "service_recovering",
	ErrCodeServiceDegraded:    "service_degraded",
	ErrCodeServiceOverloaded:  "service_overloaded",
}
