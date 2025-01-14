// Copyright 2016-2017 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/cilium/cilium/pkg/annotation"
	"github.com/cilium/cilium/pkg/comparator"
	"github.com/cilium/cilium/pkg/controller"
	"github.com/cilium/cilium/pkg/endpoint"
	"github.com/cilium/cilium/pkg/endpointmanager"
	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/ipcache"
	"github.com/cilium/cilium/pkg/k8s"
	"github.com/cilium/cilium/pkg/k8s/apis/cilium.io"
	"github.com/cilium/cilium/pkg/k8s/apis/cilium.io/utils"
	cilium_v2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	clientset "github.com/cilium/cilium/pkg/k8s/client/clientset/versioned"
	informer "github.com/cilium/cilium/pkg/k8s/client/informers/externalversions"
	k8sUtils "github.com/cilium/cilium/pkg/k8s/utils"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/loadbalancer"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	bpfIPCache "github.com/cilium/cilium/pkg/maps/ipcache"
	"github.com/cilium/cilium/pkg/metrics"
	"github.com/cilium/cilium/pkg/node"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/policy"
	"github.com/cilium/cilium/pkg/service"
	"github.com/cilium/cilium/pkg/versioncheck"
	"github.com/cilium/cilium/pkg/versioned"

	go_version "github.com/hashicorp/go-version"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
)

const (
	k8sErrLogTimeout = time.Minute

	k8sAPIGroupCRD              = "CustomResourceDefinition"
	k8sAPIGroupNodeV1Core       = "core/v1::Node"
	k8sAPIGroupNamespaceV1Core  = "core/v1::Namespace"
	k8sAPIGroupServiceV1Core    = "core/v1::Service"
	k8sAPIGroupEndpointV1Core   = "core/v1::Endpoint"
	k8sAPIGroupPodV1Core        = "core/v1::Pods"
	k8sAPIGroupNetworkingV1Core = "networking.k8s.io/v1::NetworkPolicy"
	k8sAPIGroupIngressV1Beta1   = "extensions/v1beta1::Ingress"
	k8sAPIGroupCiliumV2         = "cilium/v2::CiliumNetworkPolicy"
	cacheSyncTimeout            = time.Duration(3 * time.Minute)

	metricCNP      = "CiliumNetworkPolicy"
	metricEndpoint = "Endpoint"
	metricIngress  = "Ingress"
	metricKNP      = "NetworkPolicy"
	metricNS       = "Namespace"
	metricNode     = "Node"
	metricPod      = "Pod"
	metricService  = "Service"
	metricCreate   = "create"
	metricDelete   = "delete"
	metricUpdate   = "update"
)

var (
	// k8sErrMsgMU guards additions and removals to k8sErrMsg, which stores a
	// time after which a repeat error message can be printed
	k8sErrMsgMU  lock.Mutex
	k8sErrMsg    = map[string]time.Time{}
	k8sServerVer *go_version.Version

	ciliumNPClient clientset.Interface

	networkPolicyV1VerConstr = versioncheck.MustCompile(">= 1.7.0")

	ciliumv2VerConstr           = versioncheck.MustCompile(">= 1.8.0")
	ciliumUpdateStatusVerConstr = versioncheck.MustCompile(">= 1.11.0")

	k8sCM = controller.NewManager()

	importMetadataCache = ruleImportMetadataCache{
		ruleImportMetadataMap: make(map[string]policyImportMetadata),
	}

	// local cache of Kubernetes Endpoints which relate to external services.
	endpointMetadataCache = endpointImportMetadataCache{
		endpointImportMetadataMap: make(map[string]endpointImportMetadata),
	}
)

// ruleImportMetadataCache maps the unique identifier of a CiliumNetworkPolicy
// (namespace and name) to metadata about the importing of the rule into the
// agent's policy repository at the time said rule was imported (revision
// number, and if any error occurred while importing).
type ruleImportMetadataCache struct {
	mutex                 lock.RWMutex
	ruleImportMetadataMap map[string]policyImportMetadata
}

type policyImportMetadata struct {
	revision          uint64
	policyImportError error
}

func (r *ruleImportMetadataCache) upsert(cnp *cilium_v2.CiliumNetworkPolicy, revision uint64, importErr error) {
	if cnp == nil {
		return
	}

	meta := policyImportMetadata{
		revision:          revision,
		policyImportError: importErr,
	}
	podNSName := k8sUtils.GetObjNamespaceName(&cnp.ObjectMeta)

	r.mutex.Lock()
	r.ruleImportMetadataMap[podNSName] = meta
	r.mutex.Unlock()
}

func (r *ruleImportMetadataCache) delete(cnp *cilium_v2.CiliumNetworkPolicy) {
	if cnp == nil {
		return
	}
	podNSName := k8sUtils.GetObjNamespaceName(&cnp.ObjectMeta)

	r.mutex.Lock()
	delete(r.ruleImportMetadataMap, podNSName)
	r.mutex.Unlock()
}

func (r *ruleImportMetadataCache) get(cnp *cilium_v2.CiliumNetworkPolicy) (policyImportMetadata, bool) {
	if cnp == nil {
		return policyImportMetadata{}, false
	}
	podNSName := k8sUtils.GetObjNamespaceName(&cnp.ObjectMeta)
	r.mutex.RLock()
	policyImportMeta, ok := r.ruleImportMetadataMap[podNSName]
	r.mutex.RUnlock()
	return policyImportMeta, ok
}

// endpointImportMetadataCache maps the unique identifier of a Kubernetes
// Endpoint (namespace and name) to metadata about whether translation of the
// rules involving services that the endpoint corresponds to into the
// agent's policy repository at the time said rule was imported (if any error
// occurred while importing).
type endpointImportMetadataCache struct {
	mutex                     lock.RWMutex
	endpointImportMetadataMap map[string]endpointImportMetadata
}

type endpointImportMetadata struct {
	ruleTranslationError error
}

func (r *endpointImportMetadataCache) upsert(ep *v1.Endpoints, ruleTranslationErr error) {
	if ep == nil {
		return
	}

	meta := endpointImportMetadata{
		ruleTranslationError: ruleTranslationErr,
	}
	epNSName := k8sUtils.GetObjNamespaceName(&ep.ObjectMeta)

	r.mutex.Lock()
	r.endpointImportMetadataMap[epNSName] = meta
	r.mutex.Unlock()
}

func (r *endpointImportMetadataCache) delete(ep *v1.Endpoints) {
	if ep == nil {
		return
	}
	epNSName := k8sUtils.GetObjNamespaceName(&ep.ObjectMeta)

	r.mutex.Lock()
	delete(r.endpointImportMetadataMap, epNSName)
	r.mutex.Unlock()
}

func (r *endpointImportMetadataCache) get(cnp *v1.Endpoints) (endpointImportMetadata, bool) {
	if cnp == nil {
		return endpointImportMetadata{}, false
	}
	epNSName := k8sUtils.GetObjNamespaceName(&cnp.ObjectMeta)
	r.mutex.RLock()
	endpointImportMeta, ok := r.endpointImportMetadataMap[epNSName]
	r.mutex.RUnlock()
	return endpointImportMeta, ok
}

// k8sAPIGroupsUsed is a lockable map to hold which k8s API Groups we have
// enabled/in-use
// Note: We can replace it with a Go 1.9 map once we require that version
type k8sAPIGroupsUsed struct {
	lock.RWMutex
	apis map[string]bool
}

func (m *k8sAPIGroupsUsed) addAPI(api string) {
	m.Lock()
	defer m.Unlock()
	if m.apis == nil {
		m.apis = make(map[string]bool)
	}
	m.apis[api] = true
}

func (m *k8sAPIGroupsUsed) removeAPI(api string) {
	m.Lock()
	defer m.Unlock()
	delete(m.apis, api)
}

func (m *k8sAPIGroupsUsed) getGroups() []string {
	m.RLock()
	defer m.RUnlock()
	groups := make([]string, 0, len(m.apis))
	for k := range m.apis {
		groups = append(groups, k)
	}
	return groups
}

func init() {
	// Replace error handler with our own
	runtime.ErrorHandlers = []func(error){
		k8sErrorHandler,
	}
}

// k8sErrorUpdateCheckUnmuteTime returns a boolean indicating whether we should
// log errmsg or not. It manages once-per-k8sErrLogTimeout entry in k8sErrMsg.
// When errmsg is new or more than k8sErrLogTimeout has passed since the last
// invocation that returned true, it returns true.
func k8sErrorUpdateCheckUnmuteTime(errstr string, now time.Time) bool {
	k8sErrMsgMU.Lock()
	defer k8sErrMsgMU.Unlock()

	if unmuteDeadline, ok := k8sErrMsg[errstr]; !ok || now.After(unmuteDeadline) {
		k8sErrMsg[errstr] = now.Add(k8sErrLogTimeout)
		return true
	}

	return false
}

// k8sErrorHandler handles the error messages in a non verbose way by omitting
// repeated instances of the same error message for a timeout defined with
// k8sErrLogTimeout.
func k8sErrorHandler(e error) {
	if e == nil {
		return
	}

	// We rate-limit certain categories of error message. These are matched
	// below, with a default behaviour to print everything else without
	// rate-limiting.
	// Note: We also have side-effects in some of the special cases.
	now := time.Now()
	errstr := e.Error()
	switch {
	// This can occur when cilium comes up before the k8s API server, and keeps
	// trying to connect.
	case strings.Contains(errstr, "connection refused"):
		if k8sErrorUpdateCheckUnmuteTime(errstr, now) {
			log.WithError(e).Error("k8sError")
		}

	// k8s does not allow us to watch both ThirdPartyResource and
	// CustomResourceDefinition. This would occur when a user mixes these within
	// the k8s cluster, and might occur when upgrading from versions of cilium
	// that used ThirdPartyResource to define CiliumNetworkPolicy.
	case strings.Contains(errstr, "Failed to list *v2.CiliumNetworkPolicy: the server could not find the requested resource"):
		if k8sErrorUpdateCheckUnmuteTime(errstr, now) {
			log.WithError(e).Error("Conflicting TPR and CRD resources")
			log.Warn("Detected conflicting TPR and CRD, please migrate all ThirdPartyResource to CustomResourceDefinition! More info: https://cilium.link/migrate-tpr")
			log.Warn("Due to conflicting TPR and CRD rules, CiliumNetworkPolicy enforcement can't be guaranteed!")
		}

	// fromCIDR and toCIDR used to expect an "ip" subfield (so, they were a YAML
	// map with one field) but common usage and expectation would simply list the
	// CIDR ranges and IPs desired as a YAML list. In these cases we would see
	// this decode error. We have since changed the definition to be a simple
	// list of strings.
	case strings.Contains(errstr, "Unable to decode an event from the watch stream: unable to decode watch event"),
		strings.Contains(errstr, "Failed to list *v1.CiliumNetworkPolicy: only encoded map or array can be decoded into a struct"),
		strings.Contains(errstr, "Failed to list *v2.CiliumNetworkPolicy: only encoded map or array can be decoded into a struct"),
		strings.Contains(errstr, "Failed to list *v2.CiliumNetworkPolicy: v2.CiliumNetworkPolicyList:"):
		if k8sErrorUpdateCheckUnmuteTime(errstr, now) {
			log.WithError(e).Error("Unable to decode k8s watch event")
		}

	default:
		log.WithError(e).Error("k8sError")
	}
}

