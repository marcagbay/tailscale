// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// tailscale-operator provides a way to expose services running in a Kubernetes
// cluster to your Tailnet.
package main

import (
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/exp/slices"
	"golang.org/x/oauth2/clientcredentials"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/transport"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	kzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"
	"tailscale.com/client/tailscale"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn"
	"tailscale.com/ipn/store/kubestore"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
	"tailscale.com/types/opt"
	"tailscale.com/util/dnsname"
	"tailscale.com/version"
)

const (
	dnsConfigKey     = "dns.json"
	dnsConfigMapName = "dnsconfig"
)

func main() {
	// Required to use our client API. We're fine with the instability since the
	// client lives in the same repo as this code.
	tailscale.I_Acknowledge_This_API_Is_Unstable = true

	var (
		hostname           = defaultEnv("OPERATOR_HOSTNAME", "tailscale-operator")
		kubeSecret         = defaultEnv("OPERATOR_SECRET", "")
		operatorTags       = defaultEnv("OPERATOR_INITIAL_TAGS", "tag:k8s-operator")
		tsNamespace        = defaultEnv("OPERATOR_NAMESPACE", "")
		tslogging          = defaultEnv("OPERATOR_LOGGING", "info")
		clientIDPath       = defaultEnv("CLIENT_ID_FILE", "")
		clientSecretPath   = defaultEnv("CLIENT_SECRET_FILE", "")
		image              = defaultEnv("PROXY_IMAGE", "tailscale/tailscale:latest")
		priorityClassName  = defaultEnv("PROXY_PRIORITY_CLASS_NAME", "")
		tags               = defaultEnv("PROXY_TAGS", "tag:k8s")
		shouldRunAuthProxy = defaultBool("AUTH_PROXY", false)
	)

	var opts []kzap.Opts
	switch tslogging {
	case "info":
		opts = append(opts, kzap.Level(zapcore.InfoLevel))
	case "debug":
		opts = append(opts, kzap.Level(zapcore.DebugLevel))
	case "dev":
		opts = append(opts, kzap.UseDevMode(true), kzap.Level(zapcore.DebugLevel))
	}
	zlog := kzap.NewRaw(opts...).Sugar()
	logf.SetLogger(zapr.NewLogger(zlog.Desugar()))
	startlog := zlog.Named("startup")

	if clientIDPath == "" || clientSecretPath == "" {
		startlog.Fatalf("CLIENT_ID_FILE and CLIENT_SECRET_FILE must be set")
	}
	clientID, err := os.ReadFile(clientIDPath)
	if err != nil {
		startlog.Fatalf("reading client ID %q: %v", clientIDPath, err)
	}
	clientSecret, err := os.ReadFile(clientSecretPath)
	if err != nil {
		startlog.Fatalf("reading client secret %q: %v", clientSecretPath, err)
	}
	credentials := clientcredentials.Config{
		ClientID:     string(clientID),
		ClientSecret: string(clientSecret),
		TokenURL:     "https://login.tailscale.com/api/v2/oauth/token",
	}
	tsClient := tailscale.NewClient("-", nil)
	tsClient.HTTPClient = credentials.Client(context.Background())

	if shouldRunAuthProxy {
		hostinfo.SetApp("k8s-operator-proxy")
	} else {
		hostinfo.SetApp("k8s-operator")
	}

	s := &tsnet.Server{
		Hostname: hostname,
		Logf:     zlog.Named("tailscaled").Debugf,
	}
	if kubeSecret != "" {
		st, err := kubestore.New(logger.Discard, kubeSecret)
		if err != nil {
			startlog.Fatalf("creating kube store: %v", err)
		}
		s.Store = st
	}
	if err := s.Start(); err != nil {
		startlog.Fatalf("starting tailscale server: %v", err)
	}
	defer s.Close()
	lc, err := s.LocalClient()
	if err != nil {
		startlog.Fatalf("getting local client: %v", err)
	}

	ctx := context.Background()
	loginDone := false
	machineAuthShown := false
waitOnline:
	for {
		startlog.Debugf("querying tailscaled status")
		st, err := lc.StatusWithoutPeers(ctx)
		if err != nil {
			startlog.Fatalf("getting status: %v", err)
		}
		switch st.BackendState {
		case "Running":
			break waitOnline
		case "NeedsLogin":
			if loginDone {
				break
			}
			caps := tailscale.KeyCapabilities{
				Devices: tailscale.KeyDeviceCapabilities{
					Create: tailscale.KeyDeviceCreateCapabilities{
						Reusable:      false,
						Preauthorized: true,
						Tags:          strings.Split(operatorTags, ","),
					},
				},
			}
			authkey, _, err := tsClient.CreateKey(ctx, caps)
			if err != nil {
				startlog.Fatalf("creating operator authkey: %v", err)
			}
			if err := lc.Start(ctx, ipn.Options{
				AuthKey: authkey,
			}); err != nil {
				startlog.Fatalf("starting tailscale: %v", err)
			}
			if err := lc.StartLoginInteractive(ctx); err != nil {
				startlog.Fatalf("starting login: %v", err)
			}
			startlog.Debugf("requested login by authkey")
			loginDone = true
		case "NeedsMachineAuth":
			if !machineAuthShown {
				startlog.Infof("Machine approval required, please visit the admin panel to approve")
				machineAuthShown = true
			}
		default:
			startlog.Debugf("waiting for tailscale to start: %v", st.BackendState)
		}
		time.Sleep(time.Second)
	}

	// For secrets and statefulsets, we only get permission to touch the objects
	// in the controller's own namespace. This cannot be expressed by
	// .Watches(...) below, instead you have to add a per-type field selector to
	// the cache that sits a few layers below the builder stuff, which will
	// implicitly filter what parts of the world the builder code gets to see at
	// all.
	nsFilter := cache.ByObject{
		Field: client.InNamespace(tsNamespace).AsSelector(),
	}

	tsNSCMName := cache.ByObject{
		Field: fields.SelectorFromSet(fields.Set{"metadata.name": dnsConfigMapName}),
	}

	// build cache filter for ConfigMaps. We only want to watch the one that
	// holds ts.net nameserver config + the ones that might hold cluster DNS
	// config
	cmFilter := cache.ByObject{
		Namespaces: map[string]cache.Config{
			tsNamespace: {
				FieldSelector: tsNSCMName.Field,
			},
			defaultClusterDNSNamespace: {}, // it doesn't seem like we can OR a field selector so cache all configmaps in this namespace
		},
	}

	restConfig := config.GetConfigOrDie()
	mgr, err := manager.New(restConfig, manager.Options{
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Secret{}:      nsFilter,
				&appsv1.StatefulSet{}: nsFilter,
				&corev1.ConfigMap{}:   cmFilter,
			},
		},
	})
	if err != nil {
		startlog.Fatalf("could not create manager: %v", err)
	}

	sr := &ServiceReconciler{
		Client:                 mgr.GetClient(),
		tsClient:               tsClient,
		localClient:            lc,
		defaultTags:            strings.Split(tags, ","),
		operatorNamespace:      tsNamespace,
		proxyImage:             image,
		proxyPriorityClassName: priorityClassName,
		logger:                 zlog.Named("proxy-reconciler"),
	}

	reconcileFilter := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		ls := o.GetLabels()
		if ls[LabelManaged] != "true" {
			return nil
		}
		if ls[LabelParentType] != "svc" {
			return nil
		}
		return []reconcile.Request{
			{
				NamespacedName: types.NamespacedName{
					Namespace: ls[LabelParentNamespace],
					Name:      ls[LabelParentName],
				},
			},
		}
	})

	cmEventHandler := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		// currently we cache the hosts configmap, but do not trigger
		// reconciles if it has changed so any manual modifications to
		// it will not always be immediately overriden. Probably
		// eventually we want to do that- but make sure our reconciles
		// are not too expensive first
		return nil
	})

	err = builder.
		ControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Watches(&appsv1.StatefulSet{}, reconcileFilter).
		Watches(&corev1.Secret{}, reconcileFilter).
		Watches(&corev1.ConfigMap{}, cmEventHandler).
		Complete(sr)
	if err != nil {
		startlog.Fatalf("could not create proxy reconciler: %v", err)
	}

	dnsR := &dnsReconciler{
		logger: zlog.Named("dns-reconciler"),
	}
	err = builder.
		ControllerManagedBy(mgr).
		Watches(&corev1.Service{}, nil).
		Watches(&corev1.ConfigMap{}, nil).
		Complete(dnsR)
	if err != nil {
		startlog.Fatalf("could not create dns reconciler: %v", err)
	}

	startlog.Infof("Startup complete, operator running, version: %s", version.Long())
	if shouldRunAuthProxy {
		cfg, err := restConfig.TransportConfig()
		if err != nil {
			startlog.Fatalf("could not get rest.TransportConfig(): %v", err)
		}

		// Kubernetes uses SPDY for exec and port-forward, however SPDY is
		// incompatible with HTTP/2; so disable HTTP/2 in the proxy.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig, err = transport.TLSConfigFor(cfg)
		if err != nil {
			startlog.Fatalf("could not get transport.TLSConfigFor(): %v", err)
		}
		tr.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)

		rt, err := transport.HTTPWrappersForConfig(cfg, tr)
		if err != nil {
			startlog.Fatalf("could not get rest.TransportConfig(): %v", err)
		}
		go runAuthProxy(s, rt, zlog.Named("auth-proxy").Infof)
	}
	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		startlog.Fatalf("could not start manager: %v", err)
	}
}

