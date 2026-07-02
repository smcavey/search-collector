package transforms

import (
	"reflect"

	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
)

// ReloadAndDiff snapshots the current config, reloads from the cluster, diffs the
// old and new configs, and returns the list of config keys whose config changed.
// Returns nil if nothing changed.
func ReloadAndDiff(dynamicClient dynamic.Interface) []string {
	oldConfig := snapshotConfig()

	// Reload config from cluster (performs atomic swap under configMu.Lock).
	loadAndMergeConfigurableCollectionWithClient(dynamicClient)

	newConfig := snapshotConfig()

	affected := diffConfigs(oldConfig, newConfig)
	if len(affected) == 0 {
		klog.V(2).Info("Config reload: no config changes detected")
		return nil
	}

	klog.Infof("Config reload: %d resource config(s) changed: %v", len(affected), affected)
	return affected
}

// snapshotConfig returns a deep copy of the current mergedTransformConfig.
// The copy is taken under RLock to ensure a consistent snapshot.
func snapshotConfig() map[string]ResourceConfig {
	configMu.RLock()
	snapshot := deepCopyTransformConfig(mergedTransformConfig)
	configMu.RUnlock()
	return snapshot
}

// diffConfigs compares old and new config maps, returning the list of config keys
// that were added, removed, or changed.
func diffConfigs(old, new map[string]ResourceConfig) []string {
	affected := map[string]bool{}

	for k := range new {
		if _, ok := old[k]; !ok {
			affected[k] = true
		}
	}
	for k := range old {
		if _, ok := new[k]; !ok {
			affected[k] = true
		}
	}

	for k, newCfg := range new {
		if oldCfg, ok := old[k]; ok && !reflect.DeepEqual(oldCfg, newCfg) {
			affected[k] = true
		}
	}

	result := make([]string, 0, len(affected))
	for k := range affected {
		result = append(result, k)
	}
	return result
}
