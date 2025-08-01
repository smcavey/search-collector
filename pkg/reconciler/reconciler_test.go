/*
IBM Confidential
OCO Source Materials
(C) Copyright IBM Corporation 2019 All Rights Reserved
The source code for this program is not published or otherwise divested of its trade secrets,
irrespective of what has been deposited with the U.S. Copyright Office.
Copyright (c) 2020 Red Hat, Inc.
*/
// Copyright Contributors to the Open Cluster Management project

package reconciler

import (
	"log"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	lru "github.com/golang/groupcache/lru"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stolostron/search-collector/pkg/config"
	"github.com/stolostron/search-collector/pkg/metrics"
	tr "github.com/stolostron/search-collector/pkg/transforms"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/helm/pkg/proto/hapi/release"
	"k8s.io/klog/v2"
)

type NodeEdge struct {
	BuildNode  []tr.Node
	BuildEdges []func(tr.NodeStore) []tr.Edge
}

func initTestReconciler() *Reconciler {
	return &Reconciler{
		currentNodes:       make(map[string]tr.Node),
		previousNodes:      make(map[string]tr.Node),
		diffNodes:          make(map[string]tr.NodeEvent),
		k8sEventNodes:      make(map[string]tr.NodeEvent),
		previousEventEdges: make(map[string]tr.Edge),
		edgeFuncs:          make(map[string]func(ns tr.NodeStore) []tr.Edge),

		Input:       make(chan tr.NodeEvent),
		purgedNodes: lru.New(CACHE_SIZE),
	}
}

func createNodeEvents(resourceNameOne string, resourceNameTwo string) []tr.NodeEvent {
	events := NodeEdge{}
	nodeEvents := []tr.NodeEvent{}
	// First Node
	unstructuredInput := unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind": "testowner",
			"metadata": map[string]interface{}{
				"uid":  "1234",
				"name": "testownerName",
			},
		},
	}
	unstructuredNode := tr.GenericResourceBuilder(&unstructuredInput)
	bEdges := tr.GenericResourceBuilder(&unstructuredInput).BuildEdges
	node := unstructuredNode.BuildNode()
	node.ResourceString = resourceNameOne
	events.BuildNode = append(events.BuildNode, node)
	events.BuildEdges = append(events.BuildEdges, bEdges)

	// Second Node
	p := v1.Pod{}
	p.APIVersion = "v1"
	p.Name = "testpod"
	p.Kind = "Pod"
	p.Namespace = "default"
	p.UID = "5678"
	podNode := tr.PodResourceBuilder(&p, &unstructured.Unstructured{}).BuildNode()
	podNode.Metadata["OwnerUID"] = "local-cluster/1234"
	podNode.ResourceString = resourceNameTwo
	podEdges := tr.PodResourceBuilder(&p, &unstructured.Unstructured{}).BuildEdges

	events.BuildNode = append(events.BuildNode, podNode)
	events.BuildEdges = append(events.BuildEdges, podEdges)

	// Convert events to node events
	for i := range events.BuildNode {
		ne := tr.NodeEvent{
			Time:         time.Now().Unix(),
			Operation:    tr.Create,
			Node:         events.BuildNode[i],
			ComputeEdges: events.BuildEdges[i],
		}
		nodeEvents = append(nodeEvents, ne)
	}
	return nodeEvents
}

func createAndReconcileNodeEvents(testReconciler *Reconciler, nodeEventNameOne string, nodeEventNameTwo string) {
	events := createNodeEvents(nodeEventNameOne, nodeEventNameTwo)

	// Input node events to reconciler
	go func() {
		for _, ne := range events {
			testReconciler.Input <- ne
		}
	}()

	for range events {
		testReconciler.reconcileNode()
	}
}

func setupMetricsRegistry() {
	// reset metrics and register new ones to ensure metrics don't carry over values between tests
	metrics.EventsReceivedCount.Reset()
	metrics.ResourcesSentToIndexerCount.Reset()
	registry := prometheus.NewRegistry()
	metrics.PromRegistry = registry
	registry.MustRegister(metrics.EventsReceivedCount)
	registry.MustRegister(metrics.ResourcesSentToIndexerCount)
}

