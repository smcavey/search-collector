// Copyright Contributors to the Open Cluster Management project

package transforms

import (
	"sort"
	"sync"
	"testing"

	"github.com/stolostron/search-collector/pkg/config"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestDiffConfigs(t *testing.T) {
	baseCfg := ResourceConfig{
		properties: []ExtractProperty{{Name: "name", JSONPath: "{.metadata.name}"}},
	}
	modifiedCfg := ResourceConfig{
		properties: []ExtractProperty{
			{Name: "name", JSONPath: "{.metadata.name}"},
			{Name: "status", JSONPath: "{.status.phase}"},
		},
	}
	conditionsOnCfg := ResourceConfig{
		properties:        []ExtractProperty{{Name: "name", JSONPath: "{.metadata.name}"}},
		extractConditions: true,
	}
	priority0 := 0
	priority5 := 5
	printerCfg0 := ResourceConfig{
		properties:                       []ExtractProperty{{Name: "name", JSONPath: "{.metadata.name}"}},
		additionalPrinterColumnsPriority: &priority0,
	}
	printerCfg5 := ResourceConfig{
		properties:                       []ExtractProperty{{Name: "name", JSONPath: "{.metadata.name}"}},
		additionalPrinterColumnsPriority: &priority5,
	}

	tests := []struct {
		name     string
		old      map[string]ResourceConfig
		new      map[string]ResourceConfig
		expected []string
	}{
		{
			name:     "identical configs",
			old:      map[string]ResourceConfig{"Pod": baseCfg},
			new:      map[string]ResourceConfig{"Pod": baseCfg},
			expected: []string{},
		},
		{
			name:     "key added",
			old:      map[string]ResourceConfig{},
			new:      map[string]ResourceConfig{"Pod": baseCfg},
			expected: []string{"Pod"},
		},
		{
			name:     "key removed",
			old:      map[string]ResourceConfig{"Pod": baseCfg},
			new:      map[string]ResourceConfig{},
			expected: []string{"Pod"},
		},
		{
			name:     "key changed - property added",
			old:      map[string]ResourceConfig{"Pod": baseCfg},
			new:      map[string]ResourceConfig{"Pod": modifiedCfg},
			expected: []string{"Pod"},
		},
		{
			name:     "key changed - extractConditions toggled",
			old:      map[string]ResourceConfig{"Pod": baseCfg},
			new:      map[string]ResourceConfig{"Pod": conditionsOnCfg},
			expected: []string{"Pod"},
		},
		{
			name:     "key changed - printerColumnsPriority changed",
			old:      map[string]ResourceConfig{"Pod": printerCfg0},
			new:      map[string]ResourceConfig{"Pod": printerCfg5},
			expected: []string{"Pod"},
		},
		{
			name:     "multiple changes",
			old:      map[string]ResourceConfig{"Pod": baseCfg, "Secret": baseCfg},
			new:      map[string]ResourceConfig{"Pod": baseCfg, "Secret": modifiedCfg, "Node": baseCfg},
			expected: []string{"Node", "Secret"},
		},
		{
			name:     "both nil",
			old:      nil,
			new:      nil,
			expected: []string{},
		},
		{
			name:     "old nil new populated",
			old:      nil,
			new:      map[string]ResourceConfig{"Pod": baseCfg},
			expected: []string{"Pod"},
		},
		{
			name:     "wildcard key changed",
			old:      map[string]ResourceConfig{"*.apps": baseCfg},
			new:      map[string]ResourceConfig{"*.apps": conditionsOnCfg},
			expected: []string{"*.apps"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := diffConfigs(tc.old, tc.new)
			sort.Strings(result)
			sort.Strings(tc.expected)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestSnapshotConfig(t *testing.T) {
	origConfig := mergedTransformConfig
	defer func() {
		mergedTransformConfigMu.Lock()
		mergedTransformConfig = origConfig
		mergedTransformConfigMu.Unlock()
	}()

	original := map[string]ResourceConfig{
		"Pod": {
			properties:        []ExtractProperty{{Name: "name", JSONPath: "{.metadata.name}"}},
			extractConditions: true,
		},
	}
	mergedTransformConfigMu.Lock()
	mergedTransformConfig = original
	mergedTransformConfigMu.Unlock()

	snapshot := snapshotConfig()

	assert.Equal(t, len(original), len(snapshot))
	assert.Equal(t, original["Pod"].extractConditions, snapshot["Pod"].extractConditions)

	// Mutate the snapshot — should NOT affect the global.
	snapshot["Pod"] = ResourceConfig{extractConditions: false}
	snapshot["NewKey"] = ResourceConfig{}

	mergedTransformConfigMu.RLock()
	assert.True(t, mergedTransformConfig["Pod"].extractConditions, "mutating snapshot should not affect global")
	_, exists := mergedTransformConfig["NewKey"]
	assert.False(t, exists, "adding key to snapshot should not affect global")
	mergedTransformConfigMu.RUnlock()
}

func TestSnapshotConfig_Concurrent(t *testing.T) {
	origConfig := mergedTransformConfig
	origFeature := config.Cfg.FeatureConfigurableCollection
	origNamespace := config.Cfg.PodNamespace
	defer func() {
		config.Cfg.FeatureConfigurableCollection = origFeature
		config.Cfg.PodNamespace = origNamespace
		mergedTransformConfigMu.Lock()
		mergedTransformConfig = origConfig
		mergedTransformConfigMu.Unlock()
	}()

	config.Cfg.FeatureConfigurableCollection = true
	config.Cfg.PodNamespace = "test-ns"

	mergedTransformConfigMu.Lock()
	mergedTransformConfig = map[string]ResourceConfig{
		"Pod": {properties: []ExtractProperty{{Name: "name", JSONPath: "{.metadata.name}"}}},
	}
	mergedTransformConfigMu.Unlock()

	scheme := runtime.NewScheme()
	fakeClient := fake.NewSimpleDynamicClient(scheme)

	var wg sync.WaitGroup
	const numReaders = 10

	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := snapshotConfig()
			if snap == nil {
				t.Error("snapshotConfig returned nil")
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		loadAndMergeConfigurableCollectionWithClient(fakeClient)
	}()

	wg.Wait()
}

// Helper to save and restore global state.
func saveAndRestoreConfigState(t *testing.T) {
	t.Helper()
	origConfig := mergedTransformConfig
	origRules := excludeRules
	origFeature := config.Cfg.FeatureConfigurableCollection
	origNamespace := config.Cfg.PodNamespace
	t.Cleanup(func() {
		config.Cfg.FeatureConfigurableCollection = origFeature
		config.Cfg.PodNamespace = origNamespace
		mergedTransformConfigMu.Lock()
		mergedTransformConfig = origConfig
		excludeRules = origRules
		mergedTransformConfigMu.Unlock()
	})
	config.Cfg.FeatureConfigurableCollection = true
	config.Cfg.PodNamespace = "test-ns"
}

func TestReloadAndDiff_NoChange(t *testing.T) {
	saveAndRestoreConfigState(t)

	mergedTransformConfigMu.Lock()
	mergedTransformConfig = deepCopyTransformConfig(defaultTransformConfig)
	excludeRules = nil
	mergedTransformConfigMu.Unlock()

	scheme := runtime.NewScheme()
	fakeClient := fake.NewSimpleDynamicClient(scheme)

	result := ReloadAndDiff(fakeClient)
	assert.Nil(t, result, "should return nil when config is unchanged")
}

func TestReloadAndDiff_ConfigAdded(t *testing.T) {
	saveAndRestoreConfigState(t)

	mergedTransformConfigMu.Lock()
	mergedTransformConfig = deepCopyTransformConfig(defaultTransformConfig)
	excludeRules = nil
	mergedTransformConfigMu.Unlock()

	collectionConfig := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "search.open-cluster-management.io/v1alpha1",
			"kind":       "CollectorConfig",
			"metadata": map[string]interface{}{
				"name":      "merged-collector-config",
				"namespace": "test-ns",
			},
			"spec": map[string]interface{}{
				"collectionRules": []interface{}{
					map[string]interface{}{
						"action": "include",
						"resourceSelector": map[string]interface{}{
							"apiGroups": []interface{}{""},
							"kinds":     []interface{}{"Pod"},
						},
						"fields": []interface{}{
							map[string]interface{}{
								"name":     "dnsPolicy",
								"jsonPath": "{.spec.dnsPolicy}",
							},
						},
					},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	fakeClient := fake.NewSimpleDynamicClient(scheme, collectionConfig)

	result := ReloadAndDiff(fakeClient)
	assert.NotNil(t, result)
	assert.Contains(t, result.AffectedResources, "Pod")
	// The include rule appends an ActionInclude entry to excludeRules (going from nil to non-empty),
	// so ExcludeRulesChanged is true. This is expected behavior.
	assert.True(t, result.ExcludeRulesChanged)
}

func TestReloadAndDiff_ConfigRemoved(t *testing.T) {
	saveAndRestoreConfigState(t)

	initialConfig := deepCopyTransformConfig(defaultTransformConfig)
	podCfg := initialConfig["Pod"]
	podCfg.properties = append(podCfg.properties, ExtractProperty{Name: "custom", JSONPath: "{.spec.custom}"})
	initialConfig["Pod"] = podCfg

	mergedTransformConfigMu.Lock()
	mergedTransformConfig = initialConfig
	excludeRules = nil
	mergedTransformConfigMu.Unlock()

	scheme := runtime.NewScheme()
	fakeClient := fake.NewSimpleDynamicClient(scheme)

	result := ReloadAndDiff(fakeClient)
	assert.NotNil(t, result)
	assert.Contains(t, result.AffectedResources, "Pod")
}

func TestReloadAndDiff_MultipleResourcesChanged(t *testing.T) {
	saveAndRestoreConfigState(t)

	mergedTransformConfigMu.Lock()
	mergedTransformConfig = deepCopyTransformConfig(defaultTransformConfig)
	excludeRules = nil
	mergedTransformConfigMu.Unlock()

	collectionConfig := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "search.open-cluster-management.io/v1alpha1",
			"kind":       "CollectorConfig",
			"metadata": map[string]interface{}{
				"name":      "merged-collector-config",
				"namespace": "test-ns",
			},
			"spec": map[string]interface{}{
				"collectionRules": []interface{}{
					map[string]interface{}{
						"action": "include",
						"resourceSelector": map[string]interface{}{
							"apiGroups": []interface{}{""},
							"kinds":     []interface{}{"Pod"},
						},
						"fields": []interface{}{
							map[string]interface{}{
								"name":     "dnsPolicy",
								"jsonPath": "{.spec.dnsPolicy}",
							},
						},
					},
					map[string]interface{}{
						"action": "include",
						"resourceSelector": map[string]interface{}{
							"apiGroups": []interface{}{"apps"},
							"kinds":     []interface{}{"Deployment"},
						},
						"collectConditions": true,
					},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	fakeClient := fake.NewSimpleDynamicClient(scheme, collectionConfig)

	result := ReloadAndDiff(fakeClient)
	assert.NotNil(t, result)
	sort.Strings(result.AffectedResources)
	assert.Contains(t, result.AffectedResources, "Pod")
	assert.Contains(t, result.AffectedResources, "Deployment.apps")
}

func TestReloadAndDiff_ExcludeRulesAdded(t *testing.T) {
	saveAndRestoreConfigState(t)

	mergedTransformConfigMu.Lock()
	mergedTransformConfig = deepCopyTransformConfig(defaultTransformConfig)
	excludeRules = nil
	mergedTransformConfigMu.Unlock()

	collectionConfig := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "search.open-cluster-management.io/v1alpha1",
			"kind":       "CollectorConfig",
			"metadata": map[string]interface{}{
				"name":      "merged-collector-config",
				"namespace": "test-ns",
			},
			"spec": map[string]interface{}{
				"collectionRules": []interface{}{
					map[string]interface{}{
						"action": "exclude",
						"resourceSelector": map[string]interface{}{
							"apiGroups": []interface{}{""},
							"kinds":     []interface{}{"Secret"},
						},
					},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	fakeClient := fake.NewSimpleDynamicClient(scheme, collectionConfig)

	result := ReloadAndDiff(fakeClient)
	assert.NotNil(t, result)
	assert.True(t, result.ExcludeRulesChanged, "exclude rules should be detected as changed")
}

func TestReloadAndDiff_ExcludeRulesRemoved(t *testing.T) {
	saveAndRestoreConfigState(t)

	mergedTransformConfigMu.Lock()
	mergedTransformConfig = deepCopyTransformConfig(defaultTransformConfig)
	excludeRules = []excludeRule{
		{apiGroups: []string{""}, kinds: []string{"Secret"}, action: "exclude"},
	}
	mergedTransformConfigMu.Unlock()

	// No CR present — reload will clear exclude rules to nil.
	scheme := runtime.NewScheme()
	fakeClient := fake.NewSimpleDynamicClient(scheme)

	result := ReloadAndDiff(fakeClient)
	assert.NotNil(t, result)
	assert.True(t, result.ExcludeRulesChanged, "removing exclude rules should be detected as changed")
}

func TestSnapshotExcludeRules(t *testing.T) {
	origRules := excludeRules
	defer func() {
		mergedTransformConfigMu.Lock()
		excludeRules = origRules
		mergedTransformConfigMu.Unlock()
	}()

	mergedTransformConfigMu.Lock()
	excludeRules = []excludeRule{
		{apiGroups: []string{""}, kinds: []string{"Secret"}, action: "exclude"},
		{apiGroups: []string{"apps"}, kinds: []string{"*"}, action: "exclude"},
	}
	mergedTransformConfigMu.Unlock()

	snapshot := snapshotExcludeRules()
	assert.Equal(t, 2, len(snapshot))

	// Mutate snapshot — should not affect global.
	snapshot[0].action = "include"
	mergedTransformConfigMu.RLock()
	assert.Equal(t, excludeRule{apiGroups: []string{""}, kinds: []string{"Secret"}, action: "exclude"}, excludeRules[0])
	mergedTransformConfigMu.RUnlock()
}

func TestExcludeRulesChanged(t *testing.T) {
	ruleA := []excludeRule{{apiGroups: []string{""}, kinds: []string{"Secret"}, action: "exclude"}}
	ruleB := []excludeRule{{apiGroups: []string{""}, kinds: []string{"Pod"}, action: "exclude"}}

	assert.False(t, excludeRulesChanged(nil, nil))
	assert.False(t, excludeRulesChanged(ruleA, ruleA))
	assert.True(t, excludeRulesChanged(nil, ruleA))
	assert.True(t, excludeRulesChanged(ruleA, nil))
	assert.True(t, excludeRulesChanged(ruleA, ruleB))
}
