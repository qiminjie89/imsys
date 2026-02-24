// +build loadtest

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"
)

// 负载测试客户端
// 编译: go build -tags loadtest -o loadtest ./cmd/testclient/

var (
	numClients   = flag.Int("clients", 100, "Number of concurrent clients")
	numRooms     = flag.Int("rooms", 10, "Number of rooms to distribute clients")
	rampUp       = flag.Duration("rampup", 10*time.Second, "Ramp-up duration")
	duration     = flag.Duration("duration", 60*time.Second, "Test duration after ramp-up")
	msgInterval  = flag.Duration("msg-interval", 5*time.Second, "Biz message interval per client")
)

// 统计
type Stats struct {
	connected    int64
	disconnected int64
	msgSent      int64
	msgRecv      int64
	errors       int64
}

var stats Stats

func mainLoadTest() {
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("Starting load test...")
	log.Printf("  Server: %s", *serverAddr)
	log.Printf("  Clients: %d", *numClients)
	log.Printf("  Rooms: %d", *numRooms)
	log.Printf("  Ramp-up: %s", *rampUp)
	log.Printf("  Duration: %s", *duration)

	ctx, cancel := context.WithCancel(context.Background())

	// 信号处理
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("Shutting down...")
		cancel()
	}()

	// 启动统计输出
	go statsLoop(ctx)

	// 计算每个客户端启动间隔
	interval := *rampUp / time.Duration(*numClients)

	var wg sync.WaitGroup

	// 逐步启动客户端
	for i := 0; i < *numClients; i++ {
		select {
		case <-ctx.Done():
			break
		default:
		}

		clientID := fmt.Sprintf("load_user_%04d", i)
		roomID := fmt.Sprintf("load_room_%03d", i%*numRooms)

		wg.Add(1)
		go func(id, room string) {
			defer wg.Done()
			runClient(ctx, id, room)
		}(clientID, roomID)

		time.Sleep(interval)
	}

	log.Printf("All clients started. Running for %s...", *duration)

	// 等待测试时间
	select {
	case <-ctx.Done():
	case <-time.After(*duration):
		log.Printf("Test duration completed.")
		cancel()
	}

	// 等待所有客户端退出
	wg.Wait()

	// 打印最终统计
	printFinalStats()
}

func runClient(ctx context.Context, userID, roomID string) {
	conn, _, err := websocket.DefaultDialer.Dial(*serverAddr, nil)
	if err != nil {
		atomic.AddInt64(&stats.errors, 1)
		return
	}
	defer conn.Close()

	// 认证
	if err := doAuth(conn, userID); err != nil {
		atomic.AddInt64(&stats.errors, 1)
		return
	}

	// 加入房间
	if err := doJoin(conn, roomID); err != nil {
		atomic.AddInt64(&stats.errors, 1)
		return
	}

	atomic.AddInt64(&stats.connected, 1)
	defer func() {
		atomic.AddInt64(&stats.connected, -1)
		atomic.AddInt64(&stats.disconnected, 1)
	}()

	// 启动消息接收
	recvCtx, recvCancel := context.WithCancel(ctx)
	go func() {
		for {
			select {
			case <-recvCtx.Done():
				return
			default:
				conn.SetReadDeadline(time.Now().Add(35 * time.Second))
				frame, err := recvFrame(conn)
				if err != nil {
					return
				}
				atomic.AddInt64(&stats.msgRecv, 1)
				_ = frame
			}
		}
	}()
	defer recvCancel()

	// 心跳和消息发送
	hbTicker := time.NewTicker(30 * time.Second)
	msgTicker := time.NewTicker(*msgInterval + time.Duration(rand.Intn(1000))*time.Millisecond)
	defer hbTicker.Stop()
	defer msgTicker.Stop()

	seq := uint64(100)

	for {
		select {
		case <-ctx.Done():
			return

		case <-hbTicker.C:
			payload, _ := msgpack.Marshal(map[string]interface{}{
				"room_id":   roomID,
				"timestamp": time.Now().UnixMilli(),
			})
			sendFrameQuiet(conn, &Frame{MsgType: MsgTypeHeartbeat, Seq: seq, Payload: payload})
			seq++
			atomic.AddInt64(&stats.msgSent, 1)

		case <-msgTicker.C:
			// 发送业务消息
			payload, _ := msgpack.Marshal(map[string]interface{}{
				"action":  "send_message",
				"content": fmt.Sprintf("Hello from %s at %d", userID, time.Now().Unix()),
			})
			sendFrameQuiet(conn, &Frame{MsgType: MsgTypeBizRequest, Seq: seq, Payload: payload})
			seq++
			atomic.AddInt64(&stats.msgSent, 1)
		}
	}
}

func doAuth(conn *websocket.Conn, userID string) error {
	payload, _ := msgpack.Marshal(map[string]interface{}{
		"token":   "dev_" + userID,
		"user_id": userID,
	})

	frame := &Frame{MsgType: MsgTypeAuth, Seq: 1, Payload: payload}
	if err := sendFrameQuiet(conn, frame); err != nil {
		return err
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := recvFrame(conn)
	if err != nil {
		return err
	}

	if resp.MsgType != MsgTypeAuthResp {
		return fmt.Errorf("unexpected response: 0x%04X", resp.MsgType)
	}

	var authResp struct {
		Success bool `msgpack:"success"`
	}
	msgpack.Unmarshal(resp.Payload, &authResp)

	if !authResp.Success {
		return fmt.Errorf("auth failed")
	}

	return nil
}

func doJoin(conn *websocket.Conn, roomID string) error {
	payload, _ := msgpack.Marshal(map[string]interface{}{
		"room_id": roomID,
	})

	frame := &Frame{MsgType: MsgTypeJoinRoom, Seq: 2, Payload: payload}
	if err := sendFrameQuiet(conn, frame); err != nil {
		return err
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := recvFrame(conn)
	if err != nil {
		return err
	}

	if resp.MsgType != MsgTypeJoinRoomResp {
		return fmt.Errorf("unexpected response: 0x%04X", resp.MsgType)
	}

	var joinResp struct {
		Success bool `msgpack:"success"`
	}
	msgpack.Unmarshal(resp.Payload, &joinResp)

	if !joinResp.Success {
		return fmt.Errorf("join failed")
	}

	return nil
}

func sendFrameQuiet(conn *websocket.Conn, f *Frame) error {
	data := encodeFrame(f)
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteMessage(websocket.BinaryMessage, data)
}

func statsLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Printf("Stats: connected=%d sent=%d recv=%d errors=%d",
				atomic.LoadInt64(&stats.connected),
				atomic.LoadInt64(&stats.msgSent),
				atomic.LoadInt64(&stats.msgRecv),
				atomic.LoadInt64(&stats.errors),
			)
		}
	}
}

func printFinalStats() {
	log.Printf("=== Final Stats ===")
	log.Printf("  Total Connected: %d", atomic.LoadInt64(&stats.connected))
	log.Printf("  Total Disconnected: %d", atomic.LoadInt64(&stats.disconnected))
	log.Printf("  Messages Sent: %d", atomic.LoadInt64(&stats.msgSent))
	log.Printf("  Messages Received: %d", atomic.LoadInt64(&stats.msgRecv))
	log.Printf("  Errors: %d", atomic.LoadInt64(&stats.errors))
}
