package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"
)

// Message types (from protocol/messages.go)
const (
	// Client → Gateway
	MsgTypeAuth       uint32 = 0x0001 // 认证请求
	MsgTypeJoinRoom   uint32 = 0x0002 // 加入房间
	MsgTypeLeaveRoom  uint32 = 0x0003 // 离开房间
	MsgTypeResume     uint32 = 0x0004 // 断线重连恢复
	MsgTypeBizRequest uint32 = 0x0010 // 业务请求
	MsgTypeHeartbeat  uint32 = 0x00FF // 心跳

	// Gateway → Client
	MsgTypeAuthResp      uint32 = 0x1001 // 认证响应
	MsgTypeJoinRoomResp  uint32 = 0x1002 // 加入房间响应
	MsgTypeLeaveRoomResp uint32 = 0x1003 // 离开房间响应
	MsgTypeResumeResp    uint32 = 0x1004 // Resume 响应
	MsgTypePushMessage   uint32 = 0x1010 // 推送消息
	MsgTypeBizResponse   uint32 = 0x1011 // 业务响应
	MsgTypeHeartbeatResp uint32 = 0x10FF // 心跳响应
)

const HeaderSize = 16 // 4 + 8 + 4

// Frame represents a message frame
type Frame struct {
	MsgType uint32
	Seq     uint64
	Payload []byte
}

// EncodeFrame encodes a frame to binary
func EncodeFrame(f *Frame) []byte {
	payloadLen := len(f.Payload)
	buf := make([]byte, HeaderSize+payloadLen)
	binary.BigEndian.PutUint32(buf[0:4], f.MsgType)
	binary.BigEndian.PutUint64(buf[4:12], f.Seq)
	binary.BigEndian.PutUint32(buf[12:16], uint32(payloadLen))
	if payloadLen > 0 {
		copy(buf[HeaderSize:], f.Payload)
	}
	return buf
}

// DecodeFrame decodes a frame from binary
func DecodeFrame(data []byte) (*Frame, error) {
	if len(data) < HeaderSize {
		return nil, fmt.Errorf("data too short: %d", len(data))
	}
	msgType := binary.BigEndian.Uint32(data[0:4])
	seq := binary.BigEndian.Uint64(data[4:12])
	payloadLen := binary.BigEndian.Uint32(data[12:16])
	var payload []byte
	if payloadLen > 0 {
		payload = data[HeaderSize : HeaderSize+int(payloadLen)]
	}
	return &Frame{MsgType: msgType, Seq: seq, Payload: payload}, nil
}

// AuthRequest for authentication
type AuthRequest struct {
	Token  string `msgpack:"token"`
	UserID string `msgpack:"user_id"`
}

// AuthResp response
type AuthResp struct {
	Success   bool   `msgpack:"success"`
	UserID    string `msgpack:"user_id"`
	GatewayID string `msgpack:"gateway_id"`
	Code      int    `msgpack:"code"`
	Message   string `msgpack:"message"`
}

// JoinRoomRequest for joining a room
type JoinRoomRequest struct {
	RoomID string `msgpack:"room_id"`
}

// JoinRoomAck response
type JoinRoomAck struct {
	Code    int    `msgpack:"code"`
	Message string `msgpack:"message"`
	RoomID  string `msgpack:"room_id"`
	Success bool   `msgpack:"success"`
}

func main() {
	addr := flag.String("addr", "ws://localhost:8080/ws", "WebSocket server address")
	userID := flag.String("user", "test-user-1", "User ID")
	roomID := flag.String("room", "room-1", "Room ID")
	flag.Parse()

	fmt.Printf("Connecting to %s...\n", *addr)

	conn, _, err := websocket.DefaultDialer.Dial(*addr, nil)
	if err != nil {
		log.Fatal("Failed to connect:", err)
	}
	defer conn.Close()
	fmt.Println("Connected!")

	// Handle incoming messages
	go func() {
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				log.Println("Read error:", err)
				return
			}

			frame, err := DecodeFrame(message)
			if err != nil {
				log.Println("Decode frame error:", err)
				continue
			}

			switch frame.MsgType {
			case MsgTypeAuthResp:
				var resp AuthResp
				if err := msgpack.Unmarshal(frame.Payload, &resp); err != nil {
					log.Println("Unmarshal AuthResp error:", err)
					continue
				}
				fmt.Printf("AuthResp: success=%v, user_id=%s, gateway_id=%s, code=%d, message=%s\n",
					resp.Success, resp.UserID, resp.GatewayID, resp.Code, resp.Message)
			case MsgTypeJoinRoomResp:
				var ack JoinRoomAck
				if err := msgpack.Unmarshal(frame.Payload, &ack); err != nil {
					log.Println("Unmarshal JoinRoomAck error:", err)
					continue
				}
				fmt.Printf("JoinRoomAck: success=%v, room_id=%s, code=%d, message=%s\n",
					ack.Success, ack.RoomID, ack.Code, ack.Message)
			case MsgTypeLeaveRoomResp:
				fmt.Println("LeaveRoomResp received")
			case MsgTypeHeartbeatResp:
				fmt.Println("HeartbeatResp received")
			case MsgTypePushMessage:
				fmt.Println("PushMessage received")
			default:
				fmt.Printf("Received message type: 0x%04X, seq: %d, payload_len: %d\n",
					frame.MsgType, frame.Seq, len(frame.Payload))
			}
		}
	}()

	// Send auth request
	fmt.Println("Sending auth request...")
	authReq := &AuthRequest{Token: "", UserID: *userID} // In dev mode, use user_id directly
	payload, _ := msgpack.Marshal(authReq)
	frame := &Frame{MsgType: MsgTypeAuth, Payload: payload, Seq: 1}
	if err := conn.WriteMessage(websocket.BinaryMessage, EncodeFrame(frame)); err != nil {
		log.Fatal("Write error:", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Send join room request
	fmt.Printf("Sending join room request (room=%s)...\n", *roomID)
	joinReq := &JoinRoomRequest{RoomID: *roomID}
	payload, _ = msgpack.Marshal(joinReq)
	frame = &Frame{MsgType: MsgTypeJoinRoom, Payload: payload, Seq: 2}
	if err := conn.WriteMessage(websocket.BinaryMessage, EncodeFrame(frame)); err != nil {
		log.Fatal("Write error:", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Send heartbeat
	fmt.Println("Sending heartbeat...")
	frame = &Frame{MsgType: MsgTypeHeartbeat, Seq: 3}
	if err := conn.WriteMessage(websocket.BinaryMessage, EncodeFrame(frame)); err != nil {
		log.Fatal("Write error:", err)
	}

	// Wait for interrupt
	fmt.Println("\nTest client running. Press Ctrl+C to exit.")
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	<-interrupt

	fmt.Println("Closing connection...")
	conn.Close()
}
