package relay

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	tunnelpb "github.com/agentparley/tunnel/internal/proto"
)

const (
	maxFrameBytes   = 32 * 1024
	closeStreamWait = 10 * time.Second
)

// FrameStream is the daemon's end of a Relay call (tunnelpb.TunnelService_RelayClient satisfies it).
type FrameStream interface {
	Send(*tunnelpb.RelayFrame) error
	Recv() (*tunnelpb.RelayFrame, error)
	CloseSend() error
}

// Result is how a pipe ended and how much it carried.
type Result struct {
	CloseCode    string
	CloseMessage string
	BytesToFar   int64 // from AgentParley to the destination
	BytesFromFar int64
}

type exitReporter interface{ ExitMessage() string }

// Pump copies bytes between the destination and the Relay call until either side ends. cancel ends the call when
// the far side does not answer our close in time.
func Pump(stream FrameStream, conn io.ReadWriteCloser, cancel context.CancelFunc) Result {
	var result Result
	var toFar, fromFar atomic.Int64
	var closeMutex sync.Mutex

	var wait sync.WaitGroup
	wait.Add(2)

	go func() {
		defer wait.Done()
		buffer := make([]byte, maxFrameBytes)
		for {
			count, err := conn.Read(buffer)
			if count > 0 {
				fromFar.Add(int64(count))
				if sendErr := stream.Send(&tunnelpb.RelayFrame{Kind: &tunnelpb.RelayFrame_Data{Data: append([]byte(nil), buffer[:count]...)}}); sendErr != nil {
					conn.Close()
					return
				}
			}
			if err != nil {
				closeMutex.Lock()
				code, message := "", ""
				if reporter, ok := conn.(exitReporter); ok {
					code, message = CodeProgramExited, reporter.ExitMessage()
				}
				result.CloseCode, result.CloseMessage = code, message
				closeMutex.Unlock()
				_ = stream.Send(&tunnelpb.RelayFrame{Kind: &tunnelpb.RelayFrame_Close{Close: &tunnelpb.RelayClose{Code: code, Message: message}}})
				_ = stream.CloseSend()
				time.AfterFunc(closeStreamWait, cancel)
				return
			}
		}
	}()

	go func() {
		defer wait.Done()
		defer conn.Close()
		for {
			frame, err := stream.Recv()
			if err != nil {
				return
			}
			switch kind := frame.GetKind().(type) {
			case *tunnelpb.RelayFrame_Data:
				if _, writeErr := conn.Write(kind.Data); writeErr != nil {
					return
				}
				toFar.Add(int64(len(kind.Data)))
			case *tunnelpb.RelayFrame_Close:
				return
			}
		}
	}()

	wait.Wait()
	closeMutex.Lock()
	defer closeMutex.Unlock()
	result.BytesToFar, result.BytesFromFar = toFar.Load(), fromFar.Load()
	return result
}
