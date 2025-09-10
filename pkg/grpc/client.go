// Copyright Contributors to the Open Cluster Management project

package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"time"

	"github.com/stolostron/search-collector/api/proto"
	tr "github.com/stolostron/search-collector/pkg/transforms"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"
)

// GRPCClient wraps the search indexer gRPC client
type GRPCClient struct {
	client proto.SearchIndexerClient
	conn   *grpc.ClientConn
}

// NewGRPCClient creates a new gRPC client for the search indexer
func NewGRPCClient(aggregatorURL string, deployedInHub bool) (*GRPCClient, error) {
	// Configure TLS credentials
	var creds credentials.TransportCredentials

	if deployedInHub {
		// Hub deployment: Use TLS certificates if available
		_, err := os.Stat("./sslcert/tls.crt")
		if err != nil {
			klog.Warning("TLS certificates not found, using insecure connection for gRPC")
			creds = insecure.NewCredentials()
		} else {
			cert, err := tls.LoadX509KeyPair("./sslcert/tls.crt", "./sslcert/tls.key")
			if err != nil {
				klog.Warningf("Failed to load TLS certificates, using insecure connection: %v", err)
				creds = insecure.NewCredentials()
			} else {
				tlsConfig := &tls.Config{
					Certificates: []tls.Certificate{cert},
					MinVersion:   tls.VersionTLS12,
				}
				creds = credentials.NewTLS(tlsConfig)
			}
		}
	} else {
		// Klusterlet deployment: Use insecure for now (can be enhanced with proper cert handling)
		creds = insecure.NewCredentials()
	}

	// Establish gRPC connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, aggregatorURL,
		grpc.WithTransportCredentials(creds),
		grpc.WithBlock(), // Wait for connection to be ready
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to gRPC server at %s: %w", aggregatorURL, err)
	}

	client := proto.NewSearchIndexerClient(conn)

	return &GRPCClient{
		client: client,
		conn:   conn,
	}, nil
}

// Close closes the gRPC connection
func (c *GRPCClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Sync sends resources to the search indexer via gRPC
func (c *GRPCClient) Sync(ctx context.Context, clusterID string, payload CollectorPayload) (*SyncResponse, error) {
	// Convert collector payload to gRPC request
	grpcRequest := &proto.SyncRequest{
		ClusterId:       clusterID,
		OverwriteState:  payload.ClearAll,
		AddResources:    convertResourcesToProto(payload.AddResources),
		UpdateResources: convertResourcesToProto(payload.UpdatedResources),
		DeleteResources: convertDeleteResourcesToProto(payload.DeletedResources),
		AddEdges:        convertEdgesToProto(payload.AddEdges),
		DeleteEdges:     convertEdgesToProto(payload.DeleteEdges),
	}

	// Make gRPC call
	grpcResponse, err := c.client.Sync(ctx, grpcRequest)
	if err != nil {
		return nil, fmt.Errorf("gRPC sync failed: %w", err)
	}

	// Convert gRPC response to collector response
	response := &SyncResponse{
		TotalAdded:        int(grpcResponse.GetTotalAdded()),
		TotalUpdated:      int(grpcResponse.GetTotalUpdated()),
		TotalDeleted:      int(grpcResponse.GetTotalDeleted()),
		TotalResources:    int(grpcResponse.GetTotalResources()),
		TotalEdgesAdded:   int(grpcResponse.GetTotalEdgesAdded()),
		TotalEdgesDeleted: int(grpcResponse.GetTotalEdgesDeleted()),
		TotalEdges:        int(grpcResponse.GetTotalEdges()),
		Version:           grpcResponse.GetVersion(),
		AddErrors:         convertSyncErrorsFromProto(grpcResponse.GetAddErrors()),
		UpdateErrors:      convertSyncErrorsFromProto(grpcResponse.GetUpdateErrors()),
		DeleteErrors:      convertSyncErrorsFromProto(grpcResponse.GetDeleteErrors()),
		AddEdgeErrors:     convertSyncErrorsFromProto(grpcResponse.GetAddEdgeErrors()),
		DeleteEdgeErrors:  convertSyncErrorsFromProto(grpcResponse.GetDeleteEdgeErrors()),
	}

	return response, nil
}

// Health performs a health check via gRPC
func (c *GRPCClient) Health(ctx context.Context) error {
	_, err := c.client.Health(ctx, &proto.HealthRequest{})
	return err
}

// CollectorPayload mirrors the send.Payload struct but for internal use
type CollectorPayload struct {
	DeletedResources []tr.Deletion `json:"deleteResources,omitempty"`
	AddResources     []tr.Node     `json:"addResources,omitempty"`
	UpdatedResources []tr.Node     `json:"updateResources,omitempty"`
	AddEdges         []tr.Edge     `json:"addEdges,omitempty"`
	DeleteEdges      []tr.Edge     `json:"deleteEdges,omitempty"`
	ClearAll         bool          `json:"clearAll,omitempty"`
	Version          string        `json:"version,omitempty"`
}

// SyncResponse mirrors the send.SyncResponse struct
type SyncResponse struct {
	TotalAdded        int
	TotalUpdated      int
	TotalDeleted      int
	TotalResources    int
	TotalEdgesAdded   int
	TotalEdgesDeleted int
	TotalEdges        int
	AddErrors         []SyncError
	UpdateErrors      []SyncError
	DeleteErrors      []SyncError
	AddEdgeErrors     []SyncError
	DeleteEdgeErrors  []SyncError
	Version           string
}

// SyncError mirrors the send.SyncError struct
type SyncError struct {
	ResourceUID string
	Message     string
}

// Helper functions to convert between collector types and protobuf types

func convertResourcesToProto(nodes []tr.Node) []*proto.Resource {
	resources := make([]*proto.Resource, len(nodes))
	for i, node := range nodes {
		// Convert properties from map[string]interface{} to map[string]string
		properties := make(map[string]string)
		var kind string

		for k, v := range node.Properties {
			if str, ok := v.(string); ok {
				properties[k] = str
			} else {
				properties[k] = fmt.Sprintf("%v", v)
			}
			// Extract kind from properties
			if k == "kind" {
				if kindStr, ok := v.(string); ok {
					kind = kindStr
				}
			}
		}

		resources[i] = &proto.Resource{
			Kind:           kind,
			Uid:            node.UID,
			ResourceString: node.ResourceString,
			Properties:     properties,
		}
	}
	return resources
}

func convertDeleteResourcesToProto(deletions []tr.Deletion) []*proto.DeleteResourceEvent {
	events := make([]*proto.DeleteResourceEvent, len(deletions))
	for i, deletion := range deletions {
		events[i] = &proto.DeleteResourceEvent{
			Uid: deletion.UID,
		}
	}
	return events
}

func convertEdgesToProto(edges []tr.Edge) []*proto.Edge {
	protoEdges := make([]*proto.Edge, len(edges))
	for i, edge := range edges {
		protoEdges[i] = &proto.Edge{
			SourceUid:  edge.SourceUID,
			DestUid:    edge.DestUID,
			EdgeType:   string(edge.EdgeType), // Convert EdgeType to string
			SourceKind: edge.SourceKind,
			DestKind:   edge.DestKind,
		}
	}
	return protoEdges
}

func convertSyncErrorsFromProto(protoErrors []*proto.SyncError) []SyncError {
	errors := make([]SyncError, len(protoErrors))
	for i, protoError := range protoErrors {
		errors[i] = SyncError{
			ResourceUID: protoError.GetResourceUid(),
			Message:     protoError.GetMessage(),
		}
	}
	return errors
}