const (
	LabelManaged         = "tailscale.com/managed"
	LabelParentType      = "tailscale.com/parent-resource-type"
	LabelParentName      = "tailscale.com/parent-resource"
	LabelParentNamespace = "tailscale.com/parent-resource-ns"

	FinalizerName = "tailscale.com/finalizer"

	AnnotationExpose   = "tailscale.com/expose"
	AnnotationTags     = "tailscale.com/tags"
	AnnotationHostname = "tailscale.com/hostname"

	AnnotationTargetIP = "tailscale.com/target-ip"
)

// ServiceReconciler is a simple ControllerManagedBy example implementation.
type ServiceReconciler struct {
	client.Client
	tsClient               tsClient
	localClient            localClient
	defaultTags            []string
	operatorNamespace      string
	proxyImage             string
	proxyPriorityClassName string
	logger                 *zap.SugaredLogger
}

type tsClient interface {
	CreateKey(ctx context.Context, caps tailscale.KeyCapabilities) (string, *tailscale.Key, error)
	DeleteDevice(ctx context.Context, id string) error
}

type localClient interface {
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
}

func childResourceLabels(parent *corev1.Service) map[string]string {
	// You might wonder why we're using owner references, since they seem to be
	// built for exactly this. Unfortunately, Kubernetes does not support
	// cross-namespace ownership, by design. This means we cannot make the
	// service being exposed the owner of the implementation details of the
	// proxying. Instead, we have to do our own filtering and tracking with
	// labels.
	return map[string]string{
		LabelManaged:         "true",
		LabelParentName:      parent.GetName(),
		LabelParentNamespace: parent.GetNamespace(),
		LabelParentType:      "svc",
	}
}