func TestReconcilerIncrementsResourcesMetrics(t *testing.T) {
	// Given: Reconciler and two unique node events
	setupMetricsRegistry()
	testReconciler := initTestReconciler()
	createAndReconcileNodeEvents(testReconciler, "uniqueNameOne", "uniqueNameTwo")

	// When: we collect the metrics
	collectedMetrics, _ := metrics.PromRegistry.Gather() // use the prometheus registry to confirm metrics have been scraped.

	// Then: we get both metrics and they are incremented appropriately - everything is unique so everything gets incremented
	assert.Equal(t, 2, len(collectedMetrics))
	// EventsReceivedCount - resource_kind = uniqueNameOne, value: 1
	assert.Equal(t, 1.0, collectedMetrics[0].GetMetric()[0].GetCounter().GetValue())
	// EventsReceivedCount - resource_kind = uniqueNameTwo, value: 1
	assert.Equal(t, 1.0, collectedMetrics[0].GetMetric()[1].GetCounter().GetValue())
	// ResourcesSentToIndexerCount - resource_kind = uniqueNameOne, value: 1
	assert.Equal(t, 1.0, collectedMetrics[1].GetMetric()[0].GetCounter().GetValue())
	// ResourcesSentToIndexerCount - resource_kind = uniqueNameTwo, value: 1
	assert.Equal(t, 1.0, collectedMetrics[1].GetMetric()[1].GetCounter().GetValue())
}

func TestReconcilerResourcesMetricSentToIndexerDiff(t *testing.T) {
	// Given: Reconciler and four resources (2 duplicates)
	setupMetricsRegistry()
	testReconciler := initTestReconciler()
	// we create and reconcile the same node events so EventsReceivedCount should be 2x ResourcesSentToIndexerCount
	createAndReconcileNodeEvents(testReconciler, "uniqueNameOne", "uniqueNameTwo")
	// we have to resetDiffs to mock a send and populate previous nodes
	testReconciler.mutex.Lock()
	testReconciler.resetDiffs()
	testReconciler.mutex.Unlock()
	createAndReconcileNodeEvents(testReconciler, "uniqueNameOne", "uniqueNameTwo")

	// When: we collect the metrics
	collectedMetrics, _ := metrics.PromRegistry.Gather() // use the prometheus registry to confirm metrics have been scraped.

	// Then: we get both metrics -EventsReceivedCount should be 2x ResourcesSentToIndexerCount because it processed duplicate node events
	// Reconciler drops the duplicate node events because they contain no changes
	assert.Equal(t, 2, len(collectedMetrics))
	// EventsReceivedCount - resource_kind = uniqueNameOne, value: 2
	assert.Equal(t, 2.0, collectedMetrics[0].GetMetric()[0].GetCounter().GetValue())
	// EventsReceivedCount - resource_kind = uniqueNameTwo, value: 2
	assert.Equal(t, 2.0, collectedMetrics[0].GetMetric()[1].GetCounter().GetValue())
	// ResourcesSentToIndexerCount - resource_kind = uniqueNameOne, value: 1
	assert.Equal(t, 1.0, collectedMetrics[1].GetMetric()[0].GetCounter().GetValue())
	// ResourcesSentToIndexerCount - resource_kind = uniqueNameTwo, value: 1
	assert.Equal(t, 1.0, collectedMetrics[1].GetMetric()[1].GetCounter().GetValue())
}

func TestReconcilerOutOfOrderDelete(t *testing.T) {
	s := initTestReconciler()
	ts := time.Now().Unix()

	go func() {
		s.Input <- tr.NodeEvent{
			Time:      ts,
			Operation: tr.Delete,
			Node: tr.Node{
				UID: "test-event",
			},
		}

		s.Input <- tr.NodeEvent{
			Time:      ts - 1000, // insert out of order based off of time
			Operation: tr.Create,
			Node: tr.Node{
				UID: "test-event",
			},
		}
	}()

	// need two calls to drain the queue
	s.reconcileNode()
	s.reconcileNode()

	if _, found := s.currentNodes["test-event"]; found {
		t.Fatal("failed to ignore add event received out of order")
	}

	if _, found := s.purgedNodes.Get("test-event"); !found {
		t.Fatal("failed to added deleted NodeEvent to purgedNodes cache")
	}
}

