// +build interactive

package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"
)

// Interactive 交互式测试客户端
// 编译: go build -tags interactive -o testclient_interactive ./cmd/testclient/

func mainInteractive() {
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("Interactive test client")
	log.Printf("  Server: %s", *serverAddr)

	conn, _, err := websocket.DefaultDialer.Dial(*serverAddr, nil)
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	log.Printf("Connected. Type 'help' for commands.")

	// 启动消息接收
	go func() {
		for {
			frame, err := recvFrame(conn)
			if err != nil {
				log.Printf("Connection closed: %v", err)
				os.Exit(1)
			}
			printFrame("RECV", frame)
		}
	}()

	// 交互式命令循环
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Print("> ")
			continue
		}

		parts := strings.Fields(line)
		cmd := parts[0]

		switch cmd {
		case "help":
			printHelp()

		case "auth":
			// auth [user_id] [token]
			uid := *userID
			tok := *token
			if len(parts) > 1 {
				uid = parts[1]
			}
			if len(parts) > 2 {
				tok = parts[2]
			}
			sendAuth(conn, uid, tok)

		case "join":
			// join [room_id]
			rid := *roomID
			if len(parts) > 1 {
				rid = parts[1]
			}
			sendJoin(conn, rid)

		case "leave":
			// leave [room_id]
			rid := *roomID
			if len(parts) > 1 {
				rid = parts[1]
			}
			sendLeave(conn, rid)

		case "resume":
			// resume [room_id]
			rid := *roomID
			if len(parts) > 1 {
				rid = parts[1]
			}
			sendResume(conn, rid)

		case "hb", "heartbeat":
			// heartbeat [room_id]
			rid := *roomID
			if len(parts) > 1 {
				rid = parts[1]
			}
			sendHB(conn, rid)

		case "biz":
			// biz <action> [key=value ...]
			if len(parts) < 2 {
				log.Printf("Usage: biz <action> [key=value ...]")
			} else {
				sendBiz(conn, parts[1], parts[2:])
			}

		case "raw":
			// raw <msg_type_hex> <json_payload>
			if len(parts) < 3 {
				log.Printf("Usage: raw <msg_type> <payload_json>")
			} else {
				sendRaw(conn, parts[1], strings.Join(parts[2:], " "))
			}

		case "quit", "exit":
			log.Printf("Bye!")
			return

		default:
			log.Printf("Unknown command: %s. Type 'help' for usage.", cmd)
		}

		fmt.Print("> ")
	}
}

func printHelp() {
	fmt.Println(`
Commands:
  help                      - Show this help
  auth [user_id] [token]    - Send auth request
  join [room_id]            - Join a room
  leave [room_id]           - Leave a room
  resume [room_id]          - Resume after reconnect
  hb [room_id]              - Send heartbeat
  biz <action> [k=v ...]    - Send business request
  raw <type_hex> <payload>  - Send raw frame
  quit                      - Exit

Examples:
  auth user001 dev_token
  join room001
  biz send_gift gift_id=1 count=10
  raw 0x0010 {"action":"test"}
`)
}

func sendAuth(conn *websocket.Conn, uid, tok string) {
	payload, _ := msgpack.Marshal(map[string]interface{}{
		"token":   tok,
		"user_id": uid,
	})

	frame := &Frame{
		MsgType: MsgTypeAuth,
		Seq:     nextSeq(),
		Payload: payload,
	}

	if err := sendFrame(conn, frame); err != nil {
		log.Printf("Send failed: %v", err)
		return
	}
	printFrame("SEND", frame)
}

func sendJoin(conn *websocket.Conn, rid string) {
	payload, _ := msgpack.Marshal(map[string]interface{}{
		"room_id": rid,
	})

	frame := &Frame{
		MsgType: MsgTypeJoinRoom,
		Seq:     nextSeq(),
		Payload: payload,
	}

	if err := sendFrame(conn, frame); err != nil {
		log.Printf("Send failed: %v", err)
		return
	}
	printFrame("SEND", frame)
}