func (a *ServiceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (_ reconcile.Result, err error) {
	logger := a.logger.With("service-ns", req.Namespace, "service-name", req.Name)
	logger.Debugf("starting reconcile")
	defer logger.Debugf("reconcile finished")

	svc := new(corev1.Service)
	err = a.Get(ctx, req.NamespacedName, svc)
	if apierrors.IsNotFound(err) {
		// Request object not found, could have been deleted after reconcile request.
		logger.Debugf("service not found, assuming it was deleted")
		return reconcile.Result{}, nil
	} else if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get svc: %w", err)
	}
	if !svc.DeletionTimestamp.IsZero() || (!a.shouldExpose(svc) && !a.hasTargetAnnotation(svc)) {
		logger.Debugf("service is being deleted or should not be exposed, cleaning up")
		return reconcile.Result{}, a.maybeCleanup(ctx, logger, svc)
	}

	return reconcile.Result{}, a.maybeProvision(ctx, logger, svc)
}

// maybeCleanup removes any existing resources related to serving svc over tailscale.
//
// This function is responsible for removing the finalizer from the service,
// once all associated resources are gone.
func (a *ServiceReconciler) maybeCleanup(ctx context.Context, logger *zap.SugaredLogger, svc *corev1.Service) error {
	ix := slices.Index(svc.Finalizers, FinalizerName)
	if ix < 0 {
		logger.Debugf("no finalizer, nothing to do")
		return nil
	}

	// TODO (irbekrm): delete record from ts.net nameserver configmap

	ml := childResourceLabels(svc)

	// Need to delete the StatefulSet first, and delete it with foreground
	// cascading deletion. That way, the pod that's writing to the Secret will
	// stop running before we start looking at the Secret's contents, and
	// assuming k8s ordering semantics don't mess with us, that should avoid
	// tailscale device deletion races where we fail to notice a device that
	// should be removed.
	sts, err := getSingleObject[appsv1.StatefulSet](ctx, a.Client, a.operatorNamespace, ml)
	if err != nil {
		return fmt.Errorf("getting statefulset: %w", err)
	}
	if sts != nil {
		if !sts.GetDeletionTimestamp().IsZero() {
			// Deletion in progress, check again later. We'll get another
			// notification when the deletion is complete.
			logger.Debugf("waiting for statefulset %s/%s deletion", sts.GetNamespace(), sts.GetName())
			return nil
		}
		err := a.DeleteAllOf(ctx, &appsv1.StatefulSet{}, client.InNamespace(a.operatorNamespace), client.MatchingLabels(ml), client.PropagationPolicy(metav1.DeletePropagationForeground))
		if err != nil {
			return fmt.Errorf("deleting statefulset: %w", err)
		}
		logger.Debugf("started deletion of statefulset %s/%s", sts.GetNamespace(), sts.GetName())
		return nil
	}

	id, _, err := a.getDeviceInfo(ctx, svc)
	if err != nil {
		return fmt.Errorf("getting device info: %w", err)
	}
	if id != "" {
		// TODO: handle case where the device is already deleted, but the secret
		// is still around.
		if err := a.tsClient.DeleteDevice(ctx, id); err != nil {
			return fmt.Errorf("deleting device: %w", err)
		}
	}

	types := []client.Object{
		&corev1.Service{},
		&corev1.Secret{},
	}
	for _, typ := range types {
		if err := a.DeleteAllOf(ctx, typ, client.InNamespace(a.operatorNamespace), client.MatchingLabels(ml)); err != nil {
			return err
		}
	}

	svc.Finalizers = append(svc.Finalizers[:ix], svc.Finalizers[ix+1:]...)
	if err := a.Update(ctx, svc); err != nil {
		return fmt.Errorf("failed to remove finalizer: %w", err)
	}

	// Unlike most log entries in the reconcile loop, this will get printed
	// exactly once at the very end of cleanup, because the final step of
	// cleanup removes the tailscale finalizer, which will make all future
	// reconciles exit early.
	logger.Infof("unexposed service from tailnet")
	return nil
}

