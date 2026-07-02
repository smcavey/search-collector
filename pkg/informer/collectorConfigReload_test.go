package informer

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// drainResyncState is a test helper that clears the global resyncSignal and pendingResync.
func drainResyncState() {
	select {
	case <-resyncSignal:
	default:
	}
	resyncMu.Lock()
	for k := range pendingResync {
		delete(pendingResync, k)
	}
	resyncMu.Unlock()
}

func newCollectorConfigObj(name string, generation int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "search.open-cluster-management.io/v1alpha1",
			"kind":       "CollectorConfig",
			"metadata": map[string]interface{}{
				"name":       name,
				"generation": generation,
			},
		},
	}
}

func TestConfigReloadHandler_OnAdd(t *testing.T) {
	t.Run("ignores wrong name", func(t *testing.T) {
		drainResyncState()
		called := false
		h := &ConfigReloadHandler{ReloadFn: func() []string {
			called = true
			return []string{"Pod"}
		}}

		h.OnAdd(newCollectorConfigObj("other-config", 1))
		assert.False(t, called, "ReloadFn should not be called for wrong name")
	})

	t.Run("correct name triggers reload and sets generation", func(t *testing.T) {
		drainResyncState()
		h := &ConfigReloadHandler{ReloadFn: func() []string {
			return []string{"Pod", "Deployment.apps"}
		}}

		h.OnAdd(newCollectorConfigObj("merged-collector-config", 3))

		assert.Equal(t, int64(3), h.LastSeenGeneration)

		// Verify resync was triggered with the returned keys.
		select {
		case <-resyncSignal:
		default:
			t.Fatal("expected resyncSignal")
		}
		received := drainPendingResync()
		sort.Strings(received)
		assert.Equal(t, []string{"Deployment.apps", "Pod"}, received)
	})
}

func TestConfigReloadHandler_OnUpdate(t *testing.T) {
	t.Run("ignores wrong name", func(t *testing.T) {
		drainResyncState()
		called := false
		h := &ConfigReloadHandler{ReloadFn: func() []string {
			called = true
			return []string{"Pod"}
		}}

		h.OnUpdate(newCollectorConfigObj("other-config", 2))
		assert.False(t, called)
	})

	t.Run("skips status-only update (same generation)", func(t *testing.T) {
		drainResyncState()
		called := false
		h := &ConfigReloadHandler{
			LastSeenGeneration: 5,
			ReloadFn: func() []string {
				called = true
				return []string{"Pod"}
			},
		}

		h.OnUpdate(newCollectorConfigObj("merged-collector-config", 5))
		assert.False(t, called, "ReloadFn should not be called for status-only update")
		assert.Equal(t, int64(5), h.LastSeenGeneration)
	})

	t.Run("spec change triggers reload", func(t *testing.T) {
		drainResyncState()
		h := &ConfigReloadHandler{
			LastSeenGeneration: 5,
			ReloadFn: func() []string {
				return []string{"Secret"}
			},
		}

		h.OnUpdate(newCollectorConfigObj("merged-collector-config", 6))

		assert.Equal(t, int64(6), h.LastSeenGeneration)
		select {
		case <-resyncSignal:
		default:
			t.Fatal("expected resyncSignal")
		}
		received := drainPendingResync()
		assert.Equal(t, []string{"Secret"}, received)
	})
}

func TestConfigReloadHandler_OnDelete(t *testing.T) {
	t.Run("ignores wrong name", func(t *testing.T) {
		drainResyncState()
		called := false
		h := &ConfigReloadHandler{
			LastSeenGeneration: 3,
			ReloadFn: func() []string {
				called = true
				return []string{"Pod"}
			},
		}

		h.OnDelete(newCollectorConfigObj("other-config", 3))
		assert.False(t, called)
		assert.Equal(t, int64(3), h.LastSeenGeneration, "generation should not be reset for wrong name")
	})

	t.Run("correct name triggers reload and resets generation", func(t *testing.T) {
		drainResyncState()
		h := &ConfigReloadHandler{
			LastSeenGeneration: 5,
			ReloadFn: func() []string {
				return []string{"Pod", "*.apps"}
			},
		}

		h.OnDelete(newCollectorConfigObj("merged-collector-config", 5))

		assert.Equal(t, int64(0), h.LastSeenGeneration, "generation should be reset on delete")
		select {
		case <-resyncSignal:
		default:
			t.Fatal("expected resyncSignal")
		}
		received := drainPendingResync()
		sort.Strings(received)
		assert.Equal(t, []string{"*.apps", "Pod"}, received)
	})
}

func TestConfigReloadHandler_NoResyncWhenNoChanges(t *testing.T) {
	drainResyncState()
	h := &ConfigReloadHandler{ReloadFn: func() []string {
		return nil // no config changes
	}}

	h.OnAdd(newCollectorConfigObj("merged-collector-config", 1))

	// ReloadFn was called but returned nil — no resync should be signaled.
	select {
	case <-resyncSignal:
		t.Error("resyncSignal should not be signaled when ReloadFn returns nil")
	default:
		// expected
	}
}

