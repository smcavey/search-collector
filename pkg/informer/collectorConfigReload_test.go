package informer

import (
	"sort"
	"testing"

	tr "github.com/stolostron/search-collector/pkg/transforms"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// drainResyncState is a test helper that clears the global resyncSignal, pendingResync, and pendingSyncInformers.
func drainResyncState() {
	select {
	case <-resyncSignal:
	default:
	}
	resyncMu.Lock()
	for k := range pendingResync {
		delete(pendingResync, k)
	}
	pendingSyncInformers = false
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
		h := &ConfigReloadHandler{ReloadFn: func() *tr.ReloadResult {
			called = true
			return &tr.ReloadResult{AffectedKeys: []string{"Pod"}}
		}}

		h.OnAdd(newCollectorConfigObj("other-config", 1))
		assert.False(t, called, "ReloadFn should not be called for wrong name")
	})

	t.Run("correct name triggers reload and sets generation", func(t *testing.T) {
		drainResyncState()
		h := &ConfigReloadHandler{ReloadFn: func() *tr.ReloadResult {
			return &tr.ReloadResult{AffectedKeys: []string{"Pod", "Deployment.apps"}}
		}}

		h.OnAdd(newCollectorConfigObj("merged-collector-config", 3))

		assert.Equal(t, int64(3), h.LastSeenGeneration)

		// Verify resync was triggered with the returned keys.
		select {
		case <-resyncSignal:
		default:
			t.Fatal("expected resyncSignal")
		}
		received, needSync := drainPendingResync()
		sort.Strings(received)
		assert.Equal(t, []string{"Deployment.apps", "Pod"}, received)
		assert.False(t, needSync)
	})
}

