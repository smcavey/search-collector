package transforms

import (
	"reflect"

	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
)

// ReloadResult describes what changed after a config reload.
type ReloadResult struct {
	// AffectedKeys lists config keys (e.g. "Pod", "Deployment.apps") whose transform
	// properties changed. These trigger targeted informer re-listing.
	AffectedKeys []string
	// ExcludeRulesChanged is true when the exclude/include rule list was modified.
	// This requires a full syncInformers pass to start/stop informers.
	ExcludeRulesChanged bool
}

// ReloadAndDiff snapshots the current config and exclude rules, reloads from the
// cluster, diffs old vs new, and returns a ReloadResult describing what changed.
// Returns nil if nothing changed.
func ReloadAndDiff(dynamicClient dynamic.Interface) *ReloadResult {
	oldConfig := snapshotConfig()
	oldRules := snapshotExcludeRules()

	// Reload config from cluster (performs atomic swap under configMu.Lock).
	loadAndMergeConfigurableCollectionWithClient(dynamicClient)

	newConfig := snapshotConfig()
	newRules := snapshotExcludeRules()

	affected := diffConfigs(oldConfig, newConfig)
	rulesChanged := excludeRulesChanged(oldRules, newRules)

	if len(affected) == 0 && !rulesChanged {
		klog.V(2).Info("Config reload: no config changes detected")
		return nil
	}

	if rulesChanged {
		klog.Info("Config reload: exclude rules changed")
	}
	if len(affected) > 0 {
		klog.Infof("Config reload: %d resource config(s) changed: %v", len(affected), affected)
	}

	return &ReloadResult{
		AffectedKeys:        affected,
		ExcludeRulesChanged: rulesChanged,
	}
}

// snapshotConfig returns a deep copy of the current mergedTransformConfig.
// The copy is taken under RLock to ensure a consistent snapshot.
func snapshotConfig() map[string]ResourceConfig {
	configMu.RLock()
	snapshot := deepCopyTransformConfig(mergedTransformConfig)
	configMu.RUnlock()
	return snapshot
}

// snapshotExcludeRules returns a shallow copy of the current excludeRules slice.
// The copy is taken under RLock to ensure a consistent snapshot. Each excludeRule
// contains only slices of strings (immutable once built), so a shallow copy suffices.
func snapshotExcludeRules() []excludeRule {
	configMu.RLock()
	snapshot := make([]excludeRule, len(excludeRules))
	copy(snapshot, excludeRules)
	configMu.RUnlock()
	return snapshot
}

// excludeRulesChanged compares two exclude rule snapshots for equality.
func excludeRulesChanged(old, new []excludeRule) bool {
	return !reflect.DeepEqual(old, new)
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