// blockWaitGroupToSyncResources ensures that anything which waits on waitGroup
// waits until all objects of the specified resource stored in Kubernetes are
// received by the informer and processed by controller.
// Fatally exits if syncing these initial objects fails.
func blockWaitGroupToSyncResources(waitGroup *sync.WaitGroup, informer cache.Controller,
	resourceName string) {

	waitGroup.Add(1)
	go func() {
		scopedLog := log.WithField("kubernetesResource", resourceName)
		scopedLog.Debug("waiting for cache to synchronize")
		if ok := cache.WaitForCacheSync(wait.NeverStop, informer.HasSynced); !ok {
			// Fatally exit it resource fails to sync
			scopedLog.Fatalf("failed to wait for cache to sync")
		}
		scopedLog.Debug("cache synced")
		waitGroup.Done()
	}()
}

// EnableK8sWatcher watches for policy, services and endpoint changes on the Kubernetes
// api server defined in the receiver's daemon k8sClient. Re-syncs all state from the
// Kubernetes api server at the given reSyncPeriod duration.
func (d *Daemon) EnableK8sWatcher(reSyncPeriod time.Duration) error {
	if !k8s.IsEnabled() {
		log.Debug("Not enabling k8s event listener because k8s is not enabled")
		return nil
	}
	log.Info("Enabling k8s event listener")

	restConfig, err := k8s.CreateConfig()
	if err != nil {
		return fmt.Errorf("Unable to create rest configuration: %s", err)
	}

	apiextensionsclientset, err := apiextensionsclient.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("Unable to create rest configuration for k8s CRD: %s", err)
	}

	k8sServerVer, err = k8s.GetServerVersion()
	if err != nil {
		return fmt.Errorf("unable to retrieve kubernetes serverversion: %s", err)
	}

	switch {
	case ciliumv2VerConstr.Check(k8sServerVer):
		err = cilium_v2.CreateCustomResourceDefinitions(apiextensionsclientset)
		if err != nil {
			return fmt.Errorf("Unable to create custom resource definition: %s", err)
		}
		d.k8sAPIGroups.addAPI(k8sAPIGroupCRD)
		d.k8sAPIGroups.addAPI(k8sAPIGroupCiliumV2)
	default:
		return fmt.Errorf("Unsupported k8s version. Minimal supported version is %s", ciliumv2VerConstr.String())
	}

	ciliumNPClient, err = clientset.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("Unable to create cilium network policy client: %s", err)
	}

	switch {
	case networkPolicyV1VerConstr.Check(k8sServerVer):
		policyController := k8sUtils.ControllerFactory(
			k8s.Client().NetworkingV1().RESTClient(),
			&networkingv1.NetworkPolicy{},
			k8sUtils.ResourceEventHandlerFactory(
				func(i interface{}) func() error {
					return func() error {
						err := d.addK8sNetworkPolicyV1(i.(*networkingv1.NetworkPolicy))
						updateK8sEventMetric(metricKNP, metricCreate, err == nil)
						return nil
					}
				},
				func(i interface{}) func() error {
					return func() error {
						err := d.deleteK8sNetworkPolicyV1(i.(*networkingv1.NetworkPolicy))
						updateK8sEventMetric(metricKNP, metricDelete, err == nil)
						return nil
					}
				},
				func(old, new interface{}) func() error {
					return func() error {
						err := d.updateK8sNetworkPolicyV1(
							old.(*networkingv1.NetworkPolicy),
							new.(*networkingv1.NetworkPolicy))
						updateK8sEventMetric(metricKNP, metricUpdate, err == nil)
						return nil
					}
				},
				d.missingK8sNetworkPolicyV1,
				&networkingv1.NetworkPolicy{},
				k8s.Client(),
				reSyncPeriod,
				metrics.EventTSK8s,
			),
			fields.Everything(),
		)
		blockWaitGroupToSyncResources(&d.k8sResourceSyncWaitGroup, policyController, "NetworkPolicy")
		go policyController.Run(wait.NeverStop)

		d.k8sAPIGroups.addAPI(k8sAPIGroupNetworkingV1Core)
	}

	svcController := k8sUtils.ControllerFactory(
		k8s.Client().CoreV1().RESTClient(),
		&v1.Service{},
		k8sUtils.ResourceEventHandlerFactory(
			func(i interface{}) func() error {
				return func() error {
					err := d.addK8sServiceV1(i.(*v1.Service))
					updateK8sEventMetric(metricService, metricCreate, err == nil)
					return nil
				}
			},
			func(i interface{}) func() error {
				return func() error {
					err := d.deleteK8sServiceV1(i.(*v1.Service))
					updateK8sEventMetric(metricService, metricDelete, err == nil)
					return nil
				}
			},
			func(old, new interface{}) func() error {
				return func() error {
					err := d.updateK8sServiceV1(old.(*v1.Service), new.(*v1.Service))
					updateK8sEventMetric(metricService, metricUpdate, err == nil)
					return nil
				}
			},
			d.missingK8sServiceV1,
			&v1.Service{},
			k8s.Client(),
			reSyncPeriod,
			metrics.EventTSK8s,
		),
		fields.Everything(),
	)
	blockWaitGroupToSyncResources(&d.k8sResourceSyncWaitGroup, svcController, "Service")
	go svcController.Run(wait.NeverStop)
	d.k8sAPIGroups.addAPI(k8sAPIGroupServiceV1Core)

	endpointController := k8sUtils.ControllerFactory(
		k8s.Client().CoreV1().RESTClient(),
		&v1.Endpoints{},
		k8sUtils.ResourceEventHandlerFactory(
			func(i interface{}) func() error {
				return func() error {
					err := d.addK8sEndpointV1(i.(*v1.Endpoints))
					updateK8sEventMetric(metricEndpoint, metricCreate, err == nil)
					return nil
				}
			},
			func(i interface{}) func() error {
				return func() error {
					err := d.deleteK8sEndpointV1(i.(*v1.Endpoints))
					updateK8sEventMetric(metricEndpoint, metricDelete, err == nil)
					return nil
				}
			},
			func(old, new interface{}) func() error {
				return func() error {
					err := d.updateK8sEndpointV1(old.(*v1.Endpoints), new.(*v1.Endpoints))
					updateK8sEventMetric(metricEndpoint, metricUpdate, err == nil)
					return nil
				}
			},
			d.missingK8sEndpointsV1,
			&v1.Endpoints{},
			k8s.Client(),
			reSyncPeriod,
			metrics.EventTSK8s,
		),
		// Don't get any events from kubernetes endpoints.
		fields.ParseSelectorOrDie("metadata.name!=kube-scheduler,metadata.name!=kube-controller-manager"),
	)
	blockWaitGroupToSyncResources(&d.k8sResourceSyncWaitGroup, endpointController, "Endpoint")
	go endpointController.Run(wait.NeverStop)
	d.k8sAPIGroups.addAPI(k8sAPIGroupEndpointV1Core)

	if option.Config.IsLBEnabled() {
		ingressController := k8sUtils.ControllerFactory(
			k8s.Client().ExtensionsV1beta1().RESTClient(),
			&v1beta1.Ingress{},
			k8sUtils.ResourceEventHandlerFactory(
				func(i interface{}) func() error {
					return func() error {
						err := d.addIngressV1beta1(i.(*v1beta1.Ingress))
						updateK8sEventMetric(metricIngress, metricCreate, err == nil)
						return nil
					}
				},
				func(i interface{}) func() error {
					return func() error {
						err := d.deleteIngressV1beta1(i.(*v1beta1.Ingress))
						updateK8sEventMetric(metricIngress, metricDelete, err == nil)
						return nil
					}
				},
				func(old, new interface{}) func() error {
					return func() error {
						err := d.updateIngressV1beta1(old.(*v1beta1.Ingress), new.(*v1beta1.Ingress))
						updateK8sEventMetric(metricIngress, metricUpdate, err == nil)
						return nil
					}
				},
				func(m versioned.Map) versioned.Map {
					return m
				},
				&v1beta1.Ingress{},
				k8s.Client(),
				reSyncPeriod,
				metrics.EventTSK8s,
			),
			fields.Everything(),
		)
		blockWaitGroupToSyncResources(&d.k8sResourceSyncWaitGroup, ingressController, "Ingress")
		go ingressController.Run(wait.NeverStop)
		d.k8sAPIGroups.addAPI(k8sAPIGroupIngressV1Beta1)
	}

	si := informer.NewSharedInformerFactory(ciliumNPClient, reSyncPeriod)

	switch {
	case ciliumv2VerConstr.Check(k8sServerVer):
		ciliumV2Controller := si.Cilium().V2().CiliumNetworkPolicies().Informer()
		cnpStore := ciliumV2Controller.GetStore()

		rehf := k8sUtils.ResourceEventHandlerFactory(
			func(i interface{}) func() error {
				return func() error {
					err := d.addCiliumNetworkPolicyV2(cnpStore, i.(*cilium_v2.CiliumNetworkPolicy))
					updateK8sEventMetric(metricCNP, metricCreate, err == nil)
					return nil
				}
			},
			func(i interface{}) func() error {
				return func() error {
					err := d.deleteCiliumNetworkPolicyV2(i.(*cilium_v2.CiliumNetworkPolicy))
					updateK8sEventMetric(metricCNP, metricDelete, err == nil)
					return nil
				}
			},
			func(old, new interface{}) func() error {
				return func() error {
					err := d.updateCiliumNetworkPolicyV2(
						cnpStore,
						old.(*cilium_v2.CiliumNetworkPolicy),
						new.(*cilium_v2.CiliumNetworkPolicy),
					)
					updateK8sEventMetric(metricCNP, metricUpdate, err == nil)
					return nil
				}
			},
			d.missingCNPv2,
			&cilium_v2.CiliumNetworkPolicy{},
			ciliumNPClient,
			reSyncPeriod,
			metrics.EventTSK8s,
		)
		blockWaitGroupToSyncResources(&d.k8sResourceSyncWaitGroup, ciliumV2Controller, "CiliumNetworkPolicy")

		ciliumV2Controller.AddEventHandler(rehf)
	}

	si.Start(wait.NeverStop)

	podsController := k8sUtils.ControllerFactory(
		k8s.Client().CoreV1().RESTClient(),
		&v1.Pod{},
		k8sUtils.ResourceEventHandlerFactory(
			func(i interface{}) func() error {
				return func() error {
					err := d.addK8sPodV1(i.(*v1.Pod))
					updateK8sEventMetric(metricPod, metricCreate, err == nil)
					return nil
				}
			},
			func(i interface{}) func() error {
				return func() error {
					err := d.deleteK8sPodV1(i.(*v1.Pod))
					updateK8sEventMetric(metricPod, metricDelete, err == nil)
					return nil
				}
			},
			func(old, new interface{}) func() error {
				return func() error {
					err := d.updateK8sPodV1(old.(*v1.Pod), new.(*v1.Pod))
					updateK8sEventMetric(metricPod, metricUpdate, err == nil)
					return nil
				}
			},
			missingK8sPodV1,
			&v1.Pod{},
			k8s.Client(),
			reSyncPeriod,
			metrics.EventTSK8s,
		),
		fields.Everything(),
	)

	go podsController.Run(wait.NeverStop)
	d.k8sAPIGroups.addAPI(k8sAPIGroupPodV1Core)

	nodesController := k8sUtils.ControllerFactory(
		k8s.Client().CoreV1().RESTClient(),
		&v1.Node{},
		k8sUtils.ResourceEventHandlerFactory(
			func(i interface{}) func() error {
				return func() error {
					err := d.addK8sNodeV1(i.(*v1.Node))
					updateK8sEventMetric(metricNode, metricCreate, err == nil)
					return nil
				}
			},
			func(i interface{}) func() error {
				return func() error {
					err := d.deleteK8sNodeV1(i.(*v1.Node))
					updateK8sEventMetric(metricNode, metricDelete, err == nil)
					return nil
				}
			},
			func(old, new interface{}) func() error {
				return func() error {
					err := d.updateK8sNodeV1(old.(*v1.Node), new.(*v1.Node))
					updateK8sEventMetric(metricNode, metricUpdate, err == nil)
					return nil
				}
			},
			d.missingK8sNodeV1,
			&v1.Node{},
			k8s.Client(),
			reSyncPeriod,
			metrics.EventTSK8s,
		),
		fields.Everything(),
	)

	go nodesController.Run(wait.NeverStop)
	d.k8sAPIGroups.addAPI(k8sAPIGroupNodeV1Core)

	namespaceController := k8sUtils.ControllerFactory(
		k8s.Client().CoreV1().RESTClient(),
		&v1.Namespace{},
		k8sUtils.ResourceEventHandlerFactory(
			// AddFunc does not matter since the endpoint will fetch
			// namespace labels when the endpoint is created
			nil,
			// DelFunc does not matter since, when a namespace is deleted, all
			// pods belonging to that namespace are also deleted.
			nil,
			func(old, new interface{}) func() error {
				return func() error {
					err := d.updateK8sV1Namespace(old.(*v1.Namespace), new.(*v1.Namespace))
					updateK8sEventMetric(metricNS, metricUpdate, err == nil)
					return nil
				}
			},
			d.missingK8sNamespaceV1,
			&v1.Namespace{},
			k8s.Client(),
			reSyncPeriod,
			metrics.EventTSK8s,
		),
		fields.Everything(),
	)

	go namespaceController.Run(wait.NeverStop)
	d.k8sAPIGroups.addAPI(k8sAPIGroupNamespaceV1Core)

	endpoint.RunK8sCiliumEndpointSyncGC()

	return nil
}