func TestConfigReloadHandler_OnUpdate(t *testing.T) {
	t.Run("ignores wrong name", func(t *testing.T) {
		drainResyncState()
		called := false
		h := &ConfigReloadHandler{ReloadFn: func() *tr.ReloadResult {
			called = true
			return &tr.ReloadResult{AffectedKeys: []string{"Pod"}}
		}}

		h.OnUpdate(newCollectorConfigObj("other-config", 2))
		assert.False(t, called)
	})

	t.Run("skips status-only update (same generation)", func(t *testing.T) {
		drainResyncState()
		called := false
		h := &ConfigReloadHandler{
			LastSeenGeneration: 5,
			ReloadFn: func() *tr.ReloadResult {
				called = true
				return &tr.ReloadResult{AffectedKeys: []string{"Pod"}}
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
			ReloadFn: func() *tr.ReloadResult {
				return &tr.ReloadResult{AffectedKeys: []string{"Secret"}}
			},
		}

		h.OnUpdate(newCollectorConfigObj("merged-collector-config", 6))

		assert.Equal(t, int64(6), h.LastSeenGeneration)
		select {
		case <-resyncSignal:
		default:
			t.Fatal("expected resyncSignal")
		}
		received, needSync := drainPendingResync()
		assert.Equal(t, []string{"Secret"}, received)
		assert.False(t, needSync)
	})
}

func TestConfigReloadHandler_OnDelete(t *testing.T) {
	t.Run("ignores wrong name", func(t *testing.T) {
		drainResyncState()
		called := false
		h := &ConfigReloadHandler{
			LastSeenGeneration: 3,
			ReloadFn: func() *tr.ReloadResult {
				called = true
				return &tr.ReloadResult{AffectedKeys: []string{"Pod"}}
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
			ReloadFn: func() *tr.ReloadResult {
				return &tr.ReloadResult{AffectedKeys: []string{"Pod", "*.apps"}}
			},
		}

		h.OnDelete(newCollectorConfigObj("merged-collector-config", 5))

		assert.Equal(t, int64(0), h.LastSeenGeneration, "generation should be reset on delete")
		select {
		case <-resyncSignal:
		default:
			t.Fatal("expected resyncSignal")
		}
		received, needSync := drainPendingResync()
		sort.Strings(received)
		assert.Equal(t, []string{"*.apps", "Pod"}, received)
		assert.False(t, needSync)
	})
}

func TestConfigReloadHandler_NoResyncWhenNoChanges(t *testing.T) {
	drainResyncState()
	h := &ConfigReloadHandler{ReloadFn: func() *tr.ReloadResult {
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
	received, needSync := drainPendingResync()
	sort.Strings(received)
	sort.Strings(keys)
	assert.Equal(t, keys, received)
	assert.False(t, needSync, "TriggerResyncForConfigKeys should not set pendingSyncInformers")
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
	received, needSync := drainPendingResync()
	sort.Strings(received)
	expected := []string{"Node", "Pod", "Secret"}
	assert.Equal(t, expected, received, "all keys from both calls should be accumulated")
	assert.False(t, needSync)

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
	configMapsGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

	configKeyToGVR := map[string]schema.GroupVersionResource{
		"Pod":                                      podsGVR,
		"Deployment.apps":                          deploymentsGVR,
		"Policy.policy.open-cluster-management.io": policiesGVR,
		"ConfigMap":                                configMapsGVR, // present in map but NOT in informers
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
		{
			name:            "exact match - key in configKeyToGVR but GVR not in informers",
			key:             "ConfigMap",
			expectedResyncs: []schema.GroupVersionResource{},
		},
		{
			name:            "wildcard - multi-dot group",
			key:             "*.policy.open-cluster-management.io",
			expectedResyncs: []schema.GroupVersionResource{policiesGVR},
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

func TestTriggerSyncInformers(t *testing.T) {
	drainResyncState()

	TriggerSyncInformers()

	// Signal should be present.
	select {
	case <-resyncSignal:
	default:
		t.Fatal("expected resyncSignal to have a signal")
	}

	// Drain should report needSync=true and no keys.
	received, needSync := drainPendingResync()
	assert.Empty(t, received, "TriggerSyncInformers should not add config keys")
	assert.True(t, needSync, "expected pendingSyncInformers to be true")
}

func TestTriggerSyncInformers_CoalescesWithKeys(t *testing.T) {
	drainResyncState()

	TriggerResyncForConfigKeys([]string{"Pod"})
	TriggerSyncInformers()

	select {
	case <-resyncSignal:
	default:
		t.Fatal("expected resyncSignal")
	}

	received, needSync := drainPendingResync()
	assert.Equal(t, []string{"Pod"}, received)
	assert.True(t, needSync, "expected pendingSyncInformers to be true")
}

func TestConfigReloadHandler_ExcludeRulesChanged(t *testing.T) {
	drainResyncState()
	h := &ConfigReloadHandler{ReloadFn: func() *tr.ReloadResult {
		return &tr.ReloadResult{ExcludeRulesChanged: true}
	}}

	h.OnAdd(newCollectorConfigObj("merged-collector-config", 1))

	select {
	case <-resyncSignal:
	default:
		t.Fatal("expected resyncSignal")
	}

	received, needSync := drainPendingResync()
	assert.Empty(t, received, "no config keys expected when only exclude rules changed")
	assert.True(t, needSync, "expected pendingSyncInformers when exclude rules changed")
}

func TestConfigReloadHandler_BothKeysAndExcludeRules(t *testing.T) {
	drainResyncState()
	h := &ConfigReloadHandler{ReloadFn: func() *tr.ReloadResult {
		return &tr.ReloadResult{
			AffectedKeys:        []string{"Pod"},
			ExcludeRulesChanged: true,
		}
	}}

	h.OnAdd(newCollectorConfigObj("merged-collector-config", 1))

	select {
	case <-resyncSignal:
	default:
		t.Fatal("expected resyncSignal")
	}

	received, needSync := drainPendingResync()
	assert.Equal(t, []string{"Pod"}, received)
	assert.True(t, needSync)
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
