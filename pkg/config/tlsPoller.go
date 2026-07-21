// Copyright Contributors to the Open Cluster Management project

package config

import (
	"context"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

const tlsPollInterval = 60 * time.Second

// PollTLSProfileConfigMap polls the ocm-tls-profile ConfigMap and sends on the reload channel
// when the TLS profile data changes. Blocks until ctx is canceled.
func PollTLSProfileConfigMap(ctx context.Context, reload chan<- struct{}) {
	kubeClient := GetKubeClient(GetKubeConfig())

	// Read initial state.
	cm, err := kubeClient.CoreV1().ConfigMaps(tlsProfileNamespace).
		Get(ctx, tlsProfileConfigMap, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Could not read initial %s/%s ConfigMap, TLS poller disabled: %v",
			tlsProfileNamespace, tlsProfileConfigMap, err)
		return
	}
	lastData := cm.Data
	klog.Infof("TLS profile poller started, polling every %s", tlsPollInterval)

	ticker := time.NewTicker(tlsPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.Info("TLS profile poller stopped")
			return
		case <-ticker.C:
			cm, err := kubeClient.CoreV1().ConfigMaps(tlsProfileNamespace).
				Get(ctx, tlsProfileConfigMap, metav1.GetOptions{})
			if err != nil {
				klog.Warningf("Error polling %s/%s ConfigMap: %v",
					tlsProfileNamespace, tlsProfileConfigMap, err)
				continue
			}
			if !reflect.DeepEqual(lastData, cm.Data) {
				klog.Infof("TLS profile changed (was %s, now %s), signaling client reload",
					lastData["profileType"], cm.Data["profileType"])
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