func (d *Daemon) addK8sNetworkPolicyV1(k8sNP *networkingv1.NetworkPolicy) error {
	scopedLog := log.WithField(logfields.K8sAPIVersion, k8sNP.TypeMeta.APIVersion)
	rules, err := k8s.ParseNetworkPolicy(k8sNP)
	if err != nil {
		scopedLog.WithError(err).WithFields(logrus.Fields{
			logfields.CiliumNetworkPolicy: logfields.Repr(k8sNP),
		}).Error("Error while parsing k8s kubernetes NetworkPolicy")
		return err
	}
	scopedLog = scopedLog.WithField(logfields.K8sNetworkPolicyName, k8sNP.ObjectMeta.Name)

	opts := AddOptions{Replace: true}
	if _, err := d.PolicyAdd(rules, &opts); err != nil {
		scopedLog.WithError(err).WithFields(logrus.Fields{
			logfields.CiliumNetworkPolicy: logfields.Repr(rules),
		}).Error("Unable to add NetworkPolicy rules to policy repository")
		return err
	}

	scopedLog.Info("NetworkPolicy successfully added")
	return nil
}

func (d *Daemon) updateK8sNetworkPolicyV1(oldk8sNP, newk8sNP *networkingv1.NetworkPolicy) error {
	log.WithFields(logrus.Fields{
		logfields.K8sAPIVersion:                 oldk8sNP.TypeMeta.APIVersion,
		logfields.K8sNetworkPolicyName + ".old": oldk8sNP.ObjectMeta.Name,
		logfields.K8sNamespace + ".old":         oldk8sNP.ObjectMeta.Namespace,
		logfields.K8sNetworkPolicyName:          newk8sNP.ObjectMeta.Name,
		logfields.K8sNamespace:                  newk8sNP.ObjectMeta.Namespace,
	}).Debug("Received policy update")

	return d.addK8sNetworkPolicyV1(newk8sNP)
}

func (d *Daemon) deleteK8sNetworkPolicyV1(k8sNP *networkingv1.NetworkPolicy) error {
	labels := k8s.GetPolicyLabelsv1(k8sNP)

	if labels == nil {
		log.Fatalf("provided v1 NetworkPolicy is nil, so cannot delete it")
	}

	scopedLog := log.WithFields(logrus.Fields{
		logfields.K8sNetworkPolicyName: k8sNP.ObjectMeta.Name,
		logfields.K8sNamespace:         k8sNP.ObjectMeta.Namespace,
		logfields.K8sAPIVersion:        k8sNP.TypeMeta.APIVersion,
		logfields.Labels:               logfields.Repr(labels),
	})
	if _, err := d.PolicyDelete(labels); err != nil {
		scopedLog.WithError(err).Error("Error while deleting k8s NetworkPolicy")
		return err
	}

	scopedLog.Info("NetworkPolicy successfully removed")
	return nil
}

func (d *Daemon) missingK8sNetworkPolicyV1(m versioned.Map) versioned.Map {
	missing := versioned.NewMap()
	d.policy.Mutex.RLock()
	for k, v := range m {
		v1NP := v.Data.(*networkingv1.NetworkPolicy)
		ruleLabels := k8s.GetPolicyLabelsv1(v1NP)
		if !d.policy.ContainsAllRLocked(labels.LabelArrayList{ruleLabels}) {
			missing.Add(k, v)
		}
	}
	d.policy.Mutex.RUnlock()
	return missing
}

func parseSvcV1(svc *v1.Service) *loadbalancer.K8sServiceInfo {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.K8sSvcName:    svc.ObjectMeta.Name,
		logfields.K8sNamespace:  svc.ObjectMeta.Namespace,
		logfields.K8sAPIVersion: svc.TypeMeta.APIVersion,
		logfields.K8sSvcType:    svc.Spec.Type,
	})

	switch svc.Spec.Type {
	case v1.ServiceTypeClusterIP, v1.ServiceTypeNodePort, v1.ServiceTypeLoadBalancer:
		break

	case v1.ServiceTypeExternalName:
		// External-name services must be ignored
		return nil

	default:
		scopedLog.Warn("Ignoring k8s service: unsupported type")
		return nil
	}

	if svc.Spec.ClusterIP == "" {
		scopedLog.Info("Ignoring k8s service: empty ClusterIP")
		return nil
	}

	clusterIP := net.ParseIP(svc.Spec.ClusterIP)
	headless := false
	if strings.ToLower(svc.Spec.ClusterIP) == "none" {
		headless = true
	}
	newSI := loadbalancer.NewK8sServiceInfo(clusterIP, headless, svc.Labels, svc.Spec.Selector)

	// FIXME: Add support for
	//  - NodePort
	for _, port := range svc.Spec.Ports {
		p, err := loadbalancer.NewFEPort(loadbalancer.L4Type(port.Protocol), uint16(port.Port))
		if err != nil {
			scopedLog.WithError(err).WithField("port", port).Error("Unable to add service port")
			continue
		}
		if _, ok := newSI.Ports[loadbalancer.FEPortName(port.Name)]; !ok {
			newSI.Ports[loadbalancer.FEPortName(port.Name)] = p
		}
	}
	return newSI
}

func (d *Daemon) addK8sServiceV1(svc *v1.Service) error {
	newSI := parseSvcV1(svc)
	if newSI == nil {
		return nil
	}

	svcns := loadbalancer.K8sServiceNamespace{
		ServiceName: svc.ObjectMeta.Name,
		Namespace:   svc.ObjectMeta.Namespace,
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()

	if oldSI, ok := d.loadBalancer.K8sServices[svcns]; ok {
		if oldSI.Equals(newSI) {
			return nil
		}
	}

	d.loadBalancer.K8sServices[svcns] = newSI

	return d.syncLB(&svcns, nil, nil)
}

func (d *Daemon) updateK8sServiceV1(oldSvc, newSvc *v1.Service) error {
	log.WithFields(logrus.Fields{
		logfields.K8sAPIVersion:         oldSvc.TypeMeta.APIVersion,
		logfields.K8sSvcName + ".old":   oldSvc.ObjectMeta.Name,
		logfields.K8sNamespace + ".old": oldSvc.ObjectMeta.Namespace,
		logfields.K8sSvcType + ".old":   oldSvc.Spec.Type,
		logfields.K8sSvcName:            newSvc.ObjectMeta.Name,
		logfields.K8sNamespace:          newSvc.ObjectMeta.Namespace,
		logfields.K8sSvcType:            newSvc.Spec.Type,
	}).Debug("Received service update")

	return d.addK8sServiceV1(newSvc)
}

func (d *Daemon) deleteK8sServiceV1(svc *v1.Service) error {
	log.WithFields(logrus.Fields{
		logfields.K8sSvcName:    svc.ObjectMeta.Name,
		logfields.K8sNamespace:  svc.ObjectMeta.Namespace,
		logfields.K8sAPIVersion: svc.TypeMeta.APIVersion,
	}).Debug("Deleting k8s service")

	svcns := &loadbalancer.K8sServiceNamespace{
		ServiceName: svc.ObjectMeta.Name,
		Namespace:   svc.ObjectMeta.Namespace,
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()
	return d.syncLB(nil, nil, svcns)
}

// missingK8sServiceV1 returns a map containing missing services considered
// missing from the K8sServices of the loadbalancer.
func (d *Daemon) missingK8sServiceV1(m versioned.Map) versioned.Map {
	missing := versioned.NewMap()
	d.loadBalancer.K8sMU.RLock()
	for uuid, svcObj := range m {
		svc := svcObj.Data.(*v1.Service)
		svcns := loadbalancer.K8sServiceNamespace{
			ServiceName: svc.ObjectMeta.Name,
			Namespace:   svc.ObjectMeta.Namespace,
		}

		k8sSvc, ok := d.loadBalancer.K8sServices[svcns]
		if !ok {
			missing.Add(uuid, svcObj)
			continue
		}

		newSI := parseSvcV1(svc)
		if !k8sSvc.Equals(newSI) {
			missing.Add(uuid, svcObj)
		}
	}
	d.loadBalancer.K8sMU.RUnlock()
	return missing
}

func parseK8sEPv1(ep *v1.Endpoints) *loadbalancer.K8sServiceEndpoint {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.K8sEndpointName: ep.ObjectMeta.Name,
		logfields.K8sNamespace:    ep.ObjectMeta.Namespace,
		logfields.K8sAPIVersion:   ep.TypeMeta.APIVersion,
	})

	newSvcEP := loadbalancer.NewK8sServiceEndpoint()

	for _, sub := range ep.Subsets {
		for _, addr := range sub.Addresses {
			newSvcEP.BEIPs[addr.IP] = true
		}
		for _, port := range sub.Ports {
			lbPort, err := loadbalancer.NewL4Addr(loadbalancer.L4Type(port.Protocol), uint16(port.Port))
			if err != nil {
				scopedLog.WithError(err).Error("Error while creating a new LB Port")
				continue
			}
			newSvcEP.Ports[loadbalancer.FEPortName(port.Name)] = lbPort
		}
	}

	return newSvcEP
}

