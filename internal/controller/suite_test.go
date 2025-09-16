//go:build integration

/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/zap/zapcore"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	kubeconfigprovider "sigs.k8s.io/multicluster-runtime/providers/kubeconfig"

	"github.com/kuadrant/dns-operator/api/v1alpha1"
	"github.com/kuadrant/dns-operator/internal/provider"
	_ "github.com/kuadrant/dns-operator/internal/provider/aws"
	_ "github.com/kuadrant/dns-operator/internal/provider/azure"
	_ "github.com/kuadrant/dns-operator/internal/provider/endpoint"
	ep "github.com/kuadrant/dns-operator/internal/provider/endpoint"
	_ "github.com/kuadrant/dns-operator/internal/provider/google"
	_ "github.com/kuadrant/dns-operator/internal/provider/inmemory"
	//+kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

const (
	testDefaultClusterSecretNamespace = "dns-operator-system"
	testDefaultClusterSecretLabel     = "kuadrant.io/multicluster-kubeconfig"
)

var (
	// Controller runtime env test environments for each delegation role
	primaryTestEnv   *envtest.Environment
	secondaryTestEnv *envtest.Environment

	// Managers created for each environment
	primaryManager   ctrl.Manager
	secondaryManager ctrl.Manager

	// Kubernetes clients created for each environment
	primaryK8sClient   client.Client
	secondaryK8sClient client.Client

	// Kubeconfig data for 'kuadrant' user added to each environment
	secondaryKubeconfig []byte
	primaryKubeconfig   []byte

	// Cluster ID for each environment
	secondaryClusterID string
	primaryClusterID   string

	ctx    context.Context
	cancel context.CancelFunc
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.Level(zapcore.DebugLevel)))

	ctx, cancel = context.WithCancel(ctrl.SetupSignalHandler())
	By("bootstrapping test environment")

	primaryTestEnv, primaryManager = setupEnv(DelegationRolePrimary)
	secondaryTestEnv, secondaryManager = setupEnv(DelegationRoleSecondary)

	primaryK8sClient = primaryManager.GetClient()
	secondaryK8sClient = secondaryManager.GetClient()

	go func() {
		defer GinkgoRecover()
		err := primaryManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred())
	}()

	go func() {
		defer GinkgoRecover()
		err := secondaryManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred())
	}()

	//Create the namespace to hold multicluster secrets on the primary
	By(fmt.Sprintf("creating namespace '%s' on primary", testDefaultClusterSecretNamespace))
	CreateNamespace(testDefaultClusterSecretNamespace, primaryK8sClient)

	//Create a 'kuadrant' user in the primary environment and store the kubeconfig
	By("creating user 'kuadrant' in primary environment")
	primaryKubeconfig = createKuadrantUser(primaryTestEnv)
	Expect(primaryKubeconfig).ToNot(BeEmpty())

	//Create a 'kuadrant' user in the secondary environment and store the kubeconfig
	By("creating user 'kuadrant' in secondary environment")
	secondaryKubeconfig = createKuadrantUser(secondaryTestEnv)
	Expect(secondaryKubeconfig).ToNot(BeEmpty())

	Expect(primaryKubeconfig).ToNot(Equal(secondaryKubeconfig))

	var err error
	primaryClusterID, err = getKubeSystemUID(ctx, primaryK8sClient)
	Expect(err).NotTo(HaveOccurred())
	Expect(primaryClusterID).ToNot(BeEmpty())

	secondaryClusterID, err = getKubeSystemUID(ctx, secondaryK8sClient)
	Expect(err).NotTo(HaveOccurred())
	Expect(secondaryClusterID).ToNot(BeEmpty())

	Expect(secondaryClusterID).ToNot(Equal(primaryClusterID))
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	if primaryTestEnv != nil {
		err := primaryTestEnv.Stop()
		Expect(err).NotTo(HaveOccurred())
	}

	if secondaryTestEnv != nil {
		err := secondaryTestEnv.Stop()
		Expect(err).NotTo(HaveOccurred())
	}
})

func CreateNamespace(name string, client client.Client) {
	nsObject := &v1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}

	err := client.Create(context.Background(), nsObject)
	Expect(err).ToNot(HaveOccurred())

	existingNamespace := &v1.Namespace{}
	Eventually(func() error {
		return client.Get(context.Background(), types.NamespacedName{Name: name}, existingNamespace)
	}, time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
}

