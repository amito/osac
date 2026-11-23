/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package membership

import (
	"context"
	"net"
	"sync"
	"testing"

	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeMemberships is an in-process server: it records the authorization
// metadata it received and answers with a fixed List.
type fakeMemberships struct {
	publicv1.UnimplementedProjectMembershipsServer
	mu         sync.Mutex
	authorized []string
	items      []*publicv1.ProjectMembership
	err        error
}

func (f *fakeMemberships) List(ctx context.Context,
	_ *publicv1.ProjectMembershipsListRequest) (*publicv1.ProjectMembershipsListResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.mu.Lock()
	f.authorized = append(f.authorized, md.Get("authorization")...)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &publicv1.ProjectMembershipsListResponse{
		Size:  int32(len(f.items)),
		Total: int32(len(f.items)),
		Items: f.items,
	}, nil
}

func (f *fakeMemberships) tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.authorized...)
}

func membership(project string) *publicv1.ProjectMembership {
	return &publicv1.ProjectMembership{Metadata: &publicv1.Metadata{Project: project}}
}

func startMembershipsServer(t *testing.T, impl publicv1.ProjectMembershipsServer) *grpc.ClientConn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	server := grpc.NewServer()
	publicv1.RegisterProjectMembershipsServer(server, impl)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return conn
}

func TestClientForwardsTokenAndReadsProject(t *testing.T) {
	server := &fakeMemberships{items: []*publicv1.ProjectMembership{
		membership("team-b"),
		membership("team-a"),
		membership("team-a"),
	}}
	conn := startMembershipsServer(t, server)
	lister := clientFor(conn)
	projects, err := lister(context.Background(), "alice", "secret-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projects) != 2 || projects[0] != "team-a" || projects[1] != "team-b" {
		t.Errorf("expected sorted, de-duplicated projects, got %v", projects)
	}
	tokens := server.tokens()
	if len(tokens) != 1 || tokens[0] != "Bearer secret-token" {
		t.Errorf("the caller's token must be forwarded unchanged: %v", tokens)
	}
}

func TestClientErrorPropagates(t *testing.T) {
	server := &fakeMemberships{err: status.Error(codes.PermissionDenied, "not allowed")}
	conn := startMembershipsServer(t, server)
	if _, err := clientFor(conn)(context.Background(), "alice", "secret-token"); err == nil {
		t.Fatal("expected the server error")
	}
}
