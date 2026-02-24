package protocol

import "time"

// ========== Gateway → Roomserver ==========

// JoinRoomRequest JoinRoom 请求
type JoinRoomRequest struct {
	UserID    string `msgpack:"user_id"`
	RoomID    string `msgpack:"room_id"`
	GatewayID string `msgpack:"gateway_id"`
}

// LeaveRoomRequest LeaveRoom 请求
type LeaveRoomRequest struct {
	UserID string `msgpack:"user_id"`
	RoomID string `msgpack:"room_id"`
}

// ResumeRequest Resume 请求
type ResumeRequest struct {
	UserID    string `msgpack:"user_id"`
	RoomID    string `msgpack:"room_id"`
	GatewayID string `msgpack:"gateway_id"`
}

// BatchReSync 崩溃恢复批量同步
type BatchReSync struct {
	GatewayID string         `msgpack:"gateway_id"`
	Users     []UserRoomPair `msgpack:"users"`
}

// UserRoomPair 用户-房间对
type UserRoomPair struct {
	UserID string `msgpack:"user_id"`
	RoomID string `msgpack:"room_id"`
}

// ReSyncComplete BatchReSync 完成标记
type ReSyncComplete struct {
	GatewayID  string `msgpack:"gateway_id"`
	TotalUsers int    `msgpack:"total_users"`
}

// DisconnectNotify 用户断连通知
type DisconnectNotify struct {
	UserID    string    `msgpack:"user_id"`
	RoomID    string    `msgpack:"room_id"`
	GatewayID string    `msgpack:"gateway_id"`
	Timestamp time.Time `msgpack:"timestamp"`
}

// GatewayInfo Gateway 信息上报
type GatewayInfo struct {
	GatewayID   string `msgpack:"gateway_id"`
	Addr        string `msgpack:"addr"`
	Connections int    `msgpack:"connections"`
}

// ========== Roomserver → Gateway ==========

// JoinRoomAck JoinRoom/LeaveRoom/Resume ACK
type JoinRoomAck struct {
	UserID  string `msgpack:"user_id"`
	RoomID  string `msgpack:"room_id"`
	Success bool   `msgpack:"success"`
	Code    int    `msgpack:"code"`
	Message string `msgpack:"message,omitempty"`
}

// BroadcastMessage 广播消息
type BroadcastMessage struct {
	RoomID  string `msgpack:"room_id"`
	Seq     uint64 `msgpack:"seq"`
	Payload []byte `msgpack:"payload"` // 压缩后的消息体
}

// UnicastMessage 单播消息
type UnicastMessage struct {
	UserID  string `msgpack:"user_id"`
	Payload []byte `msgpack:"payload"`
}

// MulticastMessage 多播消息
type MulticastMessage struct {
	RoomID  string   `msgpack:"room_id"`
	UserIDs []string `msgpack:"user_ids"`
	Payload []byte   `msgpack:"payload"`
}

// EpochNotify Epoch 变更通知
type EpochNotify struct {
	ServerID  string `msgpack:"server_id"` // Roomserver 实例 ID
	Epoch     uint64 `msgpack:"epoch"`     // 启动时间戳（毫秒）
	Timestamp int64  `msgpack:"ts"`
}

// RoomCloseNotify 房间关闭通知
type RoomCloseNotify struct {
	RoomID string `msgpack:"room_id"`
	Reason string `msgpack:"reason"` // ended, banned, etc.
}

// ========== ACK 错误码 ==========

const (
	CodeSuccess          = 0
	CodeUserNotFound     = 404
	CodeUserGone         = 410 // 用户已超时被清除
	CodeServiceRecovery  = 503 // Roomserver 恢复中
	CodeInternalError    = 500
	CodeRoomNotFound     = 4041
	CodeAlreadyInRoom    = 4091
	CodeNotInRoom        = 4092
)