func (d *Daemon) addK8sEndpointV1(ep *v1.Endpoints) error {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.K8sEndpointName: ep.ObjectMeta.Name,
		logfields.K8sNamespace:    ep.ObjectMeta.Namespace,
		logfields.K8sAPIVersion:   ep.TypeMeta.APIVersion,
	})

	svcns := loadbalancer.K8sServiceNamespace{
		ServiceName: ep.ObjectMeta.Name,
		Namespace:   ep.ObjectMeta.Namespace,
	}

	newSvcEP := parseK8sEPv1(ep)

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()

	// Check whether any service corresponding to this endpoint already exists
	// before it gets inserted. If it does exist, this means that we may not have
	// to plumb the Kubernetes Endpoint into any toService rules if nothing
	// about the Endpoint has changed.
	storedK8sEndpoint, storedK8sEndpointOK := d.loadBalancer.K8sEndpoints[svcns]
	endpointsEqual := storedK8sEndpointOK && reflect.DeepEqual(storedK8sEndpoint, newSvcEP)

	d.loadBalancer.K8sEndpoints[svcns] = newSvcEP

	// Note: this does nothing if the service is headless.
	d.syncLB(&svcns, nil, nil)

	if option.Config.IsLBEnabled() {
		if err := d.syncExternalLB(&svcns, nil, nil); err != nil {
			scopedLog.WithError(err).Error("Unable to add endpoints on ingress service")
			return err
		}
	}

	svc, ok := d.loadBalancer.K8sServices[svcns]

	if ok && svc.IsExternal() {
		serviceImportMeta, cacheOK := endpointMetadataCache.get(ep)

		// If this is the first time adding this Endpoint, or there was an error
		// adding it last time, then try to add translate it and its
		// corresponding external service for any toServices rules which
		// select said service.
		if !cacheOK || (cacheOK && serviceImportMeta.ruleTranslationError != nil) {
			translator := k8s.NewK8sTranslator(svcns, *newSvcEP, false, svc.Labels, bpfIPCache.IPCache)
			err := d.policy.TranslateRules(translator)
			endpointMetadataCache.upsert(ep, err)
			if err != nil {
				log.Errorf("Unable to repopulate egress policies from ToService rules: %v", err)
				return err
			} else {
				scopedLog.Info("Kubernetes service endpoint added")
				d.TriggerPolicyUpdates(true, "Kubernetes service endpoint added")
			}
		} else if serviceImportMeta.ruleTranslationError == nil {
			// Given that we provide a non-zero value for the resyncPeriod for
			// the Kubernetes informer, we will receive updates for all
			// Endpoints from Kubernetes even if no meaningful content of the
			// Endpoints has changed. Do not trigger policy updates if no
			// meaningful content of the rule has changed, but only if the prior
			// addition of the service endpoints into the policy repository
			// succeeded.
			if endpointsEqual {
				scopedLog.Debugf("no changes to Kubernetes endpoint; not triggering policy updates")
				return nil
			}
			// Endpoint has changed, but already exists.
			scopedLog.Info("Kubernetes endpoint updated")
			d.TriggerPolicyUpdates(true, "Kubernetes service endpoint updated")
		}
	}
	return nil
}

func (d *Daemon) updateK8sEndpointV1(oldEP, newEP *v1.Endpoints) error {
	// TODO only print debug message if the difference between the old endpoint
	// and the new endpoint are important to us.
	//log.WithFields(logrus.Fields{
	//	logfields.K8sAPIVersion:            oldEP.TypeMeta.APIVersion,
	//	logfields.K8sEndpointName + ".old": oldEP.ObjectMeta.Name,
	//	logfields.K8sNamespace + ".old":    oldEP.ObjectMeta.Namespace,
	//	logfields.K8sEndpointName: newEP.ObjectMeta.Name,
	//	logfields.K8sNamespace:    newEP.ObjectMeta.Namespace,
	//}).Debug("Received endpoint update")

	return d.addK8sEndpointV1(newEP)
}

func (d *Daemon) deleteK8sEndpointV1(ep *v1.Endpoints) error {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.K8sEndpointName: ep.ObjectMeta.Name,
		logfields.K8sNamespace:    ep.ObjectMeta.Namespace,
		logfields.K8sAPIVersion:   ep.TypeMeta.APIVersion,
	})

	svcns := loadbalancer.K8sServiceNamespace{
		ServiceName: ep.ObjectMeta.Name,
		Namespace:   ep.ObjectMeta.Namespace,
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()

	if endpoint, ok := d.loadBalancer.K8sEndpoints[svcns]; ok {
		svc, ok := d.loadBalancer.K8sServices[svcns]
		if ok && svc.IsExternal() {
			translator := k8s.NewK8sTranslator(svcns, *endpoint, true, svc.Labels, bpfIPCache.IPCache)
			err := d.policy.TranslateRules(translator)
			if err != nil {
				log.Errorf("Unable to depopulate egress policies from ToService rules: %v", err)
			} else {
				d.TriggerPolicyUpdates(true, "Kubernetes service endpoint deleted")
			}
		}
	}

	syncErr := d.syncLB(nil, nil, &svcns)
	if option.Config.IsLBEnabled() {
		if err := d.syncExternalLB(nil, nil, &svcns); err != nil {
			scopedLog.WithError(err).Error("Unable to remove endpoints on ingress service")
			return err
		}
	}
	endpointMetadataCache.delete(ep)
	return syncErr
}

// missingK8sEndpointsV1 returns a map containing missing endpoints considered
// missing from the k8sEndpoints of the loadbalancer.
func (d *Daemon) missingK8sEndpointsV1(m versioned.Map) versioned.Map {
	missing := versioned.NewMap()
	// parse endpoints first to avoid holding the loadBalancer mutex
	type metaEP struct {
		k8sSvcEP *loadbalancer.K8sServiceEndpoint
		svcNS    loadbalancer.K8sServiceNamespace
		uuid     versioned.UUID
		object   versioned.Object
	}
	metaEPs := make([]metaEP, 0, len(m))
	for k, v := range m {
		v1EP := v.Data.(*v1.Endpoints)
		metaEPs = append(metaEPs, metaEP{
			k8sSvcEP: parseK8sEPv1(v1EP),
			svcNS: loadbalancer.K8sServiceNamespace{
				ServiceName: v1EP.ObjectMeta.Name,
				Namespace:   v1EP.ObjectMeta.Namespace,
			},
			uuid:   k,
			object: v,
		})
	}

	d.loadBalancer.K8sMU.RLock()
	for _, metaEP := range metaEPs {
		lbEP, ok := d.loadBalancer.K8sEndpoints[metaEP.svcNS]
		if !ok {
			missing.Add(metaEP.uuid, metaEP.object)
			continue
		}
		if !metaEP.k8sSvcEP.DeepEqual(lbEP) {
			missing.Add(metaEP.uuid, metaEP.object)
		}
	}
	d.loadBalancer.K8sMU.RUnlock()
	return missing
}

func areIPsConsistent(ipv4Enabled, isSvcIPv4 bool, svc loadbalancer.K8sServiceNamespace, se *loadbalancer.K8sServiceEndpoint) error {
	if isSvcIPv4 {
		if !ipv4Enabled {
			return fmt.Errorf("Received an IPv4 k8s service but IPv4 is "+
				"disabled in the cilium daemon. Ignoring service %+v", svc)
		}

		for epIP := range se.BEIPs {
			//is IPv6?
			if net.ParseIP(epIP).To4() == nil {
				return fmt.Errorf("Not all endpoints IPs are IPv4. Ignoring IPv4 service %+v", svc)
			}
		}
	} else {
		for epIP := range se.BEIPs {
			//is IPv4?
			if net.ParseIP(epIP).To4() != nil {
				return fmt.Errorf("Not all endpoints IPs are IPv6. Ignoring IPv6 service %+v", svc)
			}
		}
	}
	return nil
}

func getUniqPorts(svcPorts map[loadbalancer.FEPortName]*loadbalancer.FEPort) map[uint16]bool {
	// We are not discriminating the different L4 protocols on the same L4
	// port so we create the number of unique sets of service IP + service
	// port.
	uniqPorts := map[uint16]bool{}
	for _, svcPort := range svcPorts {
		uniqPorts[svcPort.Port] = true
	}
	return uniqPorts
}

func (d *Daemon) delK8sSVCs(svc loadbalancer.K8sServiceNamespace, svcInfo *loadbalancer.K8sServiceInfo, se *loadbalancer.K8sServiceEndpoint) error {
	// If east-west load balancing is disabled, we should not sync(add or delete)
	// K8s service to a cilium service.
	if lb := viper.GetBool("disable-k8s-services"); lb == true {
		return nil
	}

	// Headless services do not need any datapath implementation
	if svcInfo.IsHeadless {
		return nil
	}

	isSvcIPv4 := svcInfo.FEIP.To4() != nil
	if err := areIPsConsistent(!option.Config.IPv4Disabled, isSvcIPv4, svc, se); err != nil {
		return err
	}

	scopedLog := log.WithFields(logrus.Fields{
		logfields.K8sSvcName:   svc.ServiceName,
		logfields.K8sNamespace: svc.Namespace,
	})

	repPorts := getUniqPorts(svcInfo.Ports)

	for _, svcPort := range svcInfo.Ports {
		if !repPorts[svcPort.Port] {
			continue
		}
		repPorts[svcPort.Port] = false

		if svcPort.ID != 0 {
			if err := service.DeleteID(uint32(svcPort.ID)); err != nil {
				scopedLog.WithError(err).Warn("Error while cleaning service ID")
			}
		}

		fe, err := loadbalancer.NewL3n4Addr(svcPort.Protocol, svcInfo.FEIP, svcPort.Port)
		if err != nil {
			scopedLog.WithError(err).Error("Error while creating a New L3n4AddrID. Ignoring service")
			continue
		}

		if err := d.svcDeleteByFrontend(fe); err != nil {
			scopedLog.WithError(err).WithField(logfields.Object, logfields.Repr(fe)).
				Warn("Error deleting service by frontend")

		} else {
			scopedLog.Debugf("# cilium lb delete-service %s %d 0", svcInfo.FEIP, svcPort.Port)
		}

		if err := d.RevNATDelete(svcPort.ID); err != nil {
			scopedLog.WithError(err).WithField(logfields.ServiceID, svcPort.ID).Warn("Error deleting reverse NAT")
		} else {
			scopedLog.Debugf("# cilium lb delete-rev-nat %d", svcPort.ID)
		}
	}
	return nil
}