// TODO(maisem): XXXXXXXXXXXXXXXXXXX update docs
// maybeProvision ensures that svc is exposed over tailscale, taking any actions
// necessary to reach that state.
//
// This function adds a finalizer to svc, ensuring that we can handle orderly
// deprovisioning later.
func (a *ServiceReconciler) maybeProvision(ctx context.Context, logger *zap.SugaredLogger, svc *corev1.Service) error {
	hostname, err := nameForService(svc)
	if err != nil {
		return err
	}

	if !slices.Contains(svc.Finalizers, FinalizerName) {
		// This log line is printed exactly once during initial provisioning,
		// because once the finalizer is in place this block gets skipped. So,
		// this is a nice place to tell the operator that the high level,
		// multi-reconcile operation is underway.
		logger.Infof("exposing service over tailscale")
		svc.Finalizers = append(svc.Finalizers, FinalizerName)
		if err := a.Update(ctx, svc); err != nil {
			return fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	// Create either headless or ClusterIP service depending on whether
	// we are provisioning for ingress or egress
	proxySvc, err := a.reconcileProxyService(ctx, logger, svc)
	if err != nil {
		return fmt.Errorf("failed to reconcile headless service: %w", err)
	}

	tags := a.defaultTags
	if tstr, ok := svc.Annotations[AnnotationTags]; ok {
		tags = strings.Split(tstr, ",")
	}
	secretName, err := a.createOrGetSecret(ctx, logger, svc, proxySvc, tags)
	if err != nil {
		return fmt.Errorf("failed to create or get API key secret: %w", err)
	}
	_, err = a.reconcileSTS(ctx, logger, svc, proxySvc, secretName, hostname)
	if err != nil {
		return fmt.Errorf("failed to reconcile statefulset: %w", err)
	}

	if err := a.reconcileDNSConfig(ctx, logger, svc); err != nil {
		logger.Errorf("error reconciling DNS config: %v", err)
		return err
	}

	if !a.hasLoadBalancerClass(svc) {
		logger.Debugf("service is not a LoadBalancer, so not updating ingress")
		return nil
	}

	_, tsHost, err := a.getDeviceInfo(ctx, svc)
	if err != nil {
		return fmt.Errorf("failed to get device ID: %w", err)
	}
	if tsHost == "" {
		logger.Debugf("no Tailscale hostname known yet, waiting for proxy pod to finish auth")
		// No hostname yet. Wait for the proxy pod to auth.
		svc.Status.LoadBalancer.Ingress = nil
		if err := a.Status().Update(ctx, svc); err != nil {
			return fmt.Errorf("failed to update service status: %w", err)
		}
		return nil
	}

	logger.Debugf("setting ingress hostname to %q", tsHost)
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
		{
			Hostname: tsHost,
		},
	}
	if err := a.Status().Update(ctx, svc); err != nil {
		return fmt.Errorf("failed to update service status: %w", err)
	}
	return nil
}

func (a *ServiceReconciler) shouldExpose(svc *corev1.Service) bool {
	// Headless services can't be exposed, since there is no ClusterIP to
	// forward to.
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
		return false
	}

	return a.hasLoadBalancerClass(svc) || a.hasExposeAnnotation(svc)
}

