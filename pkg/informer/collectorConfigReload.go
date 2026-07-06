// Copyright Contributors to the Open Cluster Management project

package informer

import (
	"strings"
	"sync"

	tr "github.com/stolostron/search-collector/pkg/transforms"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
)

// CollectorConfigGVR is the GVR for the CollectorConfig custom resource.
var CollectorConfigGVR = schema.GroupVersionResource{
	Group:    "search.open-cluster-management.io",
	Version:  "v1alpha1",
	Resource: "collectorconfigs",
}

// ConfigReloadHandler intercepts GenericInformer events for CollectorConfig resources.
// When the merged-collector-config CR is added, updated (spec change), or deleted,
// it reloads the transform config and triggers targeted informer resyncs or a full
// syncInformers pass (when exclude rules change).
type ConfigReloadHandler struct {
	LastSeenGeneration int64
	// ReloadFn reloads config from the cluster and returns a ReloadResult describing
	// what changed. Returns nil if nothing changed.
	ReloadFn func() *tr.ReloadResult
}

// handleReloadResult processes a ReloadResult by triggering the appropriate signals.
func handleReloadResult(result *tr.ReloadResult) {
	if result == nil {
		return
	}
	if result.ExcludeRulesChanged {
		TriggerSyncInformers()
	}
	if len(result.AffectedKeys) > 0 {
		TriggerResyncForConfigKeys(result.AffectedKeys)
	}
}

func (h *ConfigReloadHandler) OnAdd(obj *unstructured.Unstructured) {
	if obj.GetName() != "merged-collector-config" {
		return
	}
	h.LastSeenGeneration = obj.GetGeneration()
	klog.Info("CollectorConfig merged-collector-config created, reloading config")
	handleReloadResult(h.ReloadFn())
}

func (h *ConfigReloadHandler) OnUpdate(obj *unstructured.Unstructured) {
	if obj.GetName() != "merged-collector-config" {
		return
	}
	gen := obj.GetGeneration()
	if gen == h.LastSeenGeneration {
		klog.V(3).Info("CollectorConfig status-only update (generation unchanged), skipping reload")
		return
	}
	h.LastSeenGeneration = gen
	klog.Info("CollectorConfig merged-collector-config modified, reloading config")
	handleReloadResult(h.ReloadFn())
}

func (h *ConfigReloadHandler) OnDelete(obj *unstructured.Unstructured) {
	if obj.GetName() != "merged-collector-config" {
		return
	}
	h.LastSeenGeneration = 0
	klog.Warning("CollectorConfig merged-collector-config deleted — reverting to defaults")
	handleReloadResult(h.ReloadFn())
}

// resyncSignal wakes the main loop when config keys need informer re-listing.
// Buffered (cap 1) — the signal is coalesced, but all keys are preserved in pendingResync.
var resyncSignal = make(chan struct{}, 1)

// resyncMu guards pendingResync and pendingSyncInformers for concurrent access.
var resyncMu sync.Mutex

// pendingResync accumulates config keys from one or more TriggerResyncForConfigKeys calls.
// Drained by the main loop when resyncSignal fires.
var pendingResync = make(map[string]struct{})

// pendingSyncInformers signals that a full syncInformers pass is needed (e.g. because
// exclude rules changed, requiring informers to be started or stopped).
var pendingSyncInformers bool

// TriggerResyncForConfigKeys queues a set of config keys (e.g. "Deployment.apps", "Pod", "*.apps")
// for informer re-listing. Called by the config watcher after detecting a config change.
// Keys are union-accumulated so rapid calls never lose distinct keys.
func TriggerResyncForConfigKeys(keys []string) {
	resyncMu.Lock()
	for _, key := range keys {
		pendingResync[key] = struct{}{}
	}
	resyncMu.Unlock()

	select {
	case resyncSignal <- struct{}{}:
	default: // already signaled
	}
}

// TriggerSyncInformers signals the main loop that a full syncInformers pass is needed.
// This is used when exclude rules change, requiring informers to be started or stopped.
func TriggerSyncInformers() {
	resyncMu.Lock()
	pendingSyncInformers = true
	resyncMu.Unlock()

	select {
	case resyncSignal <- struct{}{}:
	default: // already signaled
	}
}

// drainPendingResync returns all accumulated config keys, whether a full syncInformers
// pass is needed, and clears the pending state.
func drainPendingResync() ([]string, bool) {
	resyncMu.Lock()
	keys := make([]string, 0, len(pendingResync))
	for k := range pendingResync {
		keys = append(keys, k)
		delete(pendingResync, k)
	}
	needSync := pendingSyncInformers
	pendingSyncInformers = false
	resyncMu.Unlock()
	return keys, needSync
}

// dispatchResyncForKey triggers informer re-listing for a single config key.
// For specific kinds (e.g. "Pod", "Deployment.apps") it does an exact GVR lookup.
// For wildcards (e.g. "*", "*.apps") it resyncs all informers in the matching API group.
func dispatchResyncForKey(key string, configKeyToGVR map[string]schema.GroupVersionResource, informers map[schema.GroupVersionResource]informerEntry) {
	kind, group := kindAndGroupFromConfigKey(key)

	if kind != "*" {
		gvr, ok := configKeyToGVR[key]
		if !ok {
			klog.V(3).Infof("Config change for %q — no matching informer found", key)
			return
		}
		if entry, ok := informers[gvr]; ok {
			klog.V(2).Infof("Config change for %q — triggering resync of %s", key, gvr.String())
			entry.informer.TriggerResync()
		}
		return
	}

	// Wildcard: resync all informers in the matching API group.
	// If group is also "*", resync everything.
	for gvr, entry := range informers {
		if group == "*" || gvr.Group == group {
			klog.V(2).Infof("Config wildcard %q — triggering resync of %s", key, gvr.String())
			entry.informer.TriggerResync()
		}
	}
}

// kindAndGroupFromConfigKey splits a config key into its kind and group parts.
// "Pod" → ("Pod", ""), "Deployment.apps" → ("Deployment", "apps"),
// "*.apps" → ("*", "apps"), "*" → ("*", "")
func kindAndGroupFromConfigKey(key string) (kind, group string) {
	if idx := strings.Index(key, "."); idx >= 0 {
		return key[:idx], key[idx+1:]
	}
	return key, "" // core API
}