// setupEnv creates a new controller runtime envTest environment with the required controllers running for the given delegation role.
//
// The setup of controllers here should be the same how they are configured in the main application.
//
// Primary:
//   - create multicluster-controller-runtime manager
//   - setup kubeconfig provider
//   - setup DNSRecordReconciler
//   - setup RemoteDNSRecordReconciler
//
// Secondary:
//   - create controller-runtime manager
//   - setup DNSRecordReconciler
func setupEnv(delegationRole string) (*envtest.Environment, ctrl.Manager) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	var cfg *rest.Config

	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	err = v1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	dynClient, err := dynamic.NewForConfig(cfg)
	Expect(err).ToNot(HaveOccurred())
	Expect(dynClient).NotTo(BeNil())

	var mgr ctrl.Manager
	var mcmgr mcmanager.Manager

	defaultOptions := ctrl.Options{
		Scheme:                 scheme.Scheme,
		HealthProbeBindAddress: "0",
		Metrics:                metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{
			SkipNameValidation: ptr.To(true),
		},
		Logger: ctrl.LoggerFrom(ctx).WithName(delegationRole),
	}

	if delegationRole == DelegationRoleSecondary {
		// Use the normal controller runtime manager when running with the secondary delegation role
		mgr, err = ctrl.NewManager(cfg, defaultOptions)
		Expect(err).ToNot(HaveOccurred())
	} else {
		// Create the kubeconfig provider with options
		clusterProviderOpts := kubeconfigprovider.Options{
			Namespace:             testDefaultClusterSecretNamespace,
			KubeconfigSecretLabel: testDefaultClusterSecretLabel,
			KubeconfigSecretKey:   "kubeconfig",
			Scheme:                scheme.Scheme,
		}

		// Create the provider first, then the manager with the provider
		clusterProvider := kubeconfigprovider.New(clusterProviderOpts)

		// Set up a cluster-aware Manager, with the provider to lookup clusters.
		mcmgr, err = mcmanager.New(cfg, clusterProvider, defaultOptions)
		Expect(err).ToNot(HaveOccurred())

		// Set up provider controller with the manager.
		err = clusterProvider.SetupWithManager(ctx, mcmgr)
		Expect(err).ToNot(HaveOccurred())

		mgr = mcmgr.GetLocalManager()
	}
	Expect(mgr).ToNot(BeNil())

	providerFactory, err := provider.NewFactory(mgr.GetClient(), dynClient, []string{provider.DNSProviderInMem.String(), provider.DNSProviderEndpoint.String()}, ep.NewAuthoritativeDNSRecordProvider)
	Expect(err).ToNot(HaveOccurred())
	Expect(providerFactory).ToNot(BeNil())

	dnsRecordController := &DNSRecordReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		ProviderFactory: providerFactory,
		DelegationRole:  delegationRole,
	}

	err = dnsRecordController.SetupWithManager(mgr, RequeueDuration, ValidityDuration, DefaultValidationDuration, true, true)
	Expect(err).ToNot(HaveOccurred())

	if delegationRole == DelegationRolePrimary {
		Expect(mcmgr).ToNot(BeNil())

		remoteDNSRecordController := &RemoteDNSRecordReconciler{
			Scheme:          mgr.GetScheme(),
			ProviderFactory: providerFactory,
			DelegationRole:  delegationRole,
		}

		err = remoteDNSRecordController.SetupWithManager(mcmgr)
		Expect(err).ToNot(HaveOccurred())
	}

	return testEnv, mgr
}

// createKuadrantUser creates a new user 'kuadrant' in the given envTest Environment and returns the kubeconfig data for that user.
func createKuadrantUser(testEnv *envtest.Environment) (kubeconfig []byte) {
	user, err := testEnv.AddUser(envtest.User{Name: "kuadrant", Groups: []string{"system:masters"}}, &rest.Config{})
	Expect(err).ToNot(HaveOccurred())

	kubeconfig, err = user.KubeConfig()
	Expect(err).ToNot(HaveOccurred())
	Expect(kubeconfig).ToNot(BeEmpty())

	return kubeconfig
}

func generateTestNamespaceName() string {
	return "test-namespace-" + uuid.New().String()
}

// returns the `kube-system` namespace UID as a string
func getKubeSystemUID(ctx context.Context, c client.Client) (string, error) {
	ns := &v1.Namespace{}
	err := c.Get(ctx, client.ObjectKey{Name: "kube-system"}, ns)
	if err != nil {
		return "", err
	}
	return string(ns.UID), nil
}