func (a *ServiceReconciler) hasLoadBalancerClass(svc *corev1.Service) bool {
	return svc != nil &&
		svc.Spec.Type == corev1.ServiceTypeLoadBalancer &&
		svc.Spec.LoadBalancerClass != nil &&
		*svc.Spec.LoadBalancerClass == "tailscale"
}

func (a *ServiceReconciler) hasExposeAnnotation(svc *corev1.Service) bool {
	return svc != nil && svc.Annotations[AnnotationExpose] == "true"
}

func (a *ServiceReconciler) hasTargetAnnotation(svc *corev1.Service) bool {
	return svc != nil && svc.Annotations[AnnotationTargetIP] != ""
}

func (a *ServiceReconciler) reconcileProxyService(ctx context.Context, logger *zap.SugaredLogger, svc *corev1.Service) (*corev1.Service, error) {
	proxySvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "ts-" + svc.Name + "-",
			Namespace:    a.operatorNamespace,
			Labels:       childResourceLabels(svc),
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": string(svc.UID),
			},
		},
	}
	if a.hasTargetAnnotation(svc) {
		logger.Debugf("reconciling headless service for egress proxy StatefulSet")
		proxySvc.Spec.Type = "ClusterIP"
		proxySvc.Spec.Ports = []corev1.ServicePort{{Name: "http", Protocol: "TCP", Port: 80}, {Name: "https", Protocol: "TCP", Port: 443}}

	}
	if a.shouldExpose(svc) {
		logger.Debugf("reconciling ClusterIP service for ingress proxy StatefulSet")
		proxySvc.Spec.ClusterIP = "None"
	}
	return createOrUpdate(ctx, a.Client, a.operatorNamespace, proxySvc, func(svc *corev1.Service) { svc.Spec = proxySvc.Spec })
}