func TestReconcilerOutOfOrderAdd(t *testing.T) {
	s := initTestReconciler()
	ts := time.Now().Unix()

	go func() {
		s.Input <- tr.NodeEvent{
			Time:      ts,
			Operation: tr.Create,
			Node: tr.Node{
				UID: "test-event",
			},
		}

		s.Input <- tr.NodeEvent{
			Time:      ts - 1000, // insert out of order based off of time
			Operation: tr.Create,
			Node: tr.Node{
				UID: "test-event",
				Properties: map[string]interface{}{
					"staleData": true,
				},
			},
		}
	}()

	// need two calls to drain the queue
	s.reconcileNode()
	s.reconcileNode()

	testNode, ok := s.currentNodes["test-event"]
	if !ok {
		t.Fatal("failed to add test node to current state")
	}

	if _, ok := testNode.Properties["staleData"]; ok {
		t.Fatal("inserted nodes out of order: found stale data")
	}
}

func TestReconcilerAddDelete(t *testing.T) {
	s := initTestReconciler()

	go func() {
		s.Input <- tr.NodeEvent{
			Time:      time.Now().Unix(),
			Operation: tr.Create,
			Node: tr.Node{
				UID: "test-event",
			},
		}
	}()

	s.reconcileNode()

	if _, ok := s.currentNodes["test-event"]; !ok {
		t.Fatal("failed to add test event to current state")
	}
	if _, ok := s.diffNodes["test-event"]; !ok {
		t.Fatal("failed to add test event to diff state")
	}

	go func() {
		s.Input <- tr.NodeEvent{
			Time:      time.Now().Unix(),
			Operation: tr.Delete,
			Node: tr.Node{
				UID: "test-event",
			},
		}
	}()

	s.reconcileNode()

	if _, ok := s.currentNodes["test-event"]; ok {
		t.Fatal("failed to remove test event from current state")
	}
	if _, ok := s.diffNodes["test-event"]; ok {
		t.Fatal("failed to remove test event from diff state")
	}
}

func TestReconcilerRedundant(t *testing.T) {
	s := initTestReconciler()
	s.previousNodes["test-event"] = tr.Node{
		UID: "test-event",
		Properties: map[string]interface{}{
			"very": "important",
		},
	}

	go func() {
		s.Input <- tr.NodeEvent{
			Time:      time.Now().Unix(),
			Operation: tr.Create,
			Node: tr.Node{
				UID: "test-event",
				Properties: map[string]interface{}{
					"very": "important",
				},
			},
		}
	}()

	s.reconcileNode()

	if _, ok := s.diffNodes["test-event"]; ok {
		t.Fatal("failed to ignore redundant add event")
	}
}

func TestReconcilerAddEdges(t *testing.T) {
	// Establish the config
	config.InitConfig()

	testReconciler := initTestReconciler()

	createAndReconcileNodeEvents(testReconciler, "", "")

	// Build edges
	edgeMap1 := testReconciler.allEdges()

	// Expected edge
	edgeMap2 := make(map[string]map[string]tr.Edge, 1)
	edge := tr.Edge{EdgeType: "ownedBy", SourceUID: "local-cluster/5678", DestUID: "local-cluster/1234", SourceKind: "Pod", DestKind: "testowner"}
	edgeMap2["local-cluster/5678"] = map[string]tr.Edge{}
	edgeMap2["local-cluster/5678"]["local-cluster/1234"] = edge

	// Check if the actual and expected edges are the same
	if !reflect.DeepEqual(edgeMap1, edgeMap2) {
		t.Fatal("Expected edges not found")
	} else {
		t.Log("Expected edges found")
	}
}

func TestReconcilerDiff(t *testing.T) {
	testReconciler := initTestReconciler()
	// Add a node to reconciler previous nodes
	testReconciler.previousNodes["local-cluster/1234"] = tr.Node{
		UID: "local-cluster/1234",
		Properties: map[string]interface{}{
			"very": "important",
		},
	}

	createAndReconcileNodeEvents(testReconciler, "", "")

	// Compute reconciler diff - this time there should be 1 node and edge to add, 1 node to update
	diff := testReconciler.Diff()
	// Compute reconciler diff again - this time there shouldn't be any new edges or nodes to add/update
	nextDiff := testReconciler.Diff()

	if (len(diff.AddNodes) != 1 || len(diff.UpdateNodes) != 1 || len(diff.AddEdges) != 1) ||
		(len(nextDiff.AddNodes) != 0 || len(nextDiff.UpdateNodes) != 0 || len(nextDiff.AddEdges) != 0) {
		t.Fatal("Error: Reconciler Diff() not working as expected")
	} else {
		t.Log("Reconciler Diff() working as expected")
	}
}