func (d *Daemon) addK8sSVCs(svc loadbalancer.K8sServiceNamespace, svcInfo *loadbalancer.K8sServiceInfo, se *loadbalancer.K8sServiceEndpoint) error {
	// If east-west load balancing is disabled, we should not sync(add or delete)
	// K8s service to a cilium service.
	if lb := viper.GetBool("disable-k8s-services"); lb == true {
		return nil
	}

	// Headless services do not need any datapath implementation
	if svcInfo.IsHeadless {
		return nil
	}

	scopedLog := log.WithFields(logrus.Fields{
		logfields.K8sSvcName:   svc.ServiceName,
		logfields.K8sNamespace: svc.Namespace,
	})

	isSvcIPv4 := svcInfo.FEIP.To4() != nil
	if err := areIPsConsistent(!option.Config.IPv4Disabled, isSvcIPv4, svc, se); err != nil {
		return err
	}

	uniqPorts := getUniqPorts(svcInfo.Ports)

	for fePortName, fePort := range svcInfo.Ports {
		if !uniqPorts[fePort.Port] {
			continue
		}

		k8sBEPort := se.Ports[fePortName]
		uniqPorts[fePort.Port] = false

		if fePort.ID == 0 {
			feAddr, err := loadbalancer.NewL3n4Addr(fePort.Protocol, svcInfo.FEIP, fePort.Port)
			if err != nil {
				scopedLog.WithError(err).WithFields(logrus.Fields{
					logfields.ServiceID: fePortName,
					logfields.IPAddr:    svcInfo.FEIP,
					logfields.Port:      fePort.Port,
					logfields.Protocol:  fePort.Protocol,
				}).Error("Error while creating a new L3n4Addr. Ignoring service...")
				continue
			}
			feAddrID, err := service.AcquireID(*feAddr, 0)
			if err != nil {
				scopedLog.WithError(err).WithFields(logrus.Fields{
					logfields.ServiceID: fePortName,
					logfields.IPAddr:    svcInfo.FEIP,
					logfields.Port:      fePort.Port,
					logfields.Protocol:  fePort.Protocol,
				}).Error("Error while getting a new service ID. Ignoring service...")
				continue
			}
			scopedLog.WithFields(logrus.Fields{
				logfields.ServiceName: fePortName,
				logfields.ServiceID:   feAddrID.ID,
				logfields.Object:      logfields.Repr(svc),
			}).Debug("Got feAddr ID for service")
			fePort.ID = feAddrID.ID
		}

		besValues := []loadbalancer.LBBackEnd{}

		if k8sBEPort != nil {
			for epIP := range se.BEIPs {
				bePort := loadbalancer.LBBackEnd{
					L3n4Addr: loadbalancer.L3n4Addr{IP: net.ParseIP(epIP), L4Addr: *k8sBEPort},
					Weight:   0,
				}
				besValues = append(besValues, bePort)
			}
		}

		fe, err := loadbalancer.NewL3n4AddrID(fePort.Protocol, svcInfo.FEIP, fePort.Port, fePort.ID)
		if err != nil {
			scopedLog.WithError(err).WithFields(logrus.Fields{
				logfields.IPAddr: svcInfo.FEIP,
				logfields.Port:   svcInfo.Ports,
			}).Error("Error while creating a New L3n4AddrID. Ignoring service...")
			continue
		}
		if _, err := d.svcAdd(*fe, besValues, true); err != nil {
			scopedLog.WithError(err).Error("Error while inserting service in LB map")
		}
	}
	return nil
}

func (d *Daemon) syncLB(newSN, modSN, delSN *loadbalancer.K8sServiceNamespace) error {
	deleteSN := func(delSN loadbalancer.K8sServiceNamespace) error {
		svc, ok := d.loadBalancer.K8sServices[delSN]
		if !ok {
			delete(d.loadBalancer.K8sEndpoints, delSN)
			return nil
		}

		endpoint, ok := d.loadBalancer.K8sEndpoints[delSN]
		if !ok {
			delete(d.loadBalancer.K8sServices, delSN)
			return nil
		}

		if err := d.delK8sSVCs(delSN, svc, endpoint); err != nil {
			log.WithError(err).WithFields(logrus.Fields{
				logfields.K8sSvcName:   delSN.ServiceName,
				logfields.K8sNamespace: delSN.Namespace,
			}).Error("Unable to delete k8s service")
			return err
		}

		delete(d.loadBalancer.K8sServices, delSN)
		delete(d.loadBalancer.K8sEndpoints, delSN)
		return nil
	}

	addSN := func(addSN loadbalancer.K8sServiceNamespace) error {
		svcInfo, ok := d.loadBalancer.K8sServices[addSN]
		if !ok {
			return nil
		}

		endpoint, ok := d.loadBalancer.K8sEndpoints[addSN]
		if !ok {
			return nil
		}

		if err := d.addK8sSVCs(addSN, svcInfo, endpoint); err != nil {
			log.WithError(err).WithFields(logrus.Fields{
				logfields.K8sSvcName:   addSN.ServiceName,
				logfields.K8sNamespace: addSN.Namespace,
			}).Error("Unable to add k8s service")
			return err
		}
		return nil
	}

	if delSN != nil {
		// Clean old services
		return deleteSN(*delSN)
	}
	if modSN != nil {
		// Re-add modified services
		return addSN(*modSN)
	}
	if newSN != nil {
		// Add new services
		return addSN(*newSN)
	}
	return nil
}

func (d *Daemon) addIngressV1beta1(ingress *v1beta1.Ingress) error {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.K8sIngressName: ingress.ObjectMeta.Name,
		logfields.K8sAPIVersion:  ingress.TypeMeta.APIVersion,
		logfields.K8sNamespace:   ingress.ObjectMeta.Namespace,
	})

	if ingress.Spec.Backend == nil {
		// We only support Single Service Ingress for now
		scopedLog.Warn("Cilium only supports Single Service Ingress for now, ignoring ingress")
		return nil
	}

	svcName := loadbalancer.K8sServiceNamespace{
		ServiceName: ingress.Spec.Backend.ServiceName,
		Namespace:   ingress.ObjectMeta.Namespace,
	}

	ingressPort := ingress.Spec.Backend.ServicePort.IntValue()
	fePort, err := loadbalancer.NewFEPort(loadbalancer.TCP, uint16(ingressPort))
	if err != nil {
		return err
	}

	var host net.IP
	if option.Config.IPv4Disabled {
		host = option.Config.HostV6Addr
	} else {
		host = option.Config.HostV4Addr
	}
	ingressSvcInfo := loadbalancer.NewK8sServiceInfo(host, false, nil, nil)
	ingressSvcInfo.Ports[loadbalancer.FEPortName(ingress.Spec.Backend.ServicePort.StrVal)] = fePort

	syncIngress := func(ingressSvcInfo *loadbalancer.K8sServiceInfo) error {
		d.loadBalancer.K8sIngress[svcName] = ingressSvcInfo

		if err := d.syncExternalLB(&svcName, nil, nil); err != nil {
			return fmt.Errorf("Unable to add ingress service %s: %s", svcName, err)
		}
		return nil
	}

	d.loadBalancer.K8sMU.Lock()
	err = syncIngress(ingressSvcInfo)
	d.loadBalancer.K8sMU.Unlock()
	if err != nil {
		scopedLog.WithError(err).Error("Error in syncIngress")
		return err
	}

	hostname, _ := os.Hostname()
	dpyCopyIngress := ingress.DeepCopy()
	dpyCopyIngress.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{
		{
			IP:       host.String(),
			Hostname: hostname,
		},
	}

	_, err = k8s.Client().ExtensionsV1beta1().Ingresses(dpyCopyIngress.ObjectMeta.Namespace).UpdateStatus(dpyCopyIngress)
	if err != nil {
		scopedLog.WithError(err).WithFields(logrus.Fields{
			logfields.K8sIngress: dpyCopyIngress,
		}).Error("Unable to update status of ingress")
	}
	return err
}

func (d *Daemon) updateIngressV1beta1(oldIngress, newIngress *v1beta1.Ingress) error {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.K8sIngressName + ".old": oldIngress.ObjectMeta.Name,
		logfields.K8sAPIVersion + ".old":  oldIngress.TypeMeta.APIVersion,
		logfields.K8sNamespace + ".old":   oldIngress.ObjectMeta.Namespace,
		logfields.K8sIngressName:          newIngress.ObjectMeta.Name,
		logfields.K8sAPIVersion:           newIngress.TypeMeta.APIVersion,
		logfields.K8sNamespace:            newIngress.ObjectMeta.Namespace,
	})

	if oldIngress.Spec.Backend == nil || newIngress.Spec.Backend == nil {
		// We only support Single Service Ingress for now
		scopedLog.Warn("Cilium only supports Single Service Ingress for now, ignoring ingress")
		return nil
	}

	// Add RevNAT to the BPF Map for non-LB nodes when a LB node update the
	// ingress status with its address.
	if !option.Config.IsLBEnabled() {
		port := newIngress.Spec.Backend.ServicePort.IntValue()
		for _, lb := range newIngress.Status.LoadBalancer.Ingress {
			ingressIP := net.ParseIP(lb.IP)
			if ingressIP == nil {
				continue
			}
			feAddr, err := loadbalancer.NewL3n4Addr(loadbalancer.TCP, ingressIP, uint16(port))
			if err != nil {
				scopedLog.WithError(err).Error("Error while creating a new L3n4Addr. Ignoring ingress...")
				continue
			}
			feAddrID, err := service.AcquireID(*feAddr, 0)
			if err != nil {
				scopedLog.WithError(err).Error("Error while getting a new service ID. Ignoring ingress...")
				continue
			}
			scopedLog.WithFields(logrus.Fields{
				logfields.ServiceID: feAddrID.ID,
			}).Debug("Got service ID for ingress")

			if err := d.RevNATAdd(feAddrID.ID, feAddrID.L3n4Addr); err != nil {
				scopedLog.WithError(err).WithFields(logrus.Fields{
					logfields.ServiceID: feAddrID.ID,
					logfields.IPAddr:    feAddrID.L3n4Addr.IP,
					logfields.Port:      feAddrID.L3n4Addr.Port,
					logfields.Protocol:  feAddrID.L3n4Addr.Protocol,
				}).Error("Unable to add reverse NAT ID for ingress")
			}
		}
		return nil
	}

	if oldIngress.Spec.Backend.ServiceName == newIngress.Spec.Backend.ServiceName &&
		oldIngress.Spec.Backend.ServicePort == newIngress.Spec.Backend.ServicePort {
		return nil
	}

	return d.addIngressV1beta1(newIngress)
}