func (a *ServiceReconciler) reconcileDNSConfig(ctx context.Context, logger *zap.SugaredLogger, svc *corev1.Service) error {

	if !a.hasTargetAnnotation(svc) {
		// only do this for services that define an egress proxy
		return nil
	}
	ip := fmt.Sprintf("%s:0", svc.Annotations[AnnotationTargetIP])
	ar, err := a.localClient.WhoIs(ctx, ip)
	if err != nil {
		logger.Errorf("error identifying Tailscale service for %s: %v", ip, err)
		return err
	}
	// This should never be empty
	fqdn := ar.Node.Name

	logger.Debugf("ensuring a record for %s to hosts config...", fqdn)

	// Check if proxy Service already has been created and has a Cluster IP
	// assigned to it
	ml := childResourceLabels(svc)
	proxySvc, err := getSingleObject[corev1.Service](ctx, a.Client, a.operatorNamespace, ml)
	if apierrors.IsNotFound(err) {
		// we will reconcile again on proxy Service creation/update
		// event and the hosts config will get updated then
		logger.Debugf("proxy Service not yet created waiting...")
		return nil
	}
	if err != nil {
		logger.Errorf("error retrieving proxy Service: %v", err)
		return err
	}

	if proxySvc == nil || proxySvc.Spec.ClusterIP == "" || proxySvc.Spec.ClusterIP == "None" {
		// we will reconcile again on proxy Service creation/update
		// event and the hosts config will get updated then
		logger.Infof("proxy Service for %s not yet ready, waiting...", fqdn)
		return nil
	}

	cm := &corev1.ConfigMap{}
	err = a.Get(ctx, types.NamespacedName{Name: dnsConfigMapName, Namespace: a.operatorNamespace}, cm)
	if err != nil {
		logger.Errorf("error retrieving hosts config: %v", err)
		return err
	}

	hosts := make(map[string]string)
	if cm.Data[dnsConfigKey] != "" {
		if err := json.Unmarshal([]byte(cm.Data[dnsConfigKey]), &hosts); err != nil {
			logger.Errorf("error unmarshaling hosts config %v", err)
			return err
		}
	}
	hosts[fqdn] = proxySvc.Spec.ClusterIP
	hostsBytes, err := json.Marshal(hosts)
	if err != nil {
		logger.Errorf("error marshaling hosts config %v", err)
		return err
	}

	cm.Data[dnsConfigKey] = string(hostsBytes)

	// TODO (irbekrm): probably better to SSA here
	// TODO (irbekrm): check diff only apply update if needed
	if err := a.Update(ctx, cm); err != nil {
		logger.Errorf("failed to update ts.net DNS config: %v", err)
		return err
	}

	// TODO (irbekrm): run some kind of test here to ensure that the service
	// can actually be reached from cluster - i.e spin up a pod and try to
	// curl the service

	// If we get to this point we've done all we are currently doing in code
	// to set up DNS, so let's update spec.ExternaName of the user's Service
	// with Tailscale service FQDN. This is not required for things to work,
	// but is a pointer that users should use this FQDN to refer to the
	// Tailscale service from their cluster workloads.
	if svc.Spec.ExternalName != fqdn || svc.Spec.Type != corev1.ServiceTypeExternalName {
		logger.Debugf("updating service %s/%s to external Service pointing at Tailscale service FQDN", svc.Name, svc.Namespace)
		svc.Spec.ExternalName = fqdn
		svc.Spec.Type = corev1.ServiceTypeExternalName
		if err := a.Update(ctx, svc); err != nil {
			logger.Errorf("error updating Service %s: %v", svc.Name, err)
			return err
		}
	}
	return nil
}

