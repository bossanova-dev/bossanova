// Package crontest provides the cron RPC fake shared by CLI command tests.
package crontest

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/daemontest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// CronFake implements only the cron CRUD methods used by CLI consumers.
type CronFake struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
	mu                 sync.RWMutex
	socket             string
	cronJobs           map[string]*pb.CronJob
	cronJobCounter     int
	createCronJobCalls []*pb.CreateCronJobRequest
	updateCronJobCalls []*pb.UpdateCronJobRequest
	deleteCronJobCalls []string
}

// New starts a fake for cron command tests.
func New(t *testing.T) *CronFake {
	t.Helper()
	f := NewHandler()
	f.socket = daemontest.Serve(t, f)
	return f
}

// NewHandler creates a cron fake for composition with another consumer fake.
func NewHandler() *CronFake { return &CronFake{cronJobs: make(map[string]*pb.CronJob)} }

// SocketPath returns the serving socket.
func (m *CronFake) SocketPath() string { return m.socket }

// AddCronJob supports the CLI consumer fixture.
func (m *CronFake) AddCronJob(job *pb.CronJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cronJobs[job.Id] = proto.CloneOf(job)
}

// CronJobs supports the CLI consumer fixture.
func (m *CronFake) CronJobs() map[string]*pb.CronJob {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*pb.CronJob, len(m.cronJobs))
	for k, v := range m.cronJobs {
		out[k] = proto.CloneOf(v)
	}
	return out
}

// CreateCronJobCalls supports the CLI consumer fixture.
func (m *CronFake) CreateCronJobCalls() []*pb.CreateCronJobRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.CreateCronJobRequest, len(m.createCronJobCalls))
	for i, req := range m.createCronJobCalls {
		out[i] = proto.CloneOf(req)
	}
	return out
}

// UpdateCronJobCalls supports the CLI consumer fixture.
func (m *CronFake) UpdateCronJobCalls() []*pb.UpdateCronJobRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.UpdateCronJobRequest, len(m.updateCronJobCalls))
	for i, req := range m.updateCronJobCalls {
		out[i] = proto.CloneOf(req)
	}
	return out
}

// DeleteCronJobCallCount supports the CLI consumer fixture.
func (m *CronFake) DeleteCronJobCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.deleteCronJobCalls)
}

// CreateCronJob supports the CLI consumer fixture.
func (m *CronFake) CreateCronJob(_ context.Context, req *connect.Request[pb.CreateCronJobRequest]) (*connect.Response[pb.CreateCronJobResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCronJobCalls = append(m.createCronJobCalls, proto.CloneOf(req.Msg))
	m.cronJobCounter++
	job := &pb.CronJob{
		Id:        fmt.Sprintf("cron-%d", m.cronJobCounter),
		RepoId:    req.Msg.RepoId,
		Name:      req.Msg.Name,
		Prompt:    req.Msg.Prompt,
		Schedule:  req.Msg.Schedule,
		Timezone:  req.Msg.Timezone,
		IsEnabled: req.Msg.IsEnabled,
		AgentName: req.Msg.AgentName,
		// Stored so a later ListCronJobs reflects the gate a caller created.
		GateCommand: req.Msg.GateCommand,
		// Stored so CLI tests see the config-ownership marker round-trip.
		ManagedBy: req.Msg.ManagedBy,
		CreatedAt: timestamppb.Now(),
		UpdatedAt: timestamppb.Now(),
	}
	m.cronJobs[job.Id] = proto.CloneOf(job)
	return connect.NewResponse(&pb.CreateCronJobResponse{CronJob: proto.CloneOf(job)}), nil
}

// ListCronJobs supports the CLI consumer fixture.
func (m *CronFake) ListCronJobs(_ context.Context, req *connect.Request[pb.ListCronJobsRequest]) (*connect.Response[pb.ListCronJobsResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.cronJobs))
	for id := range m.cronJobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*pb.CronJob, 0, len(ids))
	for _, id := range ids {
		job := m.cronJobs[id]
		if req.Msg.RepoId != nil && *req.Msg.RepoId != "" && job.RepoId != *req.Msg.RepoId {
			continue
		}
		// Clone so concurrent writers (e.g. UpdateCronJob) cannot race with
		// the response marshaler.
		out = append(out, proto.CloneOf(job))
	}
	return connect.NewResponse(&pb.ListCronJobsResponse{CronJobs: out}), nil
}

// GetCronJob supports the CLI consumer fixture.
func (m *CronFake) GetCronJob(_ context.Context, req *connect.Request[pb.GetCronJobRequest]) (*connect.Response[pb.GetCronJobResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.cronJobs[req.Msg.Id]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("cron job %q not found", req.Msg.Id))
	}
	return connect.NewResponse(&pb.GetCronJobResponse{CronJob: proto.CloneOf(job)}), nil
}

// UpdateCronJob supports the CLI consumer fixture.
func (m *CronFake) UpdateCronJob(_ context.Context, req *connect.Request[pb.UpdateCronJobRequest]) (*connect.Response[pb.UpdateCronJobResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCronJobCalls = append(m.updateCronJobCalls, proto.CloneOf(req.Msg))
	job, ok := m.cronJobs[req.Msg.Id]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("cron job %q not found", req.Msg.Id))
	}
	if req.Msg.Name != nil {
		job.Name = *req.Msg.Name
	}
	if req.Msg.Prompt != nil {
		job.Prompt = *req.Msg.Prompt
	}
	if req.Msg.Schedule != nil {
		job.Schedule = *req.Msg.Schedule
	}
	if req.Msg.Timezone != nil {
		job.Timezone = *req.Msg.Timezone
	}
	if req.Msg.IsEnabled != nil {
		job.IsEnabled = *req.Msg.IsEnabled
	}
	if req.Msg.AgentName != nil {
		job.AgentName = *req.Msg.AgentName
	}
	if req.Msg.GateCommand != nil {
		job.GateCommand = *req.Msg.GateCommand
	}
	// Absent leaves the marker alone; "" clears it, as bossd does.
	if req.Msg.ManagedBy != nil {
		job.ManagedBy = *req.Msg.ManagedBy
	}
	job.UpdatedAt = timestamppb.Now()
	// Clone before returning: connect-go marshals the response after we
	// release the lock, and a subsequent UpdateCronJob would otherwise mutate
	// the same pointer concurrently with the in-flight marshal.
	return connect.NewResponse(&pb.UpdateCronJobResponse{CronJob: proto.CloneOf(job)}), nil
}

// DeleteCronJob supports the CLI consumer fixture.
func (m *CronFake) DeleteCronJob(_ context.Context, req *connect.Request[pb.DeleteCronJobRequest]) (*connect.Response[pb.DeleteCronJobResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCronJobCalls = append(m.deleteCronJobCalls, req.Msg.Id)
	if _, ok := m.cronJobs[req.Msg.Id]; !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("cron job %q not found", req.Msg.Id))
	}
	delete(m.cronJobs, req.Msg.Id)
	return connect.NewResponse(&pb.DeleteCronJobResponse{}), nil
}
