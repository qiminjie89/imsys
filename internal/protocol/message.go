// Package protocol 定义 IM 系统的消息类型和协议常量
package protocol

// 客户端 ↔ Gateway 消息类型
const (
	// 客户端 → Gateway
	MsgTypeAuth       uint32 = 0x0001 // 认证请求
	MsgTypeJoinRoom   uint32 = 0x0002 // 加入房间
	MsgTypeLeaveRoom  uint32 = 0x0003 // 离开房间
	MsgTypeResume     uint32 = 0x0004 // 断线重连恢复
	MsgTypeBizRequest uint32 = 0x0010 // 业务请求（转发给 Python 后端）
	MsgTypeHeartbeat  uint32 = 0x00FF // 心跳

	// Gateway → 客户端
	MsgTypeAuthResp      uint32 = 0x1001 // 认证响应
	MsgTypeJoinRoomResp  uint32 = 0x1002 // 加入房间响应
	MsgTypeLeaveRoomResp uint32 = 0x1003 // 离开房间响应
	MsgTypeResumeResp    uint32 = 0x1004 // Resume 响应
	MsgTypePushMessage   uint32 = 0x1010 // 推送消息
	MsgTypeBizResponse   uint32 = 0x1011 // 业务响应
	MsgTypeHeartbeatResp uint32 = 0x10FF // 心跳响应
)

// Gateway ↔ Roomserver 内部消息类型
const (
	// Gateway → Roomserver
	InternalMsgJoinRoom       uint32 = 0x2001 // JoinRoom
	InternalMsgLeaveRoom      uint32 = 0x2002 // LeaveRoom
	InternalMsgResume         uint32 = 0x2003 // Resume
	InternalMsgBatchReSync    uint32 = 0x2004 // 崩溃恢复批量同步
	InternalMsgGatewayInfo    uint32 = 0x2005 // Gateway 上报自身信息
	InternalMsgBatchLeave     uint32 = 0x2006 // 批量离开
	InternalMsgReSyncComplete uint32 = 0x2007 // BatchReSync 完成标记
	InternalMsgDisconnect     uint32 = 0x2008 // 用户断连通知

	// Roomserver → Gateway
	InternalMsgJoinRoomAck uint32 = 0x3001 // JoinRoom/LeaveRoom/Resume ACK
	InternalMsgBroadcast   uint32 = 0x3002 // 广播消息
	InternalMsgUnicast     uint32 = 0x3003 // 单播消息
	InternalMsgEpochNotify uint32 = 0x3004 // Epoch 变更通知
	InternalMsgRoomClose   uint32 = 0x3005 // 房间关闭通知
	InternalMsgMulticast   uint32 = 0x3006 // 多播消息
)

// 消息优先级
type MessagePriority int

const (
	PriorityCritical MessagePriority = 0 // 关键消息：系统通知、踢人、房间关闭
	PriorityHigh     MessagePriority = 1 // 高优消息：礼物特效、中奖通知
	PriorityNormal   MessagePriority = 2 // 普通消息：弹幕、聊天
	PriorityLow      MessagePriority = 3 // 低优消息：在线人数更新、点赞动画
)

// JoinState 表示 JoinRoom 状态机
type JoinState int

const (
	JoinStateInit      JoinState = iota // 未加入任何房间
	JoinStatePending                    // JoinRoom 请求已发，等待 ACK
	JoinStateConfirmed                  // 已确认加入房间
)

// MemberStatus 表示房间成员状态
type MemberStatus int

const (
	MemberStatusOnline       MemberStatus = iota // 在线
	MemberStatusDisconnected                     // 已断连，等待重连
)

// ServerStatus 表示 Roomserver 状态
type ServerStatus int

const (
	ServerStatusRecovering ServerStatus = iota // 恢复中
	ServerStatusReady                          // 就绪
)
