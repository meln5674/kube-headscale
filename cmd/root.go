/*
Copyright © 2026 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8srest "k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	k8s "sigs.k8s.io/controller-runtime/pkg/client"
	k8sconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	hsv1 "github.com/juanfont/headscale/gen/go/headscale/v1"
	hspolicyv2 "github.com/juanfont/headscale/hscontrol/policy/v2"
	hstypes "github.com/juanfont/headscale/hscontrol/types"
	"tailscale.com/tailcfg"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/spf13/cobra"
)

const (
	policyLabel     = `kube-headscale.meln5674.io/policy`
	derpMapLabel    = `kube-headscale.meln5674.io/derpmap`
	dnsLabel        = `kube-headscale.meln5674.io/dns-extra-records`
	apikeyLabel     = `kube-headscale.meln5674.io/apikey`
	preauthkeyLabel = `kube-headscale.meln5674.io/preauthkey`

	apikeyExpirationAnnotation = `kube-headscale.meln5674.io/apikey-expiration`
	apikeyLifetimeAnnotation   = `kube-headscale.meln5674.io/apikey-lifetime`

	apikeySecretField     = "apikey"
	preauthkeySecretField = "preauthkey"

	derpMapPath = "/derpmap"

	apikeyDefaultLifetime = 90 * 24 * time.Hour
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "kube-headscale",
	Short: "Sidecar for automating headscale in kubernetes",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()

		go func() {
			<-ctx.Done()
			log.Log.Info("Received interrupt")
		}()

		if namespace == "" {
			nsBytes, err := os.ReadFile("/run/secrets/kubernetes.io/serviceaccount/namespace")
			if errors.Is(err, os.ErrNotExist) {
				namespace = "default"
			} else if err != nil {
				return fmt.Errorf("reading in-cluster namespace file: %w", err)
			} else {
				namespace = string(nsBytes)
			}
		}
		err := hstypes.LoadConfig("", false)
		if err != nil {
			return fmt.Errorf("init headscale config: %w", err)
		}

		k8scfg, err := k8sconfig.GetConfig()
		if err != nil {
			return fmt.Errorf("loading kubernetes config: %w", err)
		}

		k8sclient, err := k8s.New(k8scfg, k8s.Options{})
		if err != nil {
			return fmt.Errorf("loading kubernetes config: %w", err)
		}

		sidecar := sidecar{
			policyComponents:  make(map[string]hspolicyv2.Policy),
			dnsComponents:     make(map[string][]tailcfg.DNSRecord),
			derpMapComponents: make(map[string]map[int]tailcfg.DERPRegion),
		}

		// The startup process is a bit convoluted, as we have a chicken/egg problem here.
		// Headscale wants all files to exist, and all derp urls to respond before it will start up,
		// but we need the unix socket to exist to connect over grpc before starting the controllers
		// (the manager is what runs the http server for the derpmap endpoint).
		// To resolve this, we start a dummy manager to just serve the initial derpmap, then
		// once its ready, we create the initial dns file. At that point, headscale has everything it needs
		// from us, so we can wait for it to create the socket, then quickly re-create the manager and its
		// http server after we've connected.
		mgr, err := mkMgr(k8scfg)
		if err != nil {
			return fmt.Errorf("init manager: %w", err)
		}
		mgr.AddMetricsServerExtraHandler(derpMapPath, http.HandlerFunc(sidecar.ServeDERPMap))

		log.SetLogger(klog.Background())

		log.Log.Info("listing initial derpmaps")
		err = sidecar.initDERPMap(ctx, k8sclient)
		if err != nil {
			return err
		}

		errs := make(chan error)

		initCtx, stopInit := context.WithCancel(context.Background())
		defer stopInit()
		log.Log.Info("starting dummy manager to serve derpmap")
		initDone := make(chan struct{})
		go func() {
			mgr.Start(initCtx)
			initDone <- struct{}{}
		}()

		log.Log.Info("listing initial dns records")
		err = sidecar.initDNS(ctx, k8sclient)
		if err != nil {
			return err
		}

		log.Log.Info("waiting for headscale socket to appear")

		for {
			_, err := os.Stat(headscaleSocket)
			if err == nil {
				break
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			log.Log.Info("headscale socket does not exist yet")
			time.Sleep(1 * time.Second)
		}

		hsconn, err := grpc.NewClient("unix://"+headscaleSocket,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			//grpc.WithContextDialer(func(ctx context.Context, s string) (net.Conn, error) {
			//	return (&net.Dialer{}).DialContext(ctx, "unix", headscaleSocket)
			//}),
		)
		if err != nil {
			return err
		}

		stopInit()
		<-initDone

		log.Log.Info("connecting to headscale")
		sidecar.hs = hsv1.NewHeadscaleServiceClient(hsconn)

		log.Log.Info("listing initial policies")
		err = sidecar.initPolicy(ctx, k8sclient)
		if err != nil {
			return err
		}

		log.Log.Info("starting manager")
		mgr, err = mkMgr(k8scfg)
		if err != nil {
			return fmt.Errorf("init manager: %w", err)
		}
		mgr.AddMetricsServerExtraHandler(derpMapPath, http.HandlerFunc(sidecar.ServeDERPMap))
		sidecar.k8s = mgr.GetClient()

		go func() {
			errs <- sidecar.runMgr(ctx, mgr)
		}()

		// TODO: Handle all errors, cancel context if either exits
		err = <-errs

		return err
	},
}

func mkMgr(k8scfg *k8srest.Config) (manager.Manager, error) {
	return manager.New(k8scfg, manager.Options{
		Cache: cache.Options{
			ByObject: map[k8s.Object]cache.ByObject{
				(&corev1.ConfigMap{}): cache.ByObject{
					Namespaces: map[string]cache.Config{namespace: cache.Config{}},
				},
				(&corev1.Secret{}): cache.ByObject{
					Namespaces: map[string]cache.Config{namespace: cache.Config{}},
				},
			},
		},
		Metrics: server.Options{
			BindAddress: listenAddr,
		},
	})
}

type sidecar struct {
	lock sync.Mutex

	k8s               k8s.Client
	hs                hsv1.HeadscaleServiceClient
	policyComponents  map[string]Policy
	dnsComponents     map[string]DNSRecords
	derpMapComponents map[string]DERPMapRegions
	renderedDERPMap   []byte
}

type Policy = hspolicyv2.Policy
type DERPMapRegions = map[int]tailcfg.DERPRegion
type DNSRecords = []tailcfg.DNSRecord

func (s *sidecar) initPolicy(ctx context.Context, k8sclient k8s.Client) error {
	var policyCms corev1.ConfigMapList
	err := k8sclient.List(ctx, &policyCms, k8s.HasLabels{policyLabel}, k8s.InNamespace(namespace))
	if err != nil {
		return err
	}
	var p Policy
	for _, cm := range policyCms.Items {
		_ = addKeys(s.policyComponents, cm.Name, cm.Data, parsePolicy)
	}
	err = s.setPolicy(ctx, p)
	if err != nil {
		return err
	}
	return nil
}

func (s *sidecar) initDERPMap(ctx context.Context, k8sclient k8s.Client) error {
	var derpMapCms corev1.ConfigMapList
	err := k8sclient.List(ctx, &derpMapCms, k8s.HasLabels{derpMapLabel}, k8s.InNamespace(namespace))
	if err != nil {
		return err
	}
	d := make(DERPMapRegions)
	for _, cm := range derpMapCms.Items {
		_ = addKeys(s.derpMapComponents, cm.Name, cm.Data, parseDERPMap)
	}
	err = s.renderDERPMap(ctx, d)
	if err != nil {
		return err
	}
	return nil
}

func (s *sidecar) initDNS(ctx context.Context, k8sclient k8s.Client) error {
	var dnsCms corev1.ConfigMapList
	err := k8sclient.List(ctx, &dnsCms, k8s.HasLabels{dnsLabel}, k8s.InNamespace(namespace))
	if err != nil {
		return err
	}
	var dns DNSRecords
	for _, cm := range dnsCms.Items {
		_ = addKeys(s.dnsComponents, cm.Name, cm.Data, parseDNSRecords)
	}
	err = s.syncDNSRecords(ctx, dns)
	if err != nil {
		return err
	}

	return nil
}

func (s *sidecar) runMgr(ctx context.Context, mgr manager.Manager) error {
	err := builder.
		ControllerManagedBy(mgr).
		Named("configmap").
		For(&corev1.ConfigMap{}).
		Complete(reconcile.Func(s.handleConfigMapChange))
	if err != nil {
		return fmt.Errorf("init configmap controller: %w", err)
	}
	err = builder.
		ControllerManagedBy(mgr).
		Named("secret").
		For(&corev1.Secret{}).
		Complete(reconcile.Func(s.handleSecretChange))
	if err != nil {
		return fmt.Errorf("init secret controller: %w", err)
	}
	return mgr.Start(ctx)
}

type labelState struct {
	label        string
	hasLabel     bool
	hasFinalizer bool
	deleting     bool
	ok           bool
}

func hasLabel(meta *metav1.ObjectMeta, label string) bool {
	return hasLabelOrFinalizer(meta, label).hasLabel
}

func hasLabelOrFinalizer(meta *metav1.ObjectMeta, label string) labelState {
	_, hasLabel := meta.Labels[label]
	hasFinalizer := slices.Contains(meta.Finalizers, label)
	return labelState{
		label:        label,
		hasLabel:     hasLabel,
		hasFinalizer: hasFinalizer,
		deleting:     meta.DeletionTimestamp != nil,
		ok:           hasLabel || hasFinalizer,
	}
}

func (st labelState) ensureFinalizer(meta *metav1.ObjectMeta) bool {
	if !st.hasFinalizer {
		meta.Finalizers = append(meta.Finalizers, st.label)
	}
	return !st.hasFinalizer
}

func (st labelState) needsFinalize() bool {
	return (!st.hasLabel || st.deleting) && st.hasFinalizer
}

func (st labelState) finalize(meta *metav1.ObjectMeta) {
	if st.hasFinalizer {
		meta.Finalizers = slices.DeleteFunc(meta.Finalizers, func(s string) bool { return s == st.label })
	}
}

func addKeys[V, S any](dst map[string]V, name string, src map[string]S, parse func(S) (V, error)) error {
	for k, s := range src {
		v, err := parse(s)
		if err != nil {
			return fmt.Errorf("%v: %w", s, err)
		}
		dst[name+"/"+k] = v
	}
	return nil
}

func removeKeys[V any](dst map[string]V, name string) {
	maps.DeleteFunc(dst, func(k string, _ V) bool { return strings.HasPrefix(k, name+"/") })
}

func mergeKeys[V any](out V, dst map[string]V, merge func(*V, V)) V {
	for _, v := range dst {
		merge(&out, v)
	}

	return out
}

func includeKeys[V, S any](ctx context.Context, lock *sync.Mutex, dst map[string]V, name string, src map[string]S, init V, parse func(S) (V, error), merge func(*V, V), sync func(context.Context, V) error) error {
	lock.Lock()
	defer lock.Unlock()

	removeKeys(dst, name)
	err := addKeys(dst, name, src, parse)
	if err != nil {
		return err
	}

	return sync(ctx, mergeKeys(init, dst, merge))
}

func excludeKeys[V any](ctx context.Context, lock *sync.Mutex, dst map[string]V, name string, init V, merge func(*V, V), sync func(context.Context, V) error) error {
	lock.Lock()
	defer lock.Unlock()

	removeKeys(dst, name)
	return sync(ctx, mergeKeys(init, dst, merge))
}

func (s *sidecar) handleConfigMapChange(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var cm corev1.ConfigMap
	err := s.k8s.Get(ctx, req.NamespacedName, &cm)
	if err != nil {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, err
	}

	// Object deleted, ignore
	if kerrors.IsNotFound(err) {
		log.Log.Info("ignoring missing configmap", "namespace", req.Namespace, "name", req.Name)
		return reconcile.Result{}, nil
	}

	// If a label is present, its one of ours
	// 	If the finalizer is present, and the object is being deleted, remove its components and then remove the finalizer
	// 	If the finalizer is not present, and the object is not being deleted, add the finalizer, and add its components
	// If the label is not present, but the finalizer is, the label was removed, remove its components, then remove the finalizer
	if st := hasLabelOrFinalizer(&cm.ObjectMeta, policyLabel); st.ok {
		log.Log.Info("handling policy configmap", "namespace", req.Namespace, "name", req.Name)
		return reconcileConfigMap(s, ctx, s.policyComponents, &cm, st, Policy{}, parsePolicy, mergePolicy, s.setPolicy)
	}

	if st := hasLabelOrFinalizer(&cm.ObjectMeta, derpMapLabel); st.ok {
		log.Log.Info("handling derpmap configmap", "namespace", req.Namespace, "name", req.Name)
		return reconcileConfigMap(s, ctx, s.derpMapComponents, &cm, st, DERPMapRegions{}, parseDERPMap, mergeDERPMap, s.renderDERPMap)
	}

	if st := hasLabelOrFinalizer(&cm.ObjectMeta, dnsLabel); st.ok {
		log.Log.Info("handling dns configmap", "namespace", req.Namespace, "name", req.Name)
		return reconcileConfigMap(s, ctx, s.dnsComponents, &cm, st, DNSRecords{}, parseDNSRecords, mergeDNSRecords, s.syncDNSRecords)
	}

	log.Log.Info("ignoring unrelated configmap", "namespace", req.Namespace, "name", req.Name)
	return reconcile.Result{}, nil
}

func reconcileConfigMap[V any](s *sidecar, ctx context.Context, dst map[string]V, cm *corev1.ConfigMap, st labelState, init V, parse func(string) (V, error), merge func(*V, V), sync func(context.Context, V) error) (reconcile.Result, error) {
	if st.needsFinalize() {
		err := excludeKeys(
			ctx, &s.lock,
			dst,
			cm.Name,
			init, merge, sync,
		)

		if err != nil {
			return reconcile.Result{RequeueAfter: 5 * time.Second}, err
		}
		st.finalize(&cm.ObjectMeta)
		err = s.k8s.Update(ctx, cm)
		if err != nil {
			return reconcile.Result{RequeueAfter: 5 * time.Second}, err
		}
		return reconcile.Result{}, nil
	}

	if !st.deleting {
		if st.ensureFinalizer(&cm.ObjectMeta) {
			err := s.k8s.Update(ctx, cm)
			if err != nil {
				return reconcile.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
	}
	err := includeKeys(
		ctx, &s.lock,
		dst,
		cm.Name, cm.Data,
		init, parse, merge, sync,
	)
	if err != nil {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, err
	}
	return reconcile.Result{}, nil
}

func parsePolicy(pjson string) (hspolicyv2.Policy, error) {
	var p hspolicyv2.Policy
	err := yaml.Unmarshal([]byte(pjson), &p)
	return p, err
}

func mergePolicy(p *Policy, comp Policy) {
	p.ACLs = append(p.ACLs, comp.ACLs...)
	p.AutoApprovers.ExitNode = append(p.AutoApprovers.ExitNode, comp.AutoApprovers.ExitNode...)
	if p.AutoApprovers.Routes == nil {
		p.AutoApprovers.Routes = make(map[netip.Prefix]hspolicyv2.AutoApprovers, len(comp.AutoApprovers.Routes))
	}
	maps.Copy(p.AutoApprovers.Routes, comp.AutoApprovers.Routes)
	if p.Groups == nil {
		p.Groups = make(hspolicyv2.Groups, len(comp.Groups))
	}
	maps.Copy(p.Groups, comp.Groups)
	if p.Hosts == nil {
		p.Hosts = make(hspolicyv2.Hosts, len(comp.Hosts))
	}
	maps.Copy(p.Hosts, comp.Hosts)
	p.SSHs = append(p.SSHs, comp.SSHs...)
	if p.TagOwners == nil {
		p.TagOwners = make(hspolicyv2.TagOwners, len(comp.TagOwners))
	}
	maps.Copy(p.TagOwners, comp.TagOwners)
}

func (s *sidecar) setPolicy(ctx context.Context, p Policy) error {
	pjson, err := json.Marshal(&p)
	if err != nil {
		return err
	}
	log.Log.Info("submitting policy")
	_, err = s.hs.SetPolicy(ctx, &hsv1.SetPolicyRequest{Policy: string(pjson)})
	return err
}

func parseDERPMap(pjson string) (DERPMapRegions, error) {
	var p DERPMapRegions
	err := yaml.Unmarshal([]byte(pjson), &p)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func mergeDERPMap(p *DERPMapRegions, comp DERPMapRegions) {
	maps.Copy(*p, comp)
}

func (s *sidecar) renderDERPMap(ctx context.Context, p DERPMapRegions) error {
	rendered, err := json.Marshal(map[string]any{"Regions": p})
	if err != nil {
		return err
	}
	s.renderedDERPMap = rendered
	return nil
}

func (s *sidecar) ServeDERPMap(w http.ResponseWriter, r *http.Request) {
	s.lock.Lock()
	defer s.lock.Unlock()
	log.Log.Info("serving derpmap", "derpmap", string(s.renderedDERPMap))
	w.Header().Set("content-type", "application/json")
	w.Write(s.renderedDERPMap)
}

func parseDNSRecords(pjson string) (DNSRecords, error) {
	var p DNSRecords
	err := yaml.Unmarshal([]byte(pjson), &p)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func mergeDNSRecords(p *DNSRecords, comp DNSRecords) {
	*p = append(*p, comp...)
}

func (s *sidecar) syncDNSRecords(_ context.Context, p DNSRecords) error {
	pjson, err := json.Marshal(p)
	if err != nil {
		return err
	}
	log.Log.Info("updating dns record file")
	return os.WriteFile(dnsExtraRecordsPath, pjson, 0o644)
}

func (s *sidecar) handleSecretChange(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var sec corev1.Secret
	err := s.k8s.Get(ctx, req.NamespacedName, &sec)
	if err != nil {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, err
	}

	// Object deleted, ignore
	if kerrors.IsNotFound(err) {
		log.Log.Info("ignoring missing secret", "namespace", req.Namespace, "name", req.Name)
		return reconcile.Result{}, nil
	}

	if hasLabel(&sec.ObjectMeta, apikeyLabel) {
		log.Log.Info("handling apikey secret", "namespace", req.Namespace, "name", req.Name)
		return s.handleApiKeyChange(ctx, &sec)
	}

	if hasLabel(&sec.ObjectMeta, preauthkeyLabel) {
		log.Log.Info("handling preauthkey secret", "namespace", req.Namespace, "name", req.Name)
		return s.handlePreAuthKeyChange(ctx, &sec)
	}

	log.Log.Info("ignoring unrelated configmap", "namespace", req.Namespace, "name", req.Name)
	return reconcile.Result{}, nil
}

func (s *sidecar) handleApiKeyChange(ctx context.Context, sec *corev1.Secret) (reconcile.Result, error) {
	now := time.Now()

	var err error
	var currentExpiration time.Time
	expirationStr, hasExpriation := sec.Annotations[apikeyExpirationAnnotation]
	if hasExpriation {
		currentExpiration, err = time.Parse(time.RFC3339, expirationStr)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	var lifetime time.Duration
	lifetimeStr, hasLifetime := sec.Annotations[apikeyLifetimeAnnotation]
	if hasLifetime {
		lifetime, err = time.ParseDuration(lifetimeStr)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	rotateAfter := currentExpiration.Add(-apikeyRotateBefore)

	if !hasLifetime && hasExpriation && now.After(rotateAfter) {
		log.Log.Info("ignoring expired (or about to be expired) apikey with explicit expiration and no lifetime", "expiration", currentExpiration)
		return reconcile.Result{}, nil
	}

	if len(sec.Data[apikeySecretField]) == 0 {
		log.Log.Info("apikey not yet populated")
	} else if hasExpriation && now.Before(rotateAfter) {
		log.Log.Info("not yet time to rotate apikey, validating", "expiration", currentExpiration, "rotate-after", rotateAfter)

		ok, err := s.verifyApiKey(ctx, sec)
		if err != nil {
			return reconcile.Result{RequeueAfter: 5 * time.Second}, err
		}
		if ok {
			log.Log.Info("apikey secret still valid, not rotating", "namespace", sec.Namespace, "name", sec.Name, "expiration", currentExpiration, "rotate-after", rotateAfter)
			return reconcile.Result{RequeueAfter: rotateAfter.Sub(now)}, nil
		}
		log.Log.Info("apikey no longer valid, rotating")
	}
	var nextExpiration time.Time
	if hasExpriation && !hasLifetime {
		nextExpiration = currentExpiration
	} else if hasLifetime {
		nextExpiration = now.Add(lifetime)
	} else {
		nextExpiration = now.Add(apikeyDefaultLifetime)
	}

	if sec.Annotations == nil {
		sec.Annotations = make(map[string]string, 1)
	}
	sec.Annotations[apikeyExpirationAnnotation] = nextExpiration.Format(time.RFC3339)

	rotateAfter = nextExpiration.Add(-apikeyRotateBefore)
	log.Log.Info("requesting new api key", "expiration", nextExpiration)
	resp, err := s.hs.CreateApiKey(ctx, &hsv1.CreateApiKeyRequest{Expiration: timestamppb.New(nextExpiration)})
	if err != nil {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, err
	}
	log.Log.Info("received new api key", "apikey", resp.ApiKey, "rotate-after", rotateAfter)
	if sec.Data == nil {
		sec.Data = make(map[string][]byte, 1)
	}
	sec.Data[apikeySecretField] = []byte(resp.ApiKey)
	err = s.k8s.Update(ctx, sec)
	if err != nil {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, err
	}
	return reconcile.Result{RequeueAfter: rotateAfter.Sub(now)}, nil
}

func (s *sidecar) verifyApiKey(ctx context.Context, sec *corev1.Secret) (bool, error) {
	opts := []grpc.DialOption{
		grpc.WithPerRPCCredentials(tokenAuth{
			token: string(sec.Data[apikeySecretField]),
		}),
	}
	if headscaleInsecure {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	hsconn, err := grpc.NewClient(headscaleGrpcAddr, opts...)
	if err != nil {
		return false, err
	}
	hs := hsv1.NewHeadscaleServiceClient(hsconn)
	log.Log.Info("requesting apikeys to validate apikey")
	_, err = hs.ListApiKeys(ctx, &hsv1.ListApiKeysRequest{})
	// v0.28.0 and earlier do not correctly use the Unauthenticated code
	if status.Code(err) == codes.Unauthenticated || (status.Code(err) == codes.Internal && strings.Contains(err.Error(), "failed to validate token")) {
		log.Log.Info("apikey not valid", "error", err)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *sidecar) handlePreAuthKeyChange(ctx context.Context, sec *corev1.Secret) (reconcile.Result, error) {
	if _, ok := sec.Data[preauthkeySecretField]; ok {
		log.Log.Info("preauthkey secret already populated", "namespace", sec.Namespace, "name", sec.Name)
		return reconcile.Result{}, nil
	}
	user := string(sec.Labels[preauthkeyLabel])
	log.Log.Info("requesting users")
	userResp, err := s.hs.ListUsers(ctx, &hsv1.ListUsersRequest{Name: user})
	if err != nil {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, err
	}
	if len(userResp.Users) != 1 {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("no such user: %s", user)
	}

	log.Log.Info("requesting preauthkey")
	keyResp, err := s.hs.CreatePreAuthKey(ctx, &hsv1.CreatePreAuthKeyRequest{
		User: userResp.Users[0].Id,
		// TODO: Annotation with config
	})
	if err != nil {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, err
	}
	log.Log.Info("received preauthkey")
	if sec.Data == nil {
		sec.Data = make(map[string][]byte, 1)
	}
	sec.Data[preauthkeySecretField] = []byte(keyResp.PreAuthKey.Key)
	err = s.k8s.Update(ctx, sec)
	if err != nil {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, err
	}
	return reconcile.Result{}, nil
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	kflags := flag.NewFlagSet("", flag.PanicOnError)
	klog.InitFlags(kflags)
	k8sconfig.RegisterFlags(kflags)
	rootCmd.Flags().AddGoFlagSet(kflags)
	rootCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Kubernetes namespace. Will check for namespace injected for service account is absent, otherwise uses 'default'")
	rootCmd.Flags().StringVar(&headscaleSocket, "headscale-sock", "/var/run/headscale/headscale.sock", "Path to headscale socket")
	rootCmd.Flags().StringVar(&dnsExtraRecordsPath, "dns-extra-records-path", "/var/run/headscale/dns-configmaps.json", "Path to write extra dns records from ConfigMaps to")
	rootCmd.Flags().StringVar(&headscaleGrpcAddr, "headscale-grpc", "127.0.0.1:50433", "Localhost port of headscale grpc server")
	rootCmd.Flags().BoolVar(&headscaleInsecure, "headscale-insecure", false, "True if headscale localhost port is plaintext (not TLS)")
	rootCmd.Flags().StringVar(&derpMapAddr, "derpmap-listen", "127.0.0.1:8080", "Address to listen on for dynamic derpmap from ConfigMaps")
	rootCmd.Flags().DurationVar(&apikeyRotateBefore, "apikey-rotate-before", 5*time.Second, "How long before an apikey expires to rotate it")
	rootCmd.Flags().StringVar(&listenAddr, "listen-addr", "0.0.0.0:8080", "Address to serve DERPMap and metrics on")
}

var (
	listenAddr string

	namespace string

	headscaleSocket   string
	headscaleGrpcAddr string
	headscaleInsecure bool

	apikeyRotateBefore time.Duration

	dnsExtraRecordsPath string

	derpMapAddr string

	klogFlags *flag.FlagSet
)

type tokenAuth struct {
	token string
}

// Return value is mapped to request headers.
func (t tokenAuth) GetRequestMetadata(
	ctx context.Context,
	in ...string,
) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + t.token,
	}, nil
}

func (tokenAuth) RequireTransportSecurity() bool {
	return !headscaleInsecure
}
