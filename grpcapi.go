package transport

import (
	"context"
	"io"
	"time"

	pb "github.com/Jille/raft-grpc-transport/proto"
	"github.com/armon/go-metrics"
	"github.com/hashicorp/raft"
)

// These are requests incoming over gRPC that we need to relay to the Raft engine.

type gRPCAPI struct {
	manager *Manager

	// "Unsafe" to ensure compilation fails if new methods are added but not implemented
	pb.UnsafeRaftTransportServer
}

func (g gRPCAPI) handleRPC(command interface{}, data io.Reader) (interface{}, error) {
	ch := make(chan raft.RPCResponse, 1)
	rpc := raft.RPC{
		Command:  command,
		RespChan: ch,
		Reader:   data,
	}
	if isHeartbeat(command) {
		// We can take the fast path and use the heartbeat callback and skip the queue in g.manager.rpcChan.
		fn := func() func(raft.RPC) {
			t := time.Now()
			defer metrics.MeasureSinceWithLabels([]string{"raft", "replication", "heartbeat", "follower", "AppendEntries", "handleRPC", "getHeartbeatFunc"}, t, []metrics.Label{})
			g.manager.heartbeatFuncMtx.Lock()
			fn := g.manager.heartbeatFunc
			g.manager.heartbeatFuncMtx.Unlock()
			return fn
		}()
		if fn != nil {
			func() {
				t := time.Now()
				defer metrics.MeasureSinceWithLabels([]string{"raft", "replication", "heartbeat", "follower", "AppendEntries", "handleRPC", "execHeartbeatFunc"}, t, []metrics.Label{})
				fn(rpc)
			}()
			goto wait
		}
	}
	select {
	case g.manager.rpcChan <- rpc:
	case <-g.manager.shutdownCh:
		return nil, raft.ErrTransportShutdown
	}

wait:
	if isHeartbeat(command) {
		t := time.Now()
		defer metrics.MeasureSinceWithLabels([]string{"raft", "replication", "heartbeat", "follower", "AppendEntries", "handleRPC", "pushAppendEntriesResp"}, t, []metrics.Label{})
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Response, nil
	case <-g.manager.shutdownCh:
		return nil, raft.ErrTransportShutdown
	}
}

func (g gRPCAPI) AppendEntries(ctx context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
	dec := func() *raft.AppendEntriesRequest {
		t := time.Now()
		defer metrics.MeasureSinceWithLabels([]string{"raft", "replication", "heartbeat", "follower", "AppendEntries", "decodeAppendEntriesRequest"}, t, []metrics.Label{})
		return decodeAppendEntriesRequest(req)
	}()
	resp, err := func() (interface{}, error) {
		if isHeartbeat(dec) {
			t := time.Now()
			defer metrics.MeasureSinceWithLabels([]string{"raft", "replication", "heartbeat", "follower", "AppendEntries", "handleRPC"}, t, []metrics.Label{})
		}
		return g.handleRPC(dec, nil)
	}()
	if err != nil {
		return nil, err
	}
	enc := func() *pb.AppendEntriesResponse {
		if isHeartbeat(dec) {
			t := time.Now()
			defer metrics.MeasureSinceWithLabels([]string{"raft", "replication", "heartbeat", "follower", "AppendEntries", "encodeAppendEntriesResponse"}, t, []metrics.Label{})
		}
		return encodeAppendEntriesResponse(resp.(*raft.AppendEntriesResponse))
	}()
	return enc, nil
}

func (g gRPCAPI) RequestVote(ctx context.Context, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
	resp, err := g.handleRPC(decodeRequestVoteRequest(req), nil)
	if err != nil {
		return nil, err
	}
	return encodeRequestVoteResponse(resp.(*raft.RequestVoteResponse)), nil
}

func (g gRPCAPI) TimeoutNow(ctx context.Context, req *pb.TimeoutNowRequest) (*pb.TimeoutNowResponse, error) {
	resp, err := g.handleRPC(decodeTimeoutNowRequest(req), nil)
	if err != nil {
		return nil, err
	}
	return encodeTimeoutNowResponse(resp.(*raft.TimeoutNowResponse)), nil
}

func (g gRPCAPI) InstallSnapshot(s pb.RaftTransport_InstallSnapshotServer) error {
	isr, err := s.Recv()
	if err != nil {
		return err
	}
	resp, err := g.handleRPC(decodeInstallSnapshotRequest(isr), &snapshotStream{s, isr.GetData()})
	if err != nil {
		return err
	}
	return s.SendAndClose(encodeInstallSnapshotResponse(resp.(*raft.InstallSnapshotResponse)))
}

type snapshotStream struct {
	s pb.RaftTransport_InstallSnapshotServer

	buf []byte
}

func (s *snapshotStream) Read(b []byte) (int, error) {
	if len(s.buf) > 0 {
		n := copy(b, s.buf)
		s.buf = s.buf[n:]
		return n, nil
	}
	m, err := s.s.Recv()
	if err != nil {
		return 0, err
	}
	n := copy(b, m.GetData())
	if n < len(m.GetData()) {
		s.buf = m.GetData()[n:]
	}
	return n, nil
}

func (g gRPCAPI) AppendEntriesPipeline(s pb.RaftTransport_AppendEntriesPipelineServer) error {
	for {
		msg, err := s.Recv()
		if err != nil {
			return err
		}
		resp, err := g.handleRPC(decodeAppendEntriesRequest(msg), nil)
		if err != nil {
			// TODO(quis): One failure doesn't have to break the entire stream?
			// Or does it all go wrong when it's out of order anyway?
			return err
		}
		if err := s.Send(encodeAppendEntriesResponse(resp.(*raft.AppendEntriesResponse))); err != nil {
			return err
		}
	}
}

func isHeartbeat(command interface{}) bool {
	req, ok := command.(*raft.AppendEntriesRequest)
	if !ok {
		return false
	}
	return req.Term != 0 && len(req.Leader) != 0 && req.PrevLogEntry == 0 && req.PrevLogTerm == 0 && len(req.Entries) == 0 && req.LeaderCommitIndex == 0
}