func sendLeave(conn *websocket.Conn, rid string) {
	payload, _ := msgpack.Marshal(map[string]interface{}{
		"room_id": rid,
	})

	frame := &Frame{
		MsgType: MsgTypeLeaveRoom,
		Seq:     nextSeq(),
		Payload: payload,
	}

	if err := sendFrame(conn, frame); err != nil {
		log.Printf("Send failed: %v", err)
		return
	}
	printFrame("SEND", frame)
}

func sendResume(conn *websocket.Conn, rid string) {
	payload, _ := msgpack.Marshal(map[string]interface{}{
		"room_id": rid,
	})

	frame := &Frame{
		MsgType: MsgTypeResume,
		Seq:     nextSeq(),
		Payload: payload,
	}

	if err := sendFrame(conn, frame); err != nil {
		log.Printf("Send failed: %v", err)
		return
	}
	printFrame("SEND", frame)
}

func sendHB(conn *websocket.Conn, rid string) {
	payload, _ := msgpack.Marshal(map[string]interface{}{
		"room_id":   rid,
		"timestamp": time.Now().UnixMilli(),
	})

	frame := &Frame{
		MsgType: MsgTypeHeartbeat,
		Seq:     nextSeq(),
		Payload: payload,
	}

	if err := sendFrame(conn, frame); err != nil {
		log.Printf("Send failed: %v", err)
		return
	}
	printFrame("SEND", frame)
}

func sendBiz(conn *websocket.Conn, action string, kvPairs []string) {
	data := map[string]interface{}{
		"action": action,
	}

	for _, kv := range kvPairs {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			// 尝试解析为数字
			if i, err := strconv.Atoi(parts[1]); err == nil {
				data[parts[0]] = i
			} else if f, err := strconv.ParseFloat(parts[1], 64); err == nil {
				data[parts[0]] = f
			} else if parts[1] == "true" {
				data[parts[0]] = true
			} else if parts[1] == "false" {
				data[parts[0]] = false
			} else {
				data[parts[0]] = parts[1]
			}
		}
	}

	payload, _ := msgpack.Marshal(data)

	frame := &Frame{
		MsgType: MsgTypeBizRequest,
		Seq:     nextSeq(),
		Payload: payload,
	}

	if err := sendFrame(conn, frame); err != nil {
		log.Printf("Send failed: %v", err)
		return
	}
	printFrame("SEND", frame)
}

func sendRaw(conn *websocket.Conn, typeStr, payloadJSON string) {
	msgType, err := strconv.ParseUint(strings.TrimPrefix(typeStr, "0x"), 16, 32)
	if err != nil {
		log.Printf("Invalid message type: %s", typeStr)
		return
	}

	// 简单的 JSON 到 msgpack 转换
	payload, _ := msgpack.Marshal(map[string]interface{}{"raw": payloadJSON})

	frame := &Frame{
		MsgType: uint32(msgType),
		Seq:     nextSeq(),
		Payload: payload,
	}

	if err := sendFrame(conn, frame); err != nil {
		log.Printf("Send failed: %v", err)
		return
	}
	printFrame("SEND", frame)
}

func printFrame(direction string, frame *Frame) {
	typeName := getMsgTypeName(frame.MsgType)

	var payload map[string]interface{}
	msgpack.Unmarshal(frame.Payload, &payload)

	log.Printf("[%s] type=0x%04X (%s) seq=%d payload=%v",
		direction, frame.MsgType, typeName, frame.Seq, payload)
}

func getMsgTypeName(msgType uint32) string {
	names := map[uint32]string{
		MsgTypeAuth:         "Auth",
		MsgTypeJoinRoom:     "JoinRoom",
		MsgTypeLeaveRoom:    "LeaveRoom",
		MsgTypeResume:       "Resume",
		MsgTypeBizRequest:   "BizRequest",
		MsgTypeHeartbeat:    "Heartbeat",
		MsgTypeAuthResp:     "AuthResp",
		MsgTypeJoinRoomResp: "JoinRoomResp",
		MsgTypeLeaveRoomResp: "LeaveRoomResp",
		MsgTypeResumeResp:   "ResumeResp",
		MsgTypePushMessage:  "PushMessage",
		MsgTypeBizResponse:  "BizResponse",
		MsgTypeHeartbeatResp: "HeartbeatResp",
	}

	if name, ok := names[msgType]; ok {
		return name
	}
	return "Unknown"
}
