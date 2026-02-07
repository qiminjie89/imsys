package gateway

import (
	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/logger"
	"go.uber.org/zap"
)

// handleJoinRoom 处理 JoinRoom 请求
func (s *Server) handleJoinRoom(conn *Connection, frame *protocol.Frame) {
	var req struct {
		RoomID string `msgpack:"room_id"`
	}
	if err := protocol.Decode(frame.Payload, &req); err != nil {
		logger.Warn("decode join room request failed",
			zap.String("user_id", conn.UserID),
			zap.Error(err),
		)
		return
	}

	// 设置为 PENDING 状态
	conn.JoinState = protocol.JoinStatePending
	conn.RoomID = req.RoomID

	// 转发给 Roomserver
	s.roomserverClient.SendJoinRoom(conn.UserID, req.RoomID)

	logger.Debug("join room request forwarded",
		zap.String("user_id", conn.UserID),
		zap.String("room_id", req.RoomID),
	)
}

// handleLeaveRoom 处理 LeaveRoom 请求
func (s *Server) handleLeaveRoom(conn *Connection, frame *protocol.Frame) {
	var req struct {
		RoomID string `msgpack:"room_id"`
	}
	if err := protocol.Decode(frame.Payload, &req); err != nil {
		logger.Warn("decode leave room request failed",
			zap.String("user_id", conn.UserID),
			zap.Error(err),
		)
		return
	}

	// 转发给 Roomserver
	s.roomserverClient.SendLeaveRoom(conn.UserID, req.RoomID)

	logger.Debug("leave room request forwarded",
		zap.String("user_id", conn.UserID),
		zap.String("room_id", req.RoomID),
	)
}

// handleResume 处理 Resume 请求
func (s *Server) handleResume(conn *Connection, frame *protocol.Frame) {
	var req struct {
		RoomID string `msgpack:"room_id"`
	}
	if err := protocol.Decode(frame.Payload, &req); err != nil {
		logger.Warn("decode resume request failed",
			zap.String("user_id", conn.UserID),
			zap.Error(err),
		)
		return
	}

	conn.RoomID = req.RoomID
	conn.JoinState = protocol.JoinStatePending

	// 转发给 Roomserver
	s.roomserverClient.SendResume(conn.UserID, req.RoomID)

	logger.Debug("resume request forwarded",
		zap.String("user_id", conn.UserID),
		zap.String("room_id", req.RoomID),
	)
}

// handleHeartbeat 处理心跳（带弱自愈）
func (s *Server) handleHeartbeat(conn *Connection, frame *protocol.Frame) {
	var req struct {
		RoomID string `msgpack:"room_id"`
	}
	if err := protocol.Decode(frame.Payload, &req); err != nil {
		// 心跳可以不带 payload
		s.sendHeartbeatResp(conn)
		return
	}

	// 心跳弱自愈：检查 localRooms 与客户端上报是否一致
	if req.RoomID != "" {
		s.roomMu.RLock()
		localRoomID := ""
		for roomID, users := range s.localRooms {
			if users[conn.UserID] {
				localRoomID = roomID
				break
			}
		}
		s.roomMu.RUnlock()

		if localRoomID == "" && req.RoomID != "" {
			// 场景 A：用户不在任何房间但客户端声称在 room_id
			logger.Info("heartbeat self-healing: user not in room, sending join",
				zap.String("user_id", conn.UserID),
				zap.String("room_id", req.RoomID),
			)
			conn.JoinState = protocol.JoinStatePending
			conn.RoomID = req.RoomID
			s.roomserverClient.SendJoinRoom(conn.UserID, req.RoomID)
		} else if localRoomID != "" && localRoomID != req.RoomID {
			// 场景 B：用户在 room_A 但客户端声称在 room_B（不一致）
			logger.Info("heartbeat self-healing: room mismatch, leave then join",
				zap.String("user_id", conn.UserID),
				zap.String("local_room", localRoomID),
				zap.String("client_room", req.RoomID),
			)
			// 先 LeaveRoom 旧房间
			s.roomserverClient.SendLeaveRoom(conn.UserID, localRoomID)
			// 再 JoinRoom 新房间
			conn.JoinState = protocol.JoinStatePending
			conn.RoomID = req.RoomID
			s.roomserverClient.SendJoinRoom(conn.UserID, req.RoomID)
		}
	}

	s.sendHeartbeatResp(conn)
}