func TestReconcilerComplete(t *testing.T) {
	input := make(chan *tr.Event)
	output := make(chan tr.NodeEvent)
	ts := time.Now().Unix()
	// Read all files in test-data
	dir := "../../test-data"
	files, err := os.ReadDir(dir)
	if err != nil {
		log.Fatal(err)
	}

	events := make([]tr.Event, 0)
	var appInput unstructured.Unstructured

	// Variables to keep track of helm release object
	var c v1.ConfigMap
	var rls release.Release
	rlsFileCount := 0
	rlsEvnt := &tr.Event{}

	// Convert to events
	for _, file := range files {
		filePath := dir + "/" + file.Name()
		if strings.HasSuffix(file.Name(), "updated.json") {
			tr.UnmarshalFile(filePath, &appInput, t)
			appInputLocal := appInput
			in := &tr.Event{
				Time:      ts,
				Operation: tr.Update,
				Resource:  &appInputLocal,
			}
			events = append(events, *in)
		} else if file.Name() != "helmrelease-release.json" {
			tr.UnmarshalFile(filePath, &appInput, t)
			appInputLocal := appInput
			in := &tr.Event{
				Time:      ts - 1,
				Operation: tr.Create,
				Resource:  &appInputLocal,
			}
			// This will process one of the helmrelease files - the helmrelease configmap and store the results
			if file.Name() == "helmrelease-configmap.json" {
				tr.UnmarshalFile(filePath, &c, t)
				rlsFileCount++
				rlsEvnt = in
				continue
			}
			events = append(events, *in)
		} else if file.Name() == "helmrelease-release.json" {
			tr.UnmarshalFile(filePath, &rls, t)
			rlsFileCount++
			continue
		}
	}
	testReconciler := initTestReconciler()
	go tr.TransformRoutine(input, output)

	// Convert events to Node events
	go func() {
		for _, ev := range events {
			localEv := &ev
			input <- localEv
			actual := <-output
			testReconciler.Input <- actual
		}
	}()

	for _, ev := range events {
		if ev.Operation == tr.Update {
			testReconciler.mutex.Lock()
			testReconciler.resetDiffs()
			testReconciler.mutex.Unlock()
		}
		testReconciler.reconcileNode()
	}
	// The rlsFileCount will ensure that both the release configmap and the helm release files are read - so that the release event can be added to reconciler
	if rlsFileCount == 2 {
		releaseTrans := tr.HelmReleaseResource{ConfigMap: &c, Release: &rls}
		go func() {
			testReconciler.Input <- tr.NewNodeEvent(rlsEvnt, releaseTrans, "releases")
		}()
		testReconciler.reconcileNode()
	}

	// Compute reconciler Complete() state
	com := testReconciler.Complete()

	ns := tr.NodeStore{
		ByUID:               testReconciler.currentNodes,
		ByKindNamespaceName: nodeTripleMap(testReconciler.currentNodes),
	}

	// Checks the count of nodes and edges based on the JSON files in pkg/test-data
	// Update counts when the test data is changed
	// We don't create Nodes for kind = Event
	const Nodes = 56
	const Edges = 61
	if len(com.Edges) != Edges || com.TotalEdges != Edges || len(com.Nodes) != Nodes || com.TotalNodes != Nodes {
		klog.Infof("len edges: %d", len(com.Edges))
		for _, edge := range com.Edges {
			klog.Info("Src: ", ns.ByUID[edge.SourceUID].Properties["kind"], " Type: ", edge.EdgeType, " Dest: ", ns.ByUID[edge.DestUID].Properties["kind"])
		}

		t.Log("Expected "+strconv.Itoa(Nodes)+" nodes, but found ", len(com.Nodes))
		t.Log("Expected "+strconv.Itoa(Edges)+" edges, but found ", len(com.Edges))
		t.Fatalf("Error: Reconciler Complete() not working as expected.")
	} else {
		t.Log("Correct number of edges and nodes")
	}

	// Verify some properties are set during BuildEdges on ConfigurationPolicies
	configPolNode := ns.ByKindNamespaceName["ConfigurationPolicy"]["local-cluster"]["policy-namespace"]

	missing := configPolNode.Properties["_missingResources"]
	if missing != `[{"v":"v1","k":"Namespace","n":"nonexistent"}]` {
		t.Fatal("Incorrect _missingResources; got", missing)
	}

	noncompliant := configPolNode.Properties["_nonCompliantResources"]
	if noncompliant != `[{"v":"v1","k":"Namespace","n":"default"},{"v":"v1","k":"Namespace","n":"nonexistent"}]` {
		t.Fatal("Incorrect _nonCompliantResources; got", noncompliant)
	}
}
