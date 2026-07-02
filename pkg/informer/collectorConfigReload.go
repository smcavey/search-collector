package informer

import (
	"strings"
	"sync"

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
// it reloads the transform config and triggers targeted informer resyncs.
// It wraps the normal indexing handlers so both indexing and config reload happen on the same event.
type ConfigReloadHandler struct {
	LastSeenGeneration int64
	// ReloadFn reloads config from the cluster and returns the list of changed config keys.
	ReloadFn func() []string
}

func (h *ConfigReloadHandler) OnAdd(obj *unstructured.Unstructured) {
	if obj.GetName() != "merged-collector-config" {
		return
	}
	h.LastSeenGeneration = obj.GetGeneration()
	klog.Info("CollectorConfig merged-collector-config created, reloading config")
	if keys := h.ReloadFn(); len(keys) > 0 {
		TriggerResyncForConfigKeys(keys)
	}
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
	if keys := h.ReloadFn(); len(keys) > 0 {
		TriggerResyncForConfigKeys(keys)
	}
}

func (h *ConfigReloadHandler) OnDelete(obj *unstructured.Unstructured) {
	if obj.GetName() != "merged-collector-config" {
		return
	}
	h.LastSeenGeneration = 0
	klog.Warning("CollectorConfig merged-collector-config deleted — reverting to defaults")
	if keys := h.ReloadFn(); len(keys) > 0 {
		TriggerResyncForConfigKeys(keys)
	}
}

// resyncSignal wakes the main loop when config keys need informer re-listing.
// Buffered (cap 1) — the signal is coalesced, but all keys are preserved in pendingResync.
var resyncSignal = make(chan struct{}, 1)

// resyncMu guards pendingResync for concurrent access.
var resyncMu sync.Mutex

// pendingResync accumulates config keys from one or more TriggerResyncForConfigKeys calls.
// Drained by the main loop when resyncSignal fires.
var pendingResync = make(map[string]struct{})

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

// drainPendingResync returns all accumulated config keys and clears the set.
func drainPendingResync() []string {
	resyncMu.Lock()
	keys := make([]string, 0, len(pendingResync))
	for k := range pendingResync {
		keys = append(keys, k)
		delete(pendingResync, k)
	}
	resyncMu.Unlock()
	return keys
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