// handleBizRequest 处理业务请求（转发到 Kafka）
func (s *Server) handleBizRequest(conn *Connection, frame *protocol.Frame) {
	// 构造 Kafka 消息
	msg := &BizMessage{
		UserID:  conn.UserID,
		RoomID:  conn.RoomID,
		Payload: frame.Payload,
		Seq:     frame.Seq,
	}

	if err := s.kafkaProducer.Send(msg); err != nil {
		logger.Warn("send to kafka failed",
			zap.String("user_id", conn.UserID),
			zap.Error(err),
		)
		// TODO: 返回错误响应给客户端
	}
}

// sendHeartbeatResp 发送心跳响应
func (s *Server) sendHeartbeatResp(conn *Connection) {
	frame := &protocol.Frame{
		MsgType: protocol.MsgTypeHeartbeatResp,
		Seq:     0,
		Payload: nil,
	}
	data := protocol.EncodeFrame(frame)
	conn.Send(data)
}

// OnJoinRoomAck 处理 JoinRoom ACK（由 roomserver_client 调用）
func (s *Server) OnJoinRoomAck(userID, roomID string, success bool, code int, message string) {
	conn := s.GetConnection(userID)
	if conn == nil {
		return
	}

	if success {
		conn.JoinState = protocol.JoinStateConfirmed
		conn.RoomID = roomID
		s.AddUserToRoom(roomID, userID)

		logger.Debug("join room confirmed",
			zap.String("user_id", userID),
			zap.String("room_id", roomID),
		)
	} else {
		conn.JoinState = protocol.JoinStateInit
		conn.RoomID = ""

		logger.Warn("join room failed",
			zap.String("user_id", userID),
			zap.String("room_id", roomID),
			zap.Int("code", code),
			zap.String("message", message),
		)
	}

	// 发送响应给客户端
	s.sendJoinRoomResp(conn, success, code, message)
}

// OnLeaveRoomAck 处理 LeaveRoom ACK
func (s *Server) OnLeaveRoomAck(userID, roomID string, success bool) {
	conn := s.GetConnection(userID)

	// 无论连接是否存在，都从本地房间移除
	s.RemoveUserFromRoom(roomID, userID)

	if conn != nil {
		conn.JoinState = protocol.JoinStateInit
		conn.RoomID = ""
		s.sendLeaveRoomResp(conn, success)
	}
}

// sendJoinRoomResp 发送 JoinRoom 响应
func (s *Server) sendJoinRoomResp(conn *Connection, success bool, code int, message string) {
	resp := struct {
		Success bool   `msgpack:"success"`
		Code    int    `msgpack:"code"`
		Message string `msgpack:"message,omitempty"`
	}{
		Success: success,
		Code:    code,
		Message: message,
	}

	payload, _ := protocol.Encode(resp)
	frame := &protocol.Frame{
		MsgType: protocol.MsgTypeJoinRoomResp,
		Payload: payload,
	}
	data := protocol.EncodeFrame(frame)
	conn.Send(data)
}

// sendLeaveRoomResp 发送 LeaveRoom 响应
func (s *Server) sendLeaveRoomResp(conn *Connection, success bool) {
	resp := struct {
		Success bool `msgpack:"success"`
	}{
		Success: success,
	}

	payload, _ := protocol.Encode(resp)
	frame := &protocol.Frame{
		MsgType: protocol.MsgTypeLeaveRoomResp,
		Payload: payload,
	}
	data := protocol.EncodeFrame(frame)
	conn.Send(data)
}