func (d *Daemon) deleteIngressV1beta1(ingress *v1beta1.Ingress) error {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.K8sIngressName: ingress.ObjectMeta.Name,
		logfields.K8sAPIVersion:  ingress.TypeMeta.APIVersion,
		logfields.K8sNamespace:   ingress.ObjectMeta.Namespace,
	})

	if ingress.Spec.Backend == nil {
		// We only support Single Service Ingress for now
		scopedLog.Warn("Cilium only supports Single Service Ingress for now, ignoring ingress deletion")
		return nil
	}

	svcName := loadbalancer.K8sServiceNamespace{
		ServiceName: ingress.Spec.Backend.ServiceName,
		Namespace:   ingress.ObjectMeta.Namespace,
	}

	// Remove RevNAT from the BPF Map for non-LB nodes.
	if !option.Config.IsLBEnabled() {
		port := ingress.Spec.Backend.ServicePort.IntValue()
		for _, lb := range ingress.Status.LoadBalancer.Ingress {
			ingressIP := net.ParseIP(lb.IP)
			if ingressIP == nil {
				continue
			}
			feAddr, err := loadbalancer.NewL3n4Addr(loadbalancer.TCP, ingressIP, uint16(port))
			if err != nil {
				scopedLog.WithError(err).Error("Error while creating a new L3n4Addr. Ignoring ingress...")
				continue
			}
			// This is the only way that we can get the service's ID
			// without accessing the KVStore.
			svc := d.svcGetBySHA256Sum(feAddr.SHA256Sum())
			if svc != nil {
				if err := d.RevNATDelete(svc.FE.ID); err != nil {
					scopedLog.WithError(err).WithFields(logrus.Fields{
						logfields.ServiceID: svc.FE.ID,
					}).Error("Error while removing RevNAT for ingress")
				}
			}
		}
		return nil
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()

	ingressSvcInfo, ok := d.loadBalancer.K8sIngress[svcName]
	if !ok {
		return nil
	}

	// Get all active endpoints for the service specified in ingress
	k8sEP, ok := d.loadBalancer.K8sEndpoints[svcName]
	if !ok {
		return nil
	}

	err := d.delK8sSVCs(svcName, ingressSvcInfo, k8sEP)
	if err != nil {
		scopedLog.WithError(err).Error("Unable to delete K8s ingress")
		return err
	}
	delete(d.loadBalancer.K8sIngress, svcName)
	return nil
}

func (d *Daemon) syncExternalLB(newSN, modSN, delSN *loadbalancer.K8sServiceNamespace) error {
	deleteSN := func(delSN loadbalancer.K8sServiceNamespace) error {
		ingSvc, ok := d.loadBalancer.K8sIngress[delSN]
		if !ok {
			return nil
		}

		endpoint, ok := d.loadBalancer.K8sEndpoints[delSN]
		if !ok {
			return nil
		}

		if err := d.delK8sSVCs(delSN, ingSvc, endpoint); err != nil {
			return err
		}

		delete(d.loadBalancer.K8sServices, delSN)
		return nil
	}

	addSN := func(addSN loadbalancer.K8sServiceNamespace) error {
		ingressSvcInfo, ok := d.loadBalancer.K8sIngress[addSN]
		if !ok {
			return nil
		}

		k8sEP, ok := d.loadBalancer.K8sEndpoints[addSN]
		if !ok {
			return nil
		}

		err := d.addK8sSVCs(addSN, ingressSvcInfo, k8sEP)
		if err != nil {
			return err
		}
		return nil
	}

	if delSN != nil {
		// Clean old services
		return deleteSN(*delSN)
	}
	if modSN != nil {
		// Re-add modified services
		return addSN(*modSN)
	}
	if newSN != nil {
		// Add new services
		return addSN(*newSN)
	}
	return nil
}

// getUpdatedCNPFromStore gets the most recent version of cnp from the store
// ciliumV2Store, which is updated by the Kubernetes watcher. This reduces
// the possibility of Cilium trying to update cnp in Kubernetes which has
// been updated between the time the watcher in this Cilium instance has
// received cnp, and when this function is called. This still may occur, though
// and users of the returned CiliumNetworkPolicy may not be able to update
// the cnp because it may become out-of-date. Returns an error if the CNP cannot
// be retrieved from the store, or the object retrieved from the store is not of
// the expected type.
func getUpdatedCNPFromStore(ciliumV2Store cache.Store, cnp *cilium_v2.CiliumNetworkPolicy) (*cilium_v2.CiliumNetworkPolicy, error) {
	serverRuleStore, exists, err := ciliumV2Store.Get(cnp)
	if err != nil {
		return nil, fmt.Errorf("unable to find v2.CiliumNetworkPolicy in local cache: %s", err)
	}
	if !exists {
		return nil, errors.New("v2.CiliumNetworkPolicy does not exist in local cache")
	}

	serverRule, ok := serverRuleStore.(*cilium_v2.CiliumNetworkPolicy)
	if !ok {
		return nil, errors.New("Received object of unknown type from API server, expecting v2.CiliumNetworkPolicy")
	}

	return serverRule, nil
}

func (d *Daemon) updateCiliumNetworkPolicyV2AnnotationsOnly(ciliumV2Store cache.Store, cnp *cilium_v2.CiliumNetworkPolicy) {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.CiliumNetworkPolicyName: cnp.ObjectMeta.Name,
		logfields.K8sAPIVersion:           cnp.TypeMeta.APIVersion,
		logfields.K8sNamespace:            cnp.ObjectMeta.Namespace,
	})

	scopedLog.Infof("updating node status due to annotations-only change to CiliumNetworkPolicy")

	ctrlName := cnp.GetControllerName()

	// Revision will *always* be populated because importMetadataCache is guaranteed
	// to be updated by addCiliumNetworkPolicyV2 before calls to
	// updateCiliumNetworkPolicyV2 are invoked.
	meta, _ := importMetadataCache.get(cnp)

	k8sCM.UpdateController(ctrlName,
		controller.ControllerParams{
			DoFunc: func() error {
				return cnpNodeStatusController(ciliumV2Store, cnp, meta.revision, scopedLog, meta.policyImportError)
			},
		})

}

func (d *Daemon) addCiliumNetworkPolicyV2(ciliumV2Store cache.Store, cnp *cilium_v2.CiliumNetworkPolicy) error {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.CiliumNetworkPolicyName: cnp.ObjectMeta.Name,
		logfields.K8sAPIVersion:           cnp.TypeMeta.APIVersion,
		logfields.K8sNamespace:            cnp.ObjectMeta.Namespace,
	})

	scopedLog.Debug("Adding CiliumNetworkPolicy")

	var rev uint64

	rules, policyImportErr := cnp.Parse()
	if policyImportErr == nil && len(rules) > 0 {
		d.loadBalancer.K8sMU.Lock()
		policyImportErr = k8s.PreprocessRules(rules, d.loadBalancer.K8sEndpoints, d.loadBalancer.K8sServices)
		d.loadBalancer.K8sMU.Unlock()
		if policyImportErr == nil {
			rev, policyImportErr = d.PolicyAdd(rules, &AddOptions{Replace: true})
		}
	}

	if policyImportErr != nil {
		scopedLog.WithError(policyImportErr).Warn("Unable to add CiliumNetworkPolicy")
	} else {
		scopedLog.Info("Imported CiliumNetworkPolicy")
	}

	// Upsert to rule revision cache outside of controller, because upsertion
	// *must* be synchronous so that if we get an update for the CNP, the cache
	// is populated by the time updateCiliumNetworkPolicyV2 is invoked.
	importMetadataCache.upsert(cnp, rev, policyImportErr)

	ctrlName := cnp.GetControllerName()
	k8sCM.UpdateController(ctrlName,
		controller.ControllerParams{
			DoFunc: func() error {
				return cnpNodeStatusController(ciliumV2Store, cnp, rev, scopedLog, policyImportErr)
			},
		},
	)
	return policyImportErr
}

func cnpNodeStatusController(ciliumV2Store cache.Store, cnp *cilium_v2.CiliumNetworkPolicy, rev uint64, logger *logrus.Entry, policyImportErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	waitForEPsErr := endpointmanager.WaitForEndpointsAtPolicyRev(ctx, rev)

	// Number of attempts to retry updating of CNP in case that Update fails
	// due to out-of-date resource version.
	maxAttempts := 5

	var (
		cnpUpdateErr       error
		updateWaitDuration = time.Duration(200) * time.Millisecond
	)

	for numAttempts := 0; numAttempts < maxAttempts; numAttempts++ {

		serverRule, fromStoreErr := getUpdatedCNPFromStore(ciliumV2Store, cnp)
		if fromStoreErr != nil {
			logger.WithError(fromStoreErr).Error("error getting updated CNP from store")
			return fromStoreErr
		}

		// Make a copy since the rule is a pointer, and any of its fields
		// which are also pointers could be modified outside of this
		// function.
		serverRuleCpy := serverRule.DeepCopy()
		_, ruleCopyParseErr := serverRuleCpy.Parse()
		if ruleCopyParseErr != nil {
			// If we can't parse the rule then we should signalize
			// it in the status
			log.WithError(ruleCopyParseErr).WithField(logfields.Object, logfields.Repr(serverRuleCpy)).
				Warn("Error parsing new CiliumNetworkPolicy rule")
		}

		// Update the status of whether the rule is enforced on this node.
		// If we are unable to parse the CNP retrieved from the store,
		// or if endpoints did not reach the desired policy revision
		// after 30 seconds, then mark the rule as not being enforced.
		if policyImportErr != nil {
			// OK is false here because the policy wasn't imported into
			// cilium on this node; since it wasn't imported, it also
			// isn't enforced.
			cnpUpdateErr = updateCNPNodeStatus(serverRuleCpy, false, false, policyImportErr, rev, serverRuleCpy.Annotations)
		} else if ruleCopyParseErr != nil {
			// This handles the case where the initial instance of this
			// rule was imported into the policy repository successfully
			// (policyImportErr == nil), but, the rule has been updated
			// in the store soon after, and is now invalid. As such,
			// the rule is not OK because it cannot be imported due
			// to parsing errors, and cannot be enforced because it is
			// not OK.
			cnpUpdateErr = updateCNPNodeStatus(serverRuleCpy, false, false, ruleCopyParseErr, rev, serverRuleCpy.Annotations)
		} else {
			// If the deadline by the above context, then not all
			// endpoints are enforcing the given policy, and
			// waitForEpsErr will be non-nil.
			cnpUpdateErr = updateCNPNodeStatus(serverRuleCpy, waitForEPsErr == nil, true, waitForEPsErr, rev, serverRuleCpy.Annotations)
		}

		if cnpUpdateErr == nil {
			logger.WithField("status", serverRuleCpy.Status).Debug("successfully updated with status")
			break
		}

		// Wait a small amount of time to try to update again.
		logger.WithError(cnpUpdateErr).Warningf("update of CNP failed; sleeping for %s amount of time before trying again", updateWaitDuration)
		time.Sleep(updateWaitDuration)
	}

	if cnpUpdateErr != nil {
		return cnpUpdateErr
	} else {
		return waitForEPsErr
	}
}

