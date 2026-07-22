// Copyright Contributors to the Open Cluster Management project

package config

import (
	"context"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

const tlsPollInterval = 60 * time.Second

// configMapGetter abstracts ConfigMap reads for testability.
type configMapGetter interface {
	Get(ctx context.Context, name, namespace string) (*corev1.ConfigMap, error)
}

// kubeConfigMapGetter reads ConfigMaps from the Kubernetes API.
type kubeConfigMapGetter struct{}

func (k *kubeConfigMapGetter) Get(ctx context.Context, name, namespace string) (*corev1.ConfigMap, error) {
	return GetKubeClient(GetKubeConfig()).CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
}

// PollTLSProfileConfigMap polls the ocm-tls-profile ConfigMap and sends on the reload channel
// when the TLS profile data changes. Blocks until ctx is canceled.
func PollTLSProfileConfigMap(ctx context.Context, reload chan<- struct{}) {
	pollTLSProfile(ctx, reload, &kubeConfigMapGetter{}, tlsPollInterval)
}

// pollTLSProfile is the testable core of PollTLSProfileConfigMap.
func pollTLSProfile(ctx context.Context, reload chan<- struct{}, getter configMapGetter, interval time.Duration) {
	// Read initial state. If unavailable, keep polling until it appears.
	var lastData map[string]string
	cm, err := getter.Get(ctx, tlsProfileConfigMap, tlsProfileNamespace)
	if err != nil {
		klog.Warningf("Could not read initial %s/%s ConfigMap, will keep polling: %v",
			tlsProfileNamespace, tlsProfileConfigMap, err)
	} else {
		lastData = cm.Data
	}
	klog.Infof("TLS profile poller started, polling every %s", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.Info("TLS profile poller stopped")
			return
		case <-ticker.C:
			cm, err := getter.Get(ctx, tlsProfileConfigMap, tlsProfileNamespace)
			if err != nil {
				klog.Warningf("Error polling %s/%s ConfigMap: %v",
					tlsProfileNamespace, tlsProfileConfigMap, err)
				continue
			}
			if !reflect.DeepEqual(lastData, cm.Data) {
				klog.Info("TLS profile changed, signaling client reload")
				lastData = cm.Data
				select {
				case reload <- struct{}{}:
				default:
					// Channel already has a pending signal, skip.
				}
			}
		}
	}
}