func TestTriggerResyncForConfigKeys(t *testing.T) {
	// Drain any existing signal and keys.
	drainResyncState()

	keys := []string{"Pod", "Deployment.apps"}
	TriggerResyncForConfigKeys(keys)

	// Signal should be present.
	select {
	case <-resyncSignal:
	default:
		t.Fatal("expected resyncSignal to have a signal")
	}

	// Drain and verify keys.
	received := drainPendingResync()
	sort.Strings(received)
	sort.Strings(keys)
	assert.Equal(t, keys, received)
}

func TestTriggerResyncForConfigKeys_Coalesce(t *testing.T) {
	// Drain any existing signal and keys.
	drainResyncState()

	first := []string{"Pod"}
	second := []string{"Secret", "Node"}

	TriggerResyncForConfigKeys(first)
	TriggerResyncForConfigKeys(second) // keys are union-accumulated, not dropped

	// Drain signal.
	select {
	case <-resyncSignal:
	default:
		t.Fatal("expected resyncSignal to have a signal")
	}

	// All keys from both calls should be present.
	received := drainPendingResync()
	sort.Strings(received)
	expected := []string{"Node", "Pod", "Secret"}
	assert.Equal(t, expected, received, "all keys from both calls should be accumulated")

	// Signal should be empty now.
	select {
	case <-resyncSignal:
		t.Error("expected resyncSignal to be empty after draining")
	default:
		// expected
	}
}

// mockInformerEntry creates an informerEntry with a real resyncCh for testing dispatch.
func mockInformerEntry() informerEntry {
	inform := &GenericInformer{resyncCh: make(chan struct{}, 1)}
	return informerEntry{cancel: func() {}, informer: inform}
}

func TestDispatchResyncForKey(t *testing.T) {
	podsGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	deploymentsGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	policiesGVR := schema.GroupVersionResource{Group: "policy.open-cluster-management.io", Version: "v1", Resource: "policies"}

	configKeyToGVR := map[string]schema.GroupVersionResource{
		"Pod":                                      podsGVR,
		"Deployment.apps":                          deploymentsGVR,
		"Policy.policy.open-cluster-management.io": policiesGVR,
	}

	tests := []struct {
		name            string
		key             string
		expectedResyncs []schema.GroupVersionResource // GVRs that should have a signal queued
	}{
		{
			name:            "exact match - core resource",
			key:             "Pod",
			expectedResyncs: []schema.GroupVersionResource{podsGVR},
		},
		{
			name:            "exact match - non-core resource",
			key:             "Deployment.apps",
			expectedResyncs: []schema.GroupVersionResource{deploymentsGVR},
		},
		{
			name:            "exact match - no matching informer",
			key:             "Secret",
			expectedResyncs: []schema.GroupVersionResource{},
		},
		{
			name:            "wildcard - specific group",
			key:             "*.apps",
			expectedResyncs: []schema.GroupVersionResource{deploymentsGVR},
		},
		{
			name:            "wildcard - core group",
			key:             "*",
			expectedResyncs: []schema.GroupVersionResource{podsGVR},
		},
		{
			name:            "wildcard - all groups (*.*)",
			key:             "*.*",
			expectedResyncs: []schema.GroupVersionResource{podsGVR, deploymentsGVR, policiesGVR},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Build a fresh informers map for each test case.
			informers := map[schema.GroupVersionResource]informerEntry{
				podsGVR:        mockInformerEntry(),
				deploymentsGVR: mockInformerEntry(),
				policiesGVR:    mockInformerEntry(),
			}

			dispatchResyncForKey(tc.key, configKeyToGVR, informers)

			expectedSet := map[schema.GroupVersionResource]bool{}
			for _, gvr := range tc.expectedResyncs {
				expectedSet[gvr] = true
			}

			for gvr, entry := range informers {
				select {
				case <-entry.informer.resyncCh:
					assert.True(t, expectedSet[gvr], "unexpected resync for %s", gvr.String())
				default:
					assert.False(t, expectedSet[gvr], "expected resync for %s but none queued", gvr.String())
				}
			}
		})
	}
}

func TestKindAndGroupFromConfigKey(t *testing.T) {
	tests := []struct {
		input         string
		expectedKind  string
		expectedGroup string
	}{
		{"Pod", "Pod", ""},
		{"Deployment.apps", "Deployment", "apps"},
		{"*.apps", "*", "apps"},
		{"*", "*", ""},
		{"Policy.policy.open-cluster-management.io", "Policy", "policy.open-cluster-management.io"},
		{"*.monitoring.coreos.com", "*", "monitoring.coreos.com"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			kind, group := kindAndGroupFromConfigKey(tc.input)
			assert.Equal(t, tc.expectedKind, kind)
			assert.Equal(t, tc.expectedGroup, group)
		})
	}
}