func updateCNPNodeStatus(cnp *cilium_v2.CiliumNetworkPolicy, enforcing, ok bool, err error, rev uint64, annotations map[string]string) error {
	var (
		cnpns cilium_v2.CiliumNetworkPolicyNodeStatus
		err2  error
	)

	if err != nil {
		cnpns = cilium_v2.CiliumNetworkPolicyNodeStatus{
			Enforcing:   enforcing,
			Error:       err.Error(),
			OK:          ok,
			LastUpdated: cilium_v2.NewTimestamp(),
			Annotations: annotations,
		}
	} else {
		cnpns = cilium_v2.CiliumNetworkPolicyNodeStatus{
			Enforcing:   enforcing,
			Revision:    rev,
			OK:          ok,
			LastUpdated: cilium_v2.NewTimestamp(),
			Annotations: annotations,
		}
	}

	nodeName := node.GetName()
	cnp.SetPolicyStatus(nodeName, cnpns)
	ns := k8sUtils.ExtractNamespace(&cnp.ObjectMeta)

	switch {
	case ciliumUpdateStatusVerConstr.Check(k8sServerVer):
		_, err2 = ciliumNPClient.CiliumV2().CiliumNetworkPolicies(ns).UpdateStatus(cnp)
	default:
		_, err2 = ciliumNPClient.CiliumV2().CiliumNetworkPolicies(ns).Update(cnp)
	}
	return err2
}

func (d *Daemon) deleteCiliumNetworkPolicyV2(cnp *cilium_v2.CiliumNetworkPolicy) error {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.CiliumNetworkPolicyName: cnp.ObjectMeta.Name,
		logfields.K8sAPIVersion:           cnp.TypeMeta.APIVersion,
		logfields.K8sNamespace:            cnp.ObjectMeta.Namespace,
	})

	scopedLog.Debug("Deleting CiliumNetworkPolicy")

	importMetadataCache.delete(cnp)
	ctrlName := cnp.GetControllerName()
	err := k8sCM.RemoveController(ctrlName)
	if err != nil {
		log.Debugf("Unable to remove controller %s: %s", ctrlName, err)
	}

	labels := utils.GetPolicyLabels(cnp.ObjectMeta.Namespace, cnp.ObjectMeta.Name,
		utils.ResourceTypeCiliumNetworkPolicy)

	_, err = d.PolicyDelete(labels)
	if err == nil {
		scopedLog.Info("Deleted CiliumNetworkPolicy")
	} else {
		scopedLog.WithError(err).Warn("Unable to delete CiliumNetworkPolicy")
	}
	return err
}

func (d *Daemon) updateCiliumNetworkPolicyV2(ciliumV2Store cache.Store,
	oldRuleCpy, newRuleCpy *cilium_v2.CiliumNetworkPolicy) error {

	_, err := oldRuleCpy.Parse()
	if err != nil {
		log.WithError(err).WithField(logfields.Object, logfields.Repr(oldRuleCpy)).
			Warn("Error parsing old CiliumNetworkPolicy rule")
		return err
	}
	_, err = newRuleCpy.Parse()
	if err != nil {
		log.WithError(err).WithField(logfields.Object, logfields.Repr(newRuleCpy)).
			Warn("Error parsing new CiliumNetworkPolicy rule")
		return err
	}

	log.WithFields(logrus.Fields{
		logfields.K8sAPIVersion:                    oldRuleCpy.TypeMeta.APIVersion,
		logfields.CiliumNetworkPolicyName + ".old": oldRuleCpy.ObjectMeta.Name,
		logfields.K8sNamespace + ".old":            oldRuleCpy.ObjectMeta.Namespace,
		logfields.CiliumNetworkPolicyName:          newRuleCpy.ObjectMeta.Name,
		logfields.K8sNamespace:                     newRuleCpy.ObjectMeta.Namespace,
		"annotations.old":                          oldRuleCpy.ObjectMeta.Annotations,
		"annotations":                              newRuleCpy.ObjectMeta.Annotations,
	}).Debug("Modified CiliumNetworkPolicy")

	// Do not add rule into policy repository if the spec remains unchanged.
	if oldRuleCpy.SpecEquals(newRuleCpy) {
		if !oldRuleCpy.AnnotationsEquals(newRuleCpy) {

			// Update annotations within a controller so the status of the update
			// is trackable from the list of running controllers, and so we do
			// not block subsequent policy lifecycle operations from Kubernetes
			// until the update is complete.
			oldCtrlName := oldRuleCpy.GetControllerName()
			newCtrlName := newRuleCpy.GetControllerName()

			// In case the controller name changes between copies of rules,
			// remove old controller so we do not leak goroutines.
			if oldCtrlName != newCtrlName {
				err := k8sCM.RemoveController(oldCtrlName)
				if err != nil {
					log.Debugf("Unable to remove controller %s: %s", oldCtrlName, err)
				}
			}
			d.updateCiliumNetworkPolicyV2AnnotationsOnly(ciliumV2Store, newRuleCpy)
		}
		return nil
	}

	return d.addCiliumNetworkPolicyV2(ciliumV2Store, newRuleCpy)
}

// missingCNPv2 returns all missing policies from the given map.
func (d *Daemon) missingCNPv2(m versioned.Map) versioned.Map {
	missing := versioned.NewMap()
	d.policy.Mutex.RLock()
	for k, v := range m {
		cnp := v.Data.(*cilium_v2.CiliumNetworkPolicy)
		ruleLabels := cnp.GetRuleLabels()
		if !d.policy.ContainsAllRLocked(ruleLabels) {
			missing.Add(k, v)
		}
	}
	d.policy.Mutex.RUnlock()
	return missing
}

func (d *Daemon) updatePodHostIP(pod *v1.Pod) (bool, error) {
	if pod.Spec.HostNetwork {
		return true, fmt.Errorf("pod is using host networking")
	}

	hostIP := net.ParseIP(pod.Status.HostIP)
	if hostIP == nil {
		return true, fmt.Errorf("no/invalid HostIP: %s", pod.Status.HostIP)
	}

	podIP := net.ParseIP(pod.Status.PodIP)
	if podIP == nil {
		return true, fmt.Errorf("no/invalid PodIP: %s", pod.Status.PodIP)
	}

	selfOwned := ipcache.IPIdentityCache.Upsert(pod.Status.PodIP, hostIP, ipcache.Identity{
		ID:     identity.ReservedIdentityCluster,
		Source: ipcache.FromKubernetes,
	})
	if !selfOwned {
		return true, fmt.Errorf("ipcache entry owned by kvstore or agent")
	}

	return false, nil
}

func (d *Daemon) deletePodHostIP(pod *v1.Pod) (bool, error) {
	if pod.Spec.HostNetwork {
		return true, fmt.Errorf("pod is using host networking")
	}

	podIP := net.ParseIP(pod.Status.PodIP)
	if podIP == nil {
		return true, fmt.Errorf("no/invalid PodIP: %s", pod.Status.PodIP)
	}

	// a small race condition exists here as deletion could occur in
	// parallel based on another event but it doesn't matter as the
	// identity is going away
	id, exists := ipcache.IPIdentityCache.LookupByIP(pod.Status.PodIP)
	if !exists {
		return true, fmt.Errorf("identity for IP does not exist in case")
	}

	if id.Source != ipcache.FromKubernetes {
		return true, fmt.Errorf("ipcache entry not owned by kubernetes source")
	}

	ipcache.IPIdentityCache.Delete(pod.Status.PodIP)

	return false, nil
}

func (d *Daemon) addK8sPodV1(pod *v1.Pod) error {
	logger := log.WithFields(logrus.Fields{
		logfields.K8sPodName:   pod.ObjectMeta.Name,
		logfields.K8sNamespace: pod.ObjectMeta.Namespace,
		"podIP":                pod.Status.PodIP,
		"hostIP":               pod.Status.HostIP,
	})

	skipped, err := d.updatePodHostIP(pod)
	switch {
	case skipped:
		logger.WithError(err).Debug("Skipped ipcache map update on pod add")
		return nil
	case err != nil:
		logger.WithError(err).Warning("Unable to update ipcache map entry on pod add ")
	default:
		logger.Debug("Updated ipcache map entry on pod add")
	}
	return err
}

func (d *Daemon) updateK8sPodV1(oldK8sPod, newK8sPod *v1.Pod) error {
	if oldK8sPod == nil || newK8sPod == nil {
		return nil
	}

	// The pod IP can never change, it can only switch from unassigned to
	// assigned
	d.addK8sPodV1(newK8sPod)

	// We only care about label updates
	oldPodLabels := oldK8sPod.GetLabels()
	newPodLabels := newK8sPod.GetLabels()
	if comparator.MapStringEquals(oldPodLabels, newPodLabels) {
		return nil
	}

	podNSName := k8sUtils.GetObjNamespaceName(&newK8sPod.ObjectMeta)

	podEP := endpointmanager.LookupPodName(podNSName)
	if podEP == nil {
		log.WithField("pod", podNSName).Debugf("Endpoint not found running for the given pod")
		return nil
	}

	newLabels := labels.Map2Labels(newPodLabels, labels.LabelSourceK8s)
	newIdtyLabels, _ := labels.FilterLabels(newLabels)
	oldLabels := labels.Map2Labels(oldPodLabels, labels.LabelSourceK8s)
	oldIdtyLabels, _ := labels.FilterLabels(oldLabels)

	err := podEP.ModifyIdentityLabels(d, newIdtyLabels, oldIdtyLabels)
	if err != nil {
		log.WithError(err).Debugf("error while updating endpoint with new labels")
		return err
	}

	log.WithFields(logrus.Fields{
		logfields.EndpointID: podEP.GetID(),
		logfields.Labels:     logfields.Repr(newIdtyLabels),
	}).Debug("Update endpoint with new labels")
	return nil
}