func (a *ServiceReconciler) createOrGetSecret(ctx context.Context, logger *zap.SugaredLogger, svc, hsvc *corev1.Service, tags []string) (string, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			// Hardcode a -0 suffix so that in future, if we support
			// multiple StatefulSet replicas, we can provision -N for
			// those.
			Name:      hsvc.Name + "-0",
			Namespace: a.operatorNamespace,
			Labels:    childResourceLabels(svc),
		},
	}
	if err := a.Get(ctx, client.ObjectKeyFromObject(secret), secret); err == nil {
		logger.Debugf("secret %s/%s already exists", secret.GetNamespace(), secret.GetName())
		return secret.Name, nil
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}

	// Secret doesn't exist yet, create one. Initially it contains
	// only the Tailscale authkey, but once Tailscale starts it'll
	// also store the daemon state.
	sts, err := getSingleObject[appsv1.StatefulSet](ctx, a.Client, a.operatorNamespace, childResourceLabels(svc))
	if err != nil {
		return "", err
	}
	if sts != nil {
		// StatefulSet exists, so we have already created the secret.
		// If the secret is missing, they should delete the StatefulSet.
		logger.Errorf("Tailscale proxy secret doesn't exist, but the corresponding StatefulSet %s/%s already does. Something is wrong, please delete the StatefulSet.", sts.GetNamespace(), sts.GetName())
		return "", nil
	}
	// Create API Key secret which is going to be used by the statefulset
	// to authenticate with Tailscale.
	logger.Debugf("creating authkey for new tailscale proxy")
	authKey, err := a.newAuthKey(ctx, tags)
	if err != nil {
		return "", err
	}

	secret.StringData = map[string]string{
		"authkey": authKey,
	}
	if err := a.Create(ctx, secret); err != nil {
		return "", err
	}
	return secret.Name, nil
}

func (a *ServiceReconciler) getDeviceInfo(ctx context.Context, svc *corev1.Service) (id, hostname string, err error) {
	sec, err := getSingleObject[corev1.Secret](ctx, a.Client, a.operatorNamespace, childResourceLabels(svc))
	if err != nil {
		return "", "", err
	}
	if sec == nil {
		return "", "", nil
	}
	id = string(sec.Data["device_id"])
	if id == "" {
		return "", "", nil
	}
	// Kubernetes chokes on well-formed FQDNs with the trailing dot, so we have
	// to remove it.
	hostname = strings.TrimSuffix(string(sec.Data["device_fqdn"]), ".")
	if hostname == "" {
		return "", "", nil
	}
	return id, hostname, nil
}

func (a *ServiceReconciler) newAuthKey(ctx context.Context, tags []string) (string, error) {
	caps := tailscale.KeyCapabilities{
		Devices: tailscale.KeyDeviceCapabilities{
			Create: tailscale.KeyDeviceCreateCapabilities{
				Reusable:      false,
				Preauthorized: true,
				Tags:          tags,
			},
		},
	}

	key, _, err := a.tsClient.CreateKey(ctx, caps)
	if err != nil {
		return "", err
	}
	return key, nil
}

//go:embed manifests/proxy.yaml
var proxyYaml []byte

func (a *ServiceReconciler) reconcileSTS(ctx context.Context, logger *zap.SugaredLogger, parentSvc, headlessSvc *corev1.Service, authKeySecret, hostname string) (*appsv1.StatefulSet, error) {
	var ss appsv1.StatefulSet
	if err := yaml.Unmarshal(proxyYaml, &ss); err != nil {
		return nil, fmt.Errorf("failed to unmarshal proxy spec: %w", err)
	}
	container := &ss.Spec.Template.Spec.Containers[0]
	container.Image = a.proxyImage
	if ip := parentSvc.Annotations[AnnotationTargetIP]; ip != "" {
		container.Env = append(container.Env,
			corev1.EnvVar{
				Name:  "TS_EGRESS_IP",
				Value: ip,
			},
			corev1.EnvVar{
				// Egress might need to be able to resolve Tailscale DNS -
				// probably ok to make the proxy always pull DNS config from
				// control if it's an egress proxy?
				Name:  "TS_ACCEPT_DNS",
				Value: "true",
			},
		)
	} else {
		container.Env = append(container.Env,
			corev1.EnvVar{
				Name:  "TS_DEST_IP",
				Value: parentSvc.Spec.ClusterIP,
			},
		)
	}
	container.Env = append(container.Env,
		corev1.EnvVar{
			Name:  "TS_KUBE_SECRET",
			Value: authKeySecret,
		},
		corev1.EnvVar{
			Name:  "TS_HOSTNAME",
			Value: hostname,
		})
	ss.ObjectMeta = metav1.ObjectMeta{
		Name:      headlessSvc.Name,
		Namespace: a.operatorNamespace,
		Labels:    childResourceLabels(parentSvc),
	}
	ss.Spec.ServiceName = headlessSvc.Name
	ss.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"app": string(parentSvc.UID),
		},
	}
	ss.Spec.Template.ObjectMeta.Labels = map[string]string{
		"app": string(parentSvc.UID),
	}
	ss.Spec.Template.Spec.PriorityClassName = a.proxyPriorityClassName
	logger.Debugf("reconciling statefulset %s/%s", ss.GetNamespace(), ss.GetName())
	return createOrUpdate(ctx, a.Client, a.operatorNamespace, &ss, func(s *appsv1.StatefulSet) { s.Spec = ss.Spec })
}

