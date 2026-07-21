/*
IBM Confidential
OCO Source Materials
(C) Copyright IBM Corporation 2019 All Rights Reserved
The source code for this program is not published or otherwise divested of its trade secrets,
irrespective of what has been deposited with the U.S. Copyright Office.
*/
// Copyright (c) 2020 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package send

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"

	"k8s.io/klog/v2"

	"github.com/stolostron/search-collector/pkg/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured/unstructuredscheme"

	"k8s.io/client-go/rest"
)

func getHTTPSClient() (client http.Client) {
	// Read TLS profile from ocm-tls-profile ConfigMap (same for hub and managed cluster).
	tlsCfg := config.GetTLSConfig()

	// Klusterlet deployment: Get httpClient using the mounted kubeconfig.
	if !config.Cfg.DeployedInHub {
		config.Cfg.AggregatorConfig.NegotiatedSerializer = unstructuredscheme.NewUnstructuredNegotiatedSerializer()

		// Inject TLS profile settings into the rest.Config transport.
		config.Cfg.AggregatorConfig.Wrap(func(rt http.RoundTripper) http.RoundTripper {
			if t, ok := rt.(*http.Transport); ok && t.TLSClientConfig != nil {
				t.TLSClientConfig.MinVersion = tlsCfg.MinVersion
				t.TLSClientConfig.CipherSuites = tlsCfg.CipherSuites
			}
			return rt
		})

		aggregatorRESTClient, err := rest.UnversionedRESTClientFor(config.Cfg.AggregatorConfig)
		if err != nil {
			// Exit because this is an unrecoverable configuration problem.
			klog.Fatal("Error getting httpClient from kubeconfig. Original error: ", err)
		}
		client = *(aggregatorRESTClient.Client)
		return client
	}

	// Hub deployment:
	// Load mounted certificates. If not found, use insecure TLS (development only).
	caCert, err := os.ReadFile("./sslcert/tls.crt")
	cert, err2 := tls.LoadX509KeyPair("./sslcert/tls.crt", "./sslcert/tls.key")
	if err != nil || err2 != nil {
		klog.Error("WARNING: Using insecure TLS connection. Couldn't load certs ", err, err2)
		tlsCfg.InsecureSkipVerify = true
	} else {
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		tlsCfg.RootCAs = caCertPool
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	tr := &http.Transport{
		TLSClientConfig: tlsCfg,
	}

	return http.Client{Transport: tr}
}