func (d *Daemon) deleteK8sPodV1(pod *v1.Pod) error {
	logger := log.WithFields(logrus.Fields{
		logfields.K8sPodName:   pod.ObjectMeta.Name,
		logfields.K8sNamespace: pod.ObjectMeta.Namespace,
		"podIP":                pod.Status.PodIP,
		"hostIP":               pod.Status.HostIP,
	})

	skipped, err := d.deletePodHostIP(pod)
	switch {
	case skipped:
		logger.WithError(err).Debug("Skipped ipcache map delete on pod delete")
	case err != nil:
		logger.WithError(err).Warning("Unable to delete ipcache map entry on pod delete")
	default:
		logger.Debug("Deleted ipcache map entry on pod delete")
	}
	return err
}

// missingK8sPodV1 returns all pods considered missing, if the IP doesn't exist in
// the cache or if there is an endpoint associated with that pod that doesn't
// contain all labels from the pod.
func missingK8sPodV1(m versioned.Map) versioned.Map {
	missing := versioned.NewMap()
	for k, v := range m {
		pod := v.Data.(*v1.Pod)
		_, exists := ipcache.IPIdentityCache.LookupByIP(pod.Status.PodIP)
		// As we also look at pod events to populate ipcache we can only
		// consider that a pods is missing if it doesn't exist in the
		// identity cache.
		if !exists {
			missing.Add(k, v)
			continue
		}
		podNSName := k8sUtils.GetObjNamespaceName(&pod.ObjectMeta)

		podEP := endpointmanager.LookupPodName(podNSName)
		// Only 1 endpoint in the whole cluster is managing this pod, if it's
		// not found is due:
		// - pod is not being managed by this node;
		// - pod is running on this node but there are no endpoints associated
		//   with it. This association pod to endpoint association occurs when
		//   the endpoint is created, by the pkg/workload.
		if podEP == nil {
			continue
		}

		// If it doesn't contain all pod labels for an endpoint that is managing
		// that pod then the pod is also considered missing.
		k8sEPPodLabels := podEP.GetK8sPodLabels()

		podLabels := labels.Map2Labels(pod.GetLabels(), labels.LabelSourceK8s)
		podFilteredLabels, _ := labels.FilterLabels(podLabels)

		if !k8sEPPodLabels.Equals(podFilteredLabels) {
			missing.Add(k, v)
		}
	}
	return missing
}

func (d *Daemon) updateK8sV1Namespace(oldNS, newNS *v1.Namespace) error {
	if oldNS == nil || newNS == nil {
		return nil
	}

	// We only care about label updates
	if comparator.MapStringEquals(oldNS.GetLabels(), newNS.GetLabels()) {
		return nil
	}

	oldNSLabels := map[string]string{}
	newNSLabels := map[string]string{}

	for k, v := range oldNS.GetLabels() {
		oldNSLabels[policy.JoinPath(ciliumio.PodNamespaceMetaLabels, k)] = v
	}
	for k, v := range newNS.GetLabels() {
		newNSLabels[policy.JoinPath(ciliumio.PodNamespaceMetaLabels, k)] = v
	}

	oldLabels := labels.Map2Labels(oldNSLabels, labels.LabelSourceK8s)
	newLabels := labels.Map2Labels(newNSLabels, labels.LabelSourceK8s)

	oldIdtyLabels, _ := labels.FilterLabels(oldLabels)
	newIdtyLabels, _ := labels.FilterLabels(newLabels)

	eps := endpointmanager.GetEndpoints()
	failed := false
	for _, ep := range eps {
		epNS := ep.GetK8sNamespace()
		if oldNS.Name == epNS {
			err := ep.ModifyIdentityLabels(d, newIdtyLabels, oldIdtyLabels)
			if err != nil {
				log.WithError(err).WithField(logfields.EndpointID, ep.ID).
					Warningf("unable to update endpoint with new namespace labels")
				failed = true
			}
		}
	}
	if failed {
		return errors.New("unable to update some endpoints with new namespace labels")
	}
	return nil
}

// missingK8sNamespaceV1 returns all namespaces that don't have all of their
// labels in the namespace's endpoints.
func (d *Daemon) missingK8sNamespaceV1(m versioned.Map) versioned.Map {
	missing := versioned.NewMap()
	eps := endpointmanager.GetEndpoints()
	for k, v := range m {
		ns := v.Data.(*v1.Namespace)

		nsK8sLabels := map[string]string{}

		for k, v := range ns.GetLabels() {
			nsK8sLabels[policy.JoinPath(ciliumio.PodNamespaceMetaLabels, k)] = v
		}

		nsLabels := labels.Map2Labels(nsK8sLabels, labels.LabelSourceK8s)

		nsFilteredLabels, _ := labels.FilterLabels(nsLabels)

		for _, ep := range eps {
			epNS := ep.GetK8sNamespace()
			if ns.Name == epNS && !ep.HasLabels(nsFilteredLabels) {
				missing.Add(k, v)
				break
			}
		}
	}
	return missing
}

func (d *Daemon) updateK8sNodeTunneling(k8sNodeOld, k8sNodeNew *v1.Node) error {
	nodeNew := k8s.ParseNode(k8sNodeNew, node.FromKubernetes)
	// Ignore own node
	if nodeNew.Name == node.GetName() {
		return nil
	}

	getIDs := func(node *node.Node, k8sNode *v1.Node) (string, net.IP, error) {
		if node == nil {
			return "", nil, nil
		}
		hostIP := node.GetNodeIP(false)
		if ip4 := hostIP.To4(); ip4 == nil {
			return "", nil, fmt.Errorf("HostIP is not an IPv4 address: %s", hostIP)
		}

		ciliumIPStr := k8sNode.GetAnnotations()[annotation.CiliumHostIP]
		ciliumIP := net.ParseIP(ciliumIPStr)
		if ciliumIP == nil {
			return "", nil, fmt.Errorf("no/invalid Cilium-Host IP for host %s: %s", hostIP, ciliumIPStr)
		}

		return ciliumIPStr, hostIP, nil
	}

	ciliumIPStrNew, hostIPNew, err := getIDs(nodeNew, k8sNodeNew)
	if err != nil || ciliumIPStrNew == "" || hostIPNew == nil {
		return err
	}

	if k8sNodeOld != nil {
		nodeOld := k8s.ParseNode(k8sNodeOld, node.FromKubernetes)
		var (
			err            error
			ciliumIPStrOld string
		)
		ciliumIPStrOld, _, err = getIDs(nodeOld, k8sNodeOld)
		if err != nil {
			return err
		}

		if nodeNew.PublicAttrEquals(nodeOld) {
			// Ignore updates for the same node.
			return nil
		}

		// If the annotation of the node resource has changed, the old
		// ipcache has to be removed manually as Upsert() only handes
		// updates if the key itself is unchanged.
		if ciliumIPStrNew != ciliumIPStrOld {
			d.deleteK8sNodeV1(k8sNodeOld)
		}
	}

	selfOwned := ipcache.IPIdentityCache.Upsert(ciliumIPStrNew, hostIPNew, ipcache.Identity{
		ID:     identity.ReservedIdentityHost,
		Source: ipcache.FromKubernetes,
	})
	if !selfOwned {
		return fmt.Errorf("ipcache entry owned by kvstore or agent")
	}

	routeTypes := node.TunnelRoute

	// Add IPv6 routing only in non encap. With encap we do it with bpf tunnel
	// FIXME create a function to know on which mode is the daemon running on
	var ownAddr net.IP
	if option.Config.AutoIPv6NodeRoutes && option.Config.Device != "undefined" {
		ownAddr = node.GetIPv6()
		routeTypes |= node.DirectRoute
	}

	node.UpdateNode(nodeNew, routeTypes, ownAddr)

	return nil
}

func (d *Daemon) addK8sNodeV1(k8sNode *v1.Node) error {
	if err := d.updateK8sNodeTunneling(nil, k8sNode); err != nil {
		log.WithError(err).Warning("Unable to add ipcache entry of Kubernetes node")
		return err
	}
	return nil
}

func (d *Daemon) updateK8sNodeV1(k8sNodeOld, k8sNodeNew *v1.Node) error {
	if err := d.updateK8sNodeTunneling(k8sNodeOld, k8sNodeNew); err != nil {
		log.WithError(err).Warning("Unable to update ipcache entry of Kubernetes node")
		return err
	}
	return nil
}

func (d *Daemon) deleteK8sNodeV1(k8sNode *v1.Node) error {
	ip := k8sNode.GetAnnotations()[annotation.CiliumHostIP]

	logger := log.WithFields(logrus.Fields{
		"K8sNodeName":    k8sNode.ObjectMeta.Name,
		logfields.IPAddr: ip,
	})

	ni := node.Identity{
		Name:    k8sNode.ObjectMeta.Name,
		Cluster: option.Config.ClusterName,
	}

	node.DeleteNode(ni, node.TunnelRoute|node.DirectRoute)

	id, exists := ipcache.IPIdentityCache.LookupByIP(ip)
	if !exists {
		logger.Warning("identity for Cilium IP not found")
		return nil
	}

	// The ipcache entry ownership may have been taken over by a kvstore
	// based entry in which case we should ignore the delete event and wait
	// for the kvstore delete event.
	if id.Source != ipcache.FromKubernetes {
		logger.Debug("ipcache entry for Cilium IP no longer owned by Kubernetes")
		return nil
	}

	ipcache.IPIdentityCache.Delete(ip)

	ciliumIP := net.ParseIP(ip)
	if ciliumIP == nil {
		logger.Warning("Unable to parse Cilium IP")
		return nil
	}
	return nil
}

// updateK8sEventMetric incrment the given metric per event type and the result
// status of the function
func updateK8sEventMetric(scope string, action string, status bool) {
	result := "success"
	if status == false {
		result = "failed"
	}

	metrics.KubernetesEvent.WithLabelValues(scope, action, result).Inc()
}

// missingK8sNodeV1 checks if all nodes from the possible missing nodes have
// their CiliumHostIP missing from the ipcache or if the CiliumHostIP is
// associated with the right node.
func (d *Daemon) missingK8sNodeV1(m versioned.Map) versioned.Map {
	missing := versioned.NewMap()
	nodes := node.GetNodes()
	for k, v := range m {
		n := v.Data.(*v1.Node)
		ciliumHostIPStr := n.GetAnnotations()[annotation.CiliumHostIP]
		if ciliumHostIPStr == "" {
			continue
		}
		_, exists := ipcache.IPIdentityCache.LookupByIP(ciliumHostIPStr)
		// The node is considered missing if the Cilium HostIP of that
		// node doesn't exist in the identity cache.
		if !exists {
			missing.Add(k, v)
			continue
		}

		ciliumHostIP := net.ParseIP(ciliumHostIPStr)
		if ciliumHostIP == nil {
			continue
		}

		// Or if the CiliumHostIP is the right one for this node.
		nodeIdentity := node.Identity{Name: n.GetName(), Cluster: option.Config.ClusterName}
		var found bool
		for _, v := range nodes[nodeIdentity].IPAddresses {
			if v.IP.Equal(ciliumHostIP) {
				found = true
				break
			}
		}
		if !found {
			missing.Add(k, v)
		}
	}
	return missing
}