// ptrObject is a type constraint for pointer types that implement
// client.Object.
type ptrObject[T any] interface {
	client.Object
	*T
}

// createOrUpdate adds obj to the k8s cluster, unless the object already exists,
// in which case update is called to make changes to it. If update is nil, the
// existing object is returned unmodified.
//
// obj is looked up by its Name and Namespace if Name is set, otherwise it's
// looked up by labels.
func createOrUpdate[T any, O ptrObject[T]](ctx context.Context, c client.Client, ns string, obj O, update func(O)) (O, error) {
	var (
		existing O
		err      error
	)
	if obj.GetName() != "" {
		existing = new(T)
		existing.SetName(obj.GetName())
		existing.SetNamespace(obj.GetNamespace())
		err = c.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	} else {
		existing, err = getSingleObject[T, O](ctx, c, ns, obj.GetLabels())
	}
	if err == nil && existing != nil {
		if update != nil {
			update(existing)
			if err := c.Update(ctx, existing); err != nil {
				return nil, err
			}
		}
		return existing, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get object: %w", err)
	}
	if err := c.Create(ctx, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// getSingleObject searches for k8s objects of type T
// (e.g. corev1.Service) with the given labels, and returns
// it. Returns nil if no objects match the labels, and an error if
// more than one object matches.
func getSingleObject[T any, O ptrObject[T]](ctx context.Context, c client.Client, ns string, labels map[string]string) (O, error) {
	ret := O(new(T))
	kinds, _, err := c.Scheme().ObjectKinds(ret)
	if err != nil {
		return nil, err
	}
	if len(kinds) != 1 {
		// TODO: the runtime package apparently has a "pick the best
		// GVK" function somewhere that might be good enough?
		return nil, fmt.Errorf("more than 1 GroupVersionKind for %T", ret)
	}

	gvk := kinds[0]
	gvk.Kind += "List"
	lst := unstructured.UnstructuredList{}
	lst.SetGroupVersionKind(gvk)
	if err := c.List(ctx, &lst, client.InNamespace(ns), client.MatchingLabels(labels)); err != nil {
		return nil, err
	}

	if len(lst.Items) == 0 {
		return nil, nil
	}
	if len(lst.Items) > 1 {
		return nil, fmt.Errorf("found multiple matching %T objects", ret)
	}
	if err := c.Scheme().Convert(&lst.Items[0], ret, nil); err != nil {
		return nil, err
	}
	return ret, nil
}

func defaultBool(envName string, defVal bool) bool {
	vs := os.Getenv(envName)
	if vs == "" {
		return defVal
	}
	v, _ := opt.Bool(vs).Get()
	return v
}

func defaultEnv(envName, defVal string) string {
	v := os.Getenv(envName)
	if v == "" {
		return defVal
	}
	return v
}

func nameForService(svc *corev1.Service) (string, error) {
	if h, ok := svc.Annotations[AnnotationHostname]; ok {
		if err := dnsname.ValidLabel(h); err != nil {
			return "", fmt.Errorf("invalid Tailscale hostname %q: %w", h, err)
		}
		return h, nil
	}
	return svc.Namespace + "-" + svc.Name, nil
}
