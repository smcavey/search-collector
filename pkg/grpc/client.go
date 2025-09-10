// Copyright Contributors to the Open Cluster Management project

package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/stolostron/search-collector/api/proto"
	"github.com/stolostron/search-collector/pkg/config"
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
		// Hub deployment: Use TLS for secure communication with search-indexer
		// For service-to-service communication, we trust the server's certificate
		// but don't need to provide client certificates
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true, // Skip certificate verification for service-to-service in same cluster
			MinVersion:         tls.VersionTLS12,
		}
		creds = credentials.NewTLS(tlsConfig)
		klog.V(2).Info("Using TLS connection for gRPC with InsecureSkipVerify=true")
	} else {
		// Klusterlet deployment: Use insecure for now (can be enhanced with proper cert handling)
		creds = insecure.NewCredentials()
		klog.V(2).Info("Using insecure gRPC connection for klusterlet deployment")
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
// Automatically chooses between streaming and non-streaming based on payload size
func (c *GRPCClient) Sync(ctx context.Context, clusterID string, payload CollectorPayload) (*SyncResponse, error) {
	// Calculate total resources to determine if we should use streaming
	totalResources := len(payload.AddResources) + len(payload.UpdatedResources) + len(payload.DeletedResources)
	totalEdges := len(payload.AddEdges) + len(payload.DeleteEdges)
	chunkSize := config.Cfg.GRPCChunkSize

	// Use streaming if payload is large or if explicitly doing a full resync
	if totalResources > chunkSize || totalEdges > chunkSize || payload.ClearAll {
		klog.V(2).Infof("Using streaming gRPC for large payload: resources=%d, edges=%d, clearAll=%t", 
			totalResources, totalEdges, payload.ClearAll)
		return c.StreamSync(ctx, clusterID, payload)
	}

	// Use regular sync for smaller payloads
	return c.syncRegular(ctx, clusterID, payload)
}

// syncRegular sends resources using the original non-streaming gRPC method
func (c *GRPCClient) syncRegular(ctx context.Context, clusterID string, payload CollectorPayload) (*SyncResponse, error) {
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

	return convertSyncResponseFromProto(grpcResponse), nil
}

// StreamSync sends resources to the search indexer via streaming gRPC
func (c *GRPCClient) StreamSync(ctx context.Context, clusterID string, payload CollectorPayload) (*SyncResponse, error) {
	// Generate session ID
	sessionID := uuid.New().String()
	
	// Create streaming client
	stream, err := c.client.StreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create stream: %w", err)
	}

	// Send chunks
	chunkSize := config.Cfg.GRPCChunkSize
	
	// Process all data types into chunks
	chunks := c.createChunks(sessionID, clusterID, payload, chunkSize)
	
	klog.V(2).Infof("Sending %d chunks for session %s", len(chunks), sessionID)
	
	// Send all chunks
	for i, chunk := range chunks {
		chunk.IsFirstChunk = (i == 0)
		chunk.IsLastChunk = (i == len(chunks)-1)
		
		if err := stream.Send(chunk); err != nil {
			return nil, fmt.Errorf("failed to send chunk %d: %w", i, err)
		}
		
		klog.V(3).Infof("Sent chunk %d/%d for session %s", i+1, len(chunks), sessionID)
	}

	// Close and receive response
	grpcResponse, err := stream.CloseAndRecv()
	if err != nil {
		return nil, fmt.Errorf("failed to close stream and receive response: %w", err)
	}

	klog.V(2).Infof("Completed streaming sync for session %s", sessionID)
	
	return convertSyncResponseFromProto(grpcResponse), nil
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

// createChunks splits the payload into multiple chunks for streaming
func (c *GRPCClient) createChunks(sessionID, clusterID string, payload CollectorPayload, chunkSize int) []*proto.SyncChunk {
	var chunks []*proto.SyncChunk
	
	// Convert all resources to proto format first
	addResources := convertResourcesToProto(payload.AddResources)
	updateResources := convertResourcesToProto(payload.UpdatedResources)
	deleteResources := convertDeleteResourcesToProto(payload.DeletedResources)
	addEdges := convertEdgesToProto(payload.AddEdges)
	deleteEdges := convertEdgesToProto(payload.DeleteEdges)
	
	// Calculate total items and distribute across chunks
	totalItems := len(addResources) + len(updateResources) + len(deleteResources) + len(addEdges) + len(deleteEdges)
	
	if totalItems == 0 {
		// Create single empty chunk for heartbeat
		chunks = append(chunks, &proto.SyncChunk{
			SessionId:     sessionID,
			ClusterId:     clusterID,
			OverwriteState: payload.ClearAll,
		})
		return chunks
	}
	
	// Create chunks by distributing resources evenly
	currentChunk := &proto.SyncChunk{
		SessionId:     sessionID,
		ClusterId:     clusterID,
		OverwriteState: payload.ClearAll,
	}
	currentSize := 0
	
	// Helper function to add current chunk and start new one
	addChunk := func() {
		if currentSize > 0 {
			chunks = append(chunks, currentChunk)
			currentChunk = &proto.SyncChunk{
				SessionId:     sessionID,
				ClusterId:     clusterID,
				OverwriteState: payload.ClearAll,
			}
			currentSize = 0
		}
	}
	
	// Add resources in order: add, update, delete, addEdges, deleteEdges
	for _, resource := range addResources {
		if currentSize >= chunkSize {
			addChunk()
		}
		currentChunk.AddResources = append(currentChunk.AddResources, resource)
		currentSize++
	}
	
	for _, resource := range updateResources {
		if currentSize >= chunkSize {
			addChunk()
		}
		currentChunk.UpdateResources = append(currentChunk.UpdateResources, resource)
		currentSize++
	}
	
	for _, resource := range deleteResources {
		if currentSize >= chunkSize {
			addChunk()
		}
		currentChunk.DeleteResources = append(currentChunk.DeleteResources, resource)
		currentSize++
	}
	
	for _, edge := range addEdges {
		if currentSize >= chunkSize {
			addChunk()
		}
		currentChunk.AddEdges = append(currentChunk.AddEdges, edge)
		currentSize++
	}
	
	for _, edge := range deleteEdges {
		if currentSize >= chunkSize {
			addChunk()
		}
		currentChunk.DeleteEdges = append(currentChunk.DeleteEdges, edge)
		currentSize++
	}
	
	// Add final chunk if it has content
	if currentSize > 0 {
		chunks = append(chunks, currentChunk)
	}
	
	return chunks
}

// Helper functions to convert between collector types and protobuf types

func convertSyncResponseFromProto(grpcResponse *proto.SyncResponse) *SyncResponse {
	return &SyncResponse{
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
}

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
