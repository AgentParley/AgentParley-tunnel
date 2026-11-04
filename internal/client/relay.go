package client

import (
	"context"
	"io"
	"log"
	"strconv"
	"time"

	tunnelpb "github.com/agentparley/tunnel/internal/proto"
	"github.com/agentparley/tunnel/internal/relay"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const refusalDrainTimeout = 10 * time.Second

// handleOpenRelay answers one open_relay: slot, policy, then dial or start the program, and only then opens the
// Relay call (the egress waits a short time for its first frame, a TCP dial can take longer).
func (d *Daemon) handleOpenRelay(streamCtx context.Context, connection *grpc.ClientConn, routingKey string, request *tunnelpb.OpenRelay) {
	relayID := request.GetRelayId()
	if relayID == "" {
		return
	}
	startedAt := time.Now()
	destination := describeDestination(request)

	conn, refusal := d.openRelayDestination(streamCtx, request)
	if refusal != nil {
		d.refuseRelay(streamCtx, connection, routingKey, relayID, refusal)
		log.Printf("relay %s: refused code=%s: %s", destination, refusal.Code, refusal.Message)
		return
	}
	defer func() { <-d.relaySlots }()
	defer conn.Close()

	relayCtx, cancel := context.WithCancel(streamCtx)
	defer cancel()
	stream, err := tunnelpb.NewTunnelServiceClient(connection).Relay(relayContext(relayCtx, routingKey))
	if err != nil {
		log.Printf("relay %s: could not open the relay call: %v", destination, err)
		return
	}
	if err := stream.Send(&tunnelpb.RelayFrame{Kind: &tunnelpb.RelayFrame_Start{Start: &tunnelpb.RelayStart{RelayId: relayID}}}); err != nil {
		log.Printf("relay %s: could not send start: %v", destination, err)
		return
	}

	result := relay.Pump(stream, conn, cancel)
	log.Printf("relay %s: closed code=%q to_destination=%dB from_destination=%dB duration=%s",
		destination, result.CloseCode, result.BytesToFar, result.BytesFromFar, time.Since(startedAt).Round(time.Millisecond))
}

// openRelayDestination takes a relay slot (kept only when a connection is returned) and opens the destination.
func (d *Daemon) openRelayDestination(ctx context.Context, request *tunnelpb.OpenRelay) (io.ReadWriteCloser, *relay.Error) {
	select {
	case d.relaySlots <- struct{}{}:
	default:
		return nil, &relay.Error{Code: relay.CodeRelayBusy, Message: "this computer has " + strconv.Itoa(cap(d.relaySlots)) + " relays open (relay.max_concurrent)"}
	}

	conn, refusal := d.dialRelayDestination(ctx, request)
	if refusal != nil {
		<-d.relaySlots
		return nil, refusal
	}
	return conn, nil
}

func (d *Daemon) dialRelayDestination(ctx context.Context, request *tunnelpb.OpenRelay) (io.ReadWriteCloser, *relay.Error) {
	switch destination := request.GetDestination().(type) {
	case *tunnelpb.OpenRelay_Tcp:
		host, port := destination.Tcp.GetHost(), int(destination.Tcp.GetPort())
		if host == "" || port < 1 || port > 65535 {
			return nil, &relay.Error{Code: relay.CodeInvalidRequest, Message: "the destination needs a host and a port from 1 to 65535"}
		}
		if err := d.relayPolicy.CheckAddress(host, port); err != nil {
			return nil, asRelayError(err)
		}
		conn, refusal := relay.DialTCP(ctx, host, port)
		if refusal != nil {
			return nil, refusal
		}
		return conn, nil
	case *tunnelpb.OpenRelay_Program:
		program, err := d.relayPolicy.Program(destination.Program)
		if err != nil {
			return nil, asRelayError(err)
		}
		started, refusal := relay.StartProgram(d.runAsUser, program, d.relayPrograms)
		if refusal != nil {
			return nil, refusal
		}
		return started, nil
	default:
		return nil, &relay.Error{Code: relay.CodeInvalidRequest, Message: "the request names no destination"}
	}
}

// refuseRelay opens the Relay call only to carry the reason in its first frame.
func (d *Daemon) refuseRelay(streamCtx context.Context, connection *grpc.ClientConn, routingKey, relayID string, refusal *relay.Error) {
	ctx, cancel := context.WithTimeout(streamCtx, refusalDrainTimeout)
	defer cancel()
	stream, err := tunnelpb.NewTunnelServiceClient(connection).Relay(relayContext(ctx, routingKey))
	if err != nil {
		return
	}
	start := &tunnelpb.RelayStart{RelayId: relayID, ErrorCode: refusal.Code, ErrorMessage: refusal.Message}
	if stream.Send(&tunnelpb.RelayFrame{Kind: &tunnelpb.RelayFrame_Start{Start: start}}) != nil {
		return
	}
	_ = stream.CloseSend()
	for {
		if _, err := stream.Recv(); err != nil {
			return
		}
	}
}

// relayContext carries only the routing header: the call is authorised by the relay_id, not a bearer token.
func relayContext(ctx context.Context, routingKey string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, RouteHeader, routingKey)
}

func asRelayError(err error) *relay.Error {
	if refusal, ok := err.(*relay.Error); ok {
		return refusal
	}
	return &relay.Error{Code: relay.CodeInternal, Message: err.Error()}
}

func describeDestination(request *tunnelpb.OpenRelay) string {
	switch destination := request.GetDestination().(type) {
	case *tunnelpb.OpenRelay_Tcp:
		return "tcp " + destination.Tcp.GetHost() + ":" + strconv.Itoa(int(destination.Tcp.GetPort()))
	case *tunnelpb.OpenRelay_Program:
		return "program " + destination.Program
	}
	return "unknown destination"
}
