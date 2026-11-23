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
	"crypto/tls"
	"crypto/x509"
	"fmt"

	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

// NewClient builds a Lister that reads fulfillment's public
// ProjectMemberships API over TLS. The caller's bearer token is forwarded
// unchanged; the authorizer never holds a fulfillment credential of its own.
// The returned function closes the connection.
func NewClient(target string, roots *x509.CertPool) (Lister, func() error, error) {
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	})))
	if err != nil {
		return nil, nil, fmt.Errorf("build the fulfillment client: %w", err)
	}
	return clientFor(conn), conn.Close, nil
}

// clientFor adapts a connection to the Lister contract. The subject is part of
// that contract but is not sent: the public API answers for whoever the token
// authenticates.
func clientFor(conn grpc.ClientConnInterface) Lister {
	client := publicv1.NewProjectMembershipsClient(conn)
	return func(ctx context.Context, _, token string) ([]string, error) {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		response, err := client.List(ctx, &publicv1.ProjectMembershipsListRequest{})
		if err != nil {
			return nil, fmt.Errorf("list project memberships: %w", err)
		}
		projects := make([]string, 0, len(response.GetItems()))
		for _, item := range response.GetItems() {
			projects = append(projects, item.GetMetadata().GetProject())
		}
		return sortedUnique(projects), nil
	}
}
