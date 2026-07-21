// Copyright Contributors to the Open Cluster Management project

package config

import (
	"context"
	"crypto/tls"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	openshifttls "github.com/openshift/controller-runtime-common/pkg/tls"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

const (
	tlsProfileConfigMap = "ocm-tls-profile"
	tlsProfileNamespace = "open-cluster-management-agent-addon"
)

// GetTLSConfig reads the ocm-tls-profile ConfigMap and returns a *tls.Config.
// Falls back to the OpenShift Intermediate TLS profile if the ConfigMap can't be read.
func GetTLSConfig() *tls.Config {
	cm, err := GetKubeClient(GetKubeConfig()).CoreV1().ConfigMaps(tlsProfileNamespace).
		Get(context.TODO(), tlsProfileConfigMap, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Could not read ConfigMap %s/%s, falling back to Intermediate profile: %v",
			tlsProfileNamespace, tlsProfileConfigMap, err)
		return intermediateProfileTLSConfig()
	}

	return tlsConfigFromConfigMap(cm)
}

// intermediateProfileTLSConfig returns a *tls.Config matching the OpenShift Intermediate TLS profile.
func intermediateProfileTLSConfig() *tls.Config {
	profile := configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
	tlsConfigFn, unsupported := openshifttls.NewTLSConfigFromProfile(*profile)
	if len(unsupported) > 0 {
		klog.Warningf("Intermediate profile: cipher suites not supported by Go, skipped: %v", unsupported)
	}

	cfg := &tls.Config{} // #nosec G402 - MinVersion is set by tlsConfigFn from the Intermediate profile.
	tlsConfigFn(cfg)

	klog.Infof("Using fallback Intermediate TLS profile: min version TLS 1.%d, %d cipher suites",
		(cfg.MinVersion&0xff)-1, len(cfg.CipherSuites))

	return cfg
}

// tlsConfigFromConfigMap parses the ocm-tls-profile ConfigMap data into a *tls.Config.
func tlsConfigFromConfigMap(cm *corev1.ConfigMap) *tls.Config {
	minVersion := parseTLSVersion(cm.Data["minTLSVersion"])
	cipherSuites := cipherSuitesFromNames(cm.Data["cipherSuites"])

	klog.Infof("TLS profile %q: min version TLS 1.%d, %d cipher suites",
		cm.Data["profileType"], (minVersion&0xff)-1, len(cipherSuites))

	cfg := &tls.Config{
		MinVersion: minVersion, // #nosec G402 - TLS version is set by the cluster's APIServer TLS profile.
	}
	if len(cipherSuites) > 0 {
		cfg.CipherSuites = cipherSuites
	}
	return cfg
}

// parseTLSVersion converts an OpenShift version string (e.g. "VersionTLS12") to a crypto/tls uint16.
func parseTLSVersion(v string) uint16 {
	switch v {
	case "VersionTLS10":
		return tls.VersionTLS10
	case "VersionTLS11":
		return tls.VersionTLS11
	case "VersionTLS12":
		return tls.VersionTLS12
	case "VersionTLS13":
		return tls.VersionTLS13
	default:
		klog.Warningf("Unknown TLS version %q, defaulting to TLS 1.2", v)
		return tls.VersionTLS12
	}
}

// cipherSuitesFromNames resolves IANA cipher suite names to crypto/tls uint16 IDs using Go's stdlib
func cipherSuitesFromNames(ciphers string) []uint16 {
	if ciphers == "" {
		return nil
	}

	lookup := map[string]uint16{}
	for _, cs := range tls.CipherSuites() {
		lookup[cs.Name] = cs.ID
	}
	for _, cs := range tls.InsecureCipherSuites() {
		lookup[cs.Name] = cs.ID
	}

	var result []uint16
	var unknown []string
	for _, name := range strings.Split(ciphers, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if id, ok := lookup[name]; ok {
			result = append(result, id)
		} else {
			unknown = append(unknown, name)
		}
	}

	if len(unknown) > 0 {
		klog.Warningf("TLS cipher suites not recognized by Go, skipped: %v", unknown)
	}

	return result
}
