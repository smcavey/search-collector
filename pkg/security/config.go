// Copyright Contributors to the Open Cluster Management project

package security

import (
	"context"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	defaultConfigMapName = "search-security-config"
	configDataKey        = "config.yaml"
	pollInterval         = 30 * time.Second
)

// ConfigLoader watches a ConfigMap and keeps the SecurityConfig up to date.
type ConfigLoader struct {
	client    kubernetes.Interface
	namespace string
	configMu  sync.RWMutex
	config    *SecurityConfig
	stopCh    chan struct{}
}

// NewConfigLoader creates a ConfigLoader that polls the given namespace for the security ConfigMap.
func NewConfigLoader(client kubernetes.Interface, namespace string) *ConfigLoader {
	return &ConfigLoader{
		client:    client,
		namespace: namespace,
		config:    &SecurityConfig{Enabled: false},
		stopCh:    make(chan struct{}),
	}
}

// Start begins polling the ConfigMap for changes.
func (cl *ConfigLoader) Start() {
	cl.load() // load once immediately
	go cl.poll()
}

// Stop halts the polling loop.
func (cl *ConfigLoader) Stop() {
	close(cl.stopCh)
}

// GetConfig returns the current security configuration.
func (cl *ConfigLoader) GetConfig() *SecurityConfig {
	cl.configMu.RLock()
	defer cl.configMu.RUnlock()
	return cl.config
}

func (cl *ConfigLoader) poll() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cl.load()
		case <-cl.stopCh:
			return
		}
	}
}

func (cl *ConfigLoader) load() {
	cm, err := cl.client.CoreV1().ConfigMaps(cl.namespace).Get(
		context.Background(), defaultConfigMapName, metav1.GetOptions{},
	)
	if err != nil {
		klog.V(4).Infof("Security config ConfigMap not found, scanner disabled: %v", err)
		cl.configMu.Lock()
		cl.config = &SecurityConfig{Enabled: false}
		cl.configMu.Unlock()
		return
	}

	data, ok := cm.Data[configDataKey]
	if !ok {
		klog.Warningf("Security config ConfigMap missing key %q", configDataKey)
		cl.configMu.Lock()
		cl.config = &SecurityConfig{Enabled: false}
		cl.configMu.Unlock()
		return
	}

	var cfg SecurityConfig
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		klog.Errorf("Failed to parse security config: %v", err)
		return // keep previous config on parse error
	}

	cl.configMu.Lock()
	cl.config = &cfg
	cl.configMu.Unlock()
	klog.V(4).Infof("Loaded security config: enabled=%t, checks=%d", cfg.Enabled, len(cfg.Checks))
}

// ParseConfig parses a YAML string into a SecurityConfig. Useful for testing.
func ParseConfig(data string) (*SecurityConfig, error) {
	var cfg SecurityConfig
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
