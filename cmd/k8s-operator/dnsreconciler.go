// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// tailscale-operator provides a way to expose services running in a Kubernetes
// cluster to your Tailnet.
package main

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	defaultClusterDNSNamespace = "kube-system"
	// https://github.com/kubernetes/dns/blob/84c944bab691cb5c85da1a39c9232ce59de209a4/pkg/dns/config/config.go#L49
	kubeDNSStubDomainsKey = "stubDomains"
	coreDNSCorefileKey    = "Corefile"
	tsNetKey              = "ts.net"
)

var (
	// If you add a new one here also update the operator's RBAC
	// TODO (irbekrm): make it possible for users to configure configmap
	// names/namespaces via an operator flag
	knownKubeDNSConfigMapNames = []string{"kube-dns"}
	// CoreDNS Helm chart generates this name depending on what name users
	// have given to CoreDNS release. By default this will be
	// 'coredns-coredns'
	// https://github.com/coredns/helm/blob/562d3c8809db9edbad89ae2006a4bd81c34b7b8f/charts/coredns/templates/configmap.yaml#L6
	knownCoreDNSConfigMapNames = []string{"coredns", "coredns-coredns"}
)

// dnsReconciler knows how to update common cluster DNS setups to add a stub ts.net
// nameserver
type dnsReconciler struct {
	client.Client
	operatorNamespace string
	logger            *zap.SugaredLogger
}

func (r *dnsReconciler) Reconcile(ctx context.Context, req reconcile.Request) (res reconcile.Result, err error) {
	res = reconcile.Result{}
	key := req.NamespacedName

	svc := &corev1.Service{}
	// get the ts.net nameserver service, check if it has cluster IP set
	err = r.Get(ctx, key, svc)
	if apierrors.IsNotFound(err) {
		r.logger.Info("nameserver Service not found, waiting...")
		return res, nil
	}
	if err != nil {
		r.logger.Error("error retrieving nameserver Service: %v", err)
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
		r.logger.Info("namserver Service not yet ready, waiting...")
		return res, nil
	}

	// We don't have a reliable way how to determine what DNS the cluster is
	// actually using so we just try to find and modify kube-dns/CoreDNS
	// configs
	kubeDNSCM := &corev1.ConfigMap{}
	kubeDNSFound := false
	for _, cmName := range knownKubeDNSConfigMapNames {
		nsName := types.NamespacedName{Name: cmName, Namespace: defaultClusterDNSNamespace}
		err := r.Get(ctx, nsName, kubeDNSCM)
		if apierrors.IsNotFound(err) {
			r.logger.Debugf("looking for kube-dns config, configmap %s/%s not found", defaultClusterDNSNamespace, cmName)
			continue
		}
		if err != nil {
			r.logger.Errorf("error trying to retrieve kube-dns config: %v", err)
			return res, err
		}
		r.logger.Info("kube-dns config found in configmap %s/%s", defaultClusterDNSNamespace, cmName)
		kubeDNSFound = true
		// presumably there will ever only be one
		break
	}
	// it is possible that both kube-dns and CoreDNS are deployed and we
	// don't have a reliable way to tell which one is used, so update both
	coreDNSCM := &corev1.ConfigMap{}
	coreDNSFound := false
	for _, cmName := range knownCoreDNSConfigMapNames {
		nsName := types.NamespacedName{Name: cmName, Namespace: defaultClusterDNSNamespace}
		err := r.Get(ctx, nsName, coreDNSCM)
		if apierrors.IsNotFound(err) {
			r.logger.Debugf("looking for coreDNS config, configmap %s/%s not found", defaultClusterDNSNamespace, cmName)
			continue
		}
		if err != nil {
			r.logger.Errorf("error trying to retrieve CoreDNS config: %v", err)
			return res, err
		}
		r.logger.Info("CoreDNS config found in configmap %s/%s", defaultClusterDNSNamespace, cmName)
		coreDNSFound = true
		// presumably there will ever only be one
		break
	}

	if !kubeDNSFound && !coreDNSFound {
		r.logger.Info("neither kube-dns nor CoreDNS config was found. Users who want to use Tailscale egress will need to configure ts.net DNS manually")
		return res, nil
	}

	if kubeDNSFound {
		r.logger.Info("ensuring that kube-dns config in ConfigMap %s/%s contains ts.net stub nameserver", defaultClusterDNSNamespace, kubeDNSCM)

		stubDomains := make(map[string][]string)
		if _, ok := kubeDNSCM.Data[kubeDNSStubDomainsKey]; ok {
			err = json.Unmarshal([]byte(kubeDNSCM.Data[kubeDNSStubDomainsKey]), &stubDomains)
			if err != nil {
				r.logger.Errorf("error unmarshalling kube-dns config: %v", err)
				return res, err
			}
		}
		if _, ok := stubDomains[tsNetKey]; !ok {
			stubDomains[tsNetKey] = make([]string, 1)
		}
		if stubDomains[tsNetKey][0] != svc.Spec.ClusterIP {
			stubDomains[tsNetKey][0] = svc.Spec.ClusterIP
			stubDomainsBytes, err := json.Marshal(stubDomains)
			if err != nil {
				r.logger.Errorf("error marshaling stub domains: %v", err)
				return res, err
			}
			kubeDNSCM.Data[kubeDNSStubDomainsKey] = string(stubDomainsBytes)
			if err := r.Update(ctx, kubeDNSCM); err != nil {
				r.logger.Errorf("error updating kube-dns config: %v", err)
			}
			r.logger.Info("kube-dns config in ConfigMap %s/%s updated with ts.net stubserver at %s", defaultClusterDNSNamespace, kubeDNSCM.Name, svc.Spec.ClusterIP)
		} else {
			r.logger.Debugf("kube-dns config in ConfigMap %s/%s already up to date with ts.net stubserver at %s", defaultClusterDNSNamespace, kubeDNSCM.Name, svc.Spec.ClusterIP)
		}
	}

	if coreDNSFound {
		// coreDNS does not appear to have defaults where it doesn't need Corefile to
		// contain some configuration, so if this is unset something is off and we don't
		// know what to do
		if _, ok := coreDNSCM.Data[coreDNSCorefileKey]; !ok {
			r.logger.Info("found what appears to be a core-dns config in ConfigMap %s/%s, but it does not contain a Corefile, do nothing", defaultClusterDNSNamespace, coreDNSCM.Name)
			return res, nil
		}
		// unmarshal Corefile

	}

	return res, nil
}
