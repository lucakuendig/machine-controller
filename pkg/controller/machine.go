/*
Copyright 2017 The Kubernetes Authors.

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
	"fmt"
	"regexp"
	"time"

	"github.com/golang/glog"
	machineclientset "github.com/kubermatic/machine-controller/pkg/client/clientset/versioned"
	"github.com/kubermatic/machine-controller/pkg/client/informers/externalversions"
	machinelistersv1alpha1 "github.com/kubermatic/machine-controller/pkg/client/listers/machines/v1alpha1"
	"github.com/kubermatic/machine-controller/pkg/cloudprovider"
	cloudprovidererrors "github.com/kubermatic/machine-controller/pkg/cloudprovider/errors"
	"github.com/kubermatic/machine-controller/pkg/cloudprovider/instance"
	"github.com/kubermatic/machine-controller/pkg/containerruntime"
	"github.com/kubermatic/machine-controller/pkg/containerruntime/crio"
	"github.com/kubermatic/machine-controller/pkg/containerruntime/docker"
	machinev1alpha1 "github.com/kubermatic/machine-controller/pkg/machines/v1alpha1"
	"github.com/kubermatic/machine-controller/pkg/providerconfig"
	"github.com/kubermatic/machine-controller/pkg/ssh"
	"github.com/kubermatic/machine-controller/pkg/userdata"

	"github.com/kubermatic/machine-controller/pkg/cloudprovider/cloud"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"
)

const (
	finalizerDeleteInstance = "machine-delete-finalizer"
)

// Controller is the controller implementation for Foo resources
type Controller struct {
	kubeClient    kubernetes.Interface
	machineClient machineclientset.Interface

	nodesLister    listerscorev1.NodeLister
	nodesSynced    cache.InformerSynced
	machinesLister machinelistersv1alpha1.MachineLister
	machinesSynced cache.InformerSynced

	workqueue workqueue.RateLimitingInterface

	sshPrivateKey *ssh.PrivateKey
}

// NewMachineController returns a new machine controller
func NewMachineController(
	kubeClient kubernetes.Interface,
	machineClient machineclientset.Interface,
	kubeInformerFactory kubeinformers.SharedInformerFactory,
	machineInformerFactory externalversions.SharedInformerFactory,
	sshKeypair *ssh.PrivateKey) *Controller {

	nodeInformer := kubeInformerFactory.Core().V1().Nodes()
	machineInformer := machineInformerFactory.Machine().V1alpha1().Machines()

	controller := &Controller{
		kubeClient:  kubeClient,
		nodesLister: nodeInformer.Lister(),
		nodesSynced: nodeInformer.Informer().HasSynced,

		machineClient:  machineClient,
		machinesLister: machineInformer.Lister(),
		machinesSynced: machineInformer.Informer().HasSynced,

		workqueue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemFastSlowRateLimiter(2*time.Second, 10*time.Second, 5), "Machines"),

		sshPrivateKey: sshKeypair,
	}

	machineInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueMachine,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueMachine(new)
		},
	})

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleObject,
		UpdateFunc: func(old, new interface{}) {
			newNode := new.(*corev1.Node)
			oldNode := old.(*corev1.Node)
			if newNode.ResourceVersion == oldNode.ResourceVersion {
				return
			}
			controller.handleObject(new)
		},
		DeleteFunc: controller.handleObject,
	})

	return controller
}

// Run starts the control loop
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.nodesSynced, c.machinesSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	return nil
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	key, quit := c.workqueue.Get()
	if quit {
		return false
	}

	defer c.workqueue.Done(key)

	glog.V(6).Infof("Processing machine: %s", key)
	err := c.syncHandler(key.(string))
	if err == nil {
		c.workqueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", key, err))
	c.workqueue.AddRateLimited(key)

	return true
}

func (c *Controller) clearMachineErrorIfSet(machine *machinev1alpha1.Machine, reason machinev1alpha1.MachineStatusError) (*machinev1alpha1.Machine, error) {
	if machine.Status.ErrorReason != nil && *machine.Status.ErrorReason == reason {
		machine.Status.ErrorMessage = nil
		machine.Status.ErrorReason = nil
		machine.Status.LastUpdated = metav1.Now()
		return c.machineClient.MachineV1alpha1().Machines().Update(machine)
	}
	return machine, nil
}

func (c *Controller) updateMachineError(machine *machinev1alpha1.Machine, reason machinev1alpha1.MachineStatusError, message string) (*machinev1alpha1.Machine, error) {
	if machine.Status.ErrorReason == nil || *machine.Status.ErrorReason == reason {
		machine.Status.ErrorMessage = &message
		machine.Status.ErrorReason = &reason
		machine.Status.LastUpdated = metav1.Now()
		return c.machineClient.MachineV1alpha1().Machines().Update(machine)
	}
	return machine, nil
}

func (c *Controller) syncHandler(key string) error {
	listerMachine, err := c.machinesLister.Get(key)
	if err != nil {
		if kerrors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("machine '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}
	machine := listerMachine.DeepCopy()

	providerConfig, err := providerconfig.GetConfig(machine.Spec.ProviderConfig)
	if err != nil {
		return fmt.Errorf("failed to get provider config: %v", err)
	}
	prov, err := cloudprovider.ForProvider(providerConfig.CloudProvider, c.sshPrivateKey)
	if err != nil {
		return fmt.Errorf("failed to get cloud provider %q: %v", providerConfig.CloudProvider, err)
	}

	// Delete machine
	if machine.DeletionTimestamp != nil && sets.NewString(machine.Finalizers...).Has(finalizerDeleteInstance) {
		if err := prov.Delete(machine); err != nil {
			if machine, err = c.updateMachineError(machine, machinev1alpha1.DeleteMachineError, err.Error()); err != nil {
				return fmt.Errorf("failed to update machine error after failed delete: %v", err)
			}
			return fmt.Errorf("failed to delete machine at cloudprovider: %v", err)
		}

		//Check that the instance has really gone
		i, err := prov.Get(machine)
		if err == nil {
			return fmt.Errorf("instance %s is not deleted yet", i.ID())
		} else if err != cloudprovidererrors.ErrInstanceNotFound {
			return fmt.Errorf("failed to check instance %s after the delete got triggered", i.ID())
		}

		glog.V(4).Infof("Deleted machine %s at cloud provider", machine.Name)
		// Delete delete instance finalizer
		finalizers := sets.NewString(machine.Finalizers...)
		finalizers.Delete(finalizerDeleteInstance)
		machine.Finalizers = finalizers.List()
		if machine, err = c.machineClient.MachineV1alpha1().Machines().Update(machine); err != nil {
			return fmt.Errorf("failed to update machine after removing the delete instance finalizer: %v", err)
		}
		glog.V(4).Infof("Removed delete finalizer from machine %s", machine.Name)

		// Remove error message in case it was set
		if machine, err = c.clearMachineErrorIfSet(machine, machinev1alpha1.DeleteMachineError); err != nil {
			return fmt.Errorf("failed to update machine after removing the delete error: %v", err)
		}

		return nil
	}

	// Create the delete finalizer before actually creating the instance.
	// otherwise the machine gets created at the cloud provider and the machine resource gets deleted meanwhile
	// which causes a orphaned instance
	if !sets.NewString(machine.Finalizers...).Has(finalizerDeleteInstance) {
		finalizers := sets.NewString(machine.Finalizers...)
		finalizers.Insert(finalizerDeleteInstance)
		machine.Finalizers = finalizers.List()
		if machine, err = c.machineClient.MachineV1alpha1().Machines().Update(machine); err != nil {
			return fmt.Errorf("failed to update machine after adding the delete instance finalizer: %v", err)
		}
		glog.V(4).Infof("Added delete finalizer to machine %s", machine.Name)
	}

	userdataProvider, err := userdata.ForOS(providerConfig.OperatingSystem)
	if err != nil {
		return fmt.Errorf("failed to userdata provider for '%s': %v", providerConfig.OperatingSystem, err)
	}
	if machine, err = c.defaultContainerRuntime(machine, userdataProvider); err != nil {
		return fmt.Errorf("failed to default the container runtime version: %v", err)
	}

	providerInstance, err := prov.Get(machine)
	if err != nil {
		if err == cloudprovidererrors.ErrInstanceNotFound {
			if err := prov.Validate(machine.Spec); err != nil {
				if machine, err = c.updateMachineError(machine, machinev1alpha1.InvalidConfigurationMachineError, err.Error()); err != nil {
					return fmt.Errorf("failed to update machine error after failed validation: %v", err)
				}
				return fmt.Errorf("invalid provider config: %v", err)
			}
			// Remove error message in case it was set
			if machine, err = c.clearMachineErrorIfSet(machine, machinev1alpha1.InvalidConfigurationMachineError); err != nil {
				return fmt.Errorf("failed to update machine after removing the failed validation error: %v", err)
			}
			glog.V(4).Infof("Validated machine spec of %s", machine.Name)

			providerInstance, err = c.createProviderInstance(machine, prov, providerConfig, userdataProvider)
			if err != nil {
				if machine, err = c.updateMachineError(machine, machinev1alpha1.CreateMachineError, err.Error()); err != nil {
					return fmt.Errorf("failed to update machine error after failed machine creation: %v", err)
				}
				return fmt.Errorf("failed to create machine at cloudprovider: %v", err)
			}
			// Remove error message in case it was set
			if machine, err = c.clearMachineErrorIfSet(machine, machinev1alpha1.CreateMachineError); err != nil {
				return fmt.Errorf("failed to update machine after removing the create machine error: %v", err)
			}
			glog.V(4).Infof("Created machine %s at cloud provider", machine.Name)

		} else {
			return fmt.Errorf("failed to get instance from provider: %v", err)
		}
	}

	node, exists, err := c.getNode(providerInstance, string(providerConfig.CloudProvider))
	if err != nil {
		return fmt.Errorf("failed to get node for machine %s: %v", machine.Name, err)
	}
	if exists {
		ownerRef := metav1.GetControllerOf(node)
		if ownerRef == nil {
			gv := machinev1alpha1.SchemeGroupVersion
			node.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(machine, gv.WithKind("Machine"))}
			node, err = c.kubeClient.CoreV1().Nodes().Update(node)
			if err != nil {
				return fmt.Errorf("failed to update node %s after adding the owner ref: %v", node.Name, err)
			}
			glog.V(4).Infof("Added owner ref to node %s (machine %s)", node.Name, machine.Name)
		}

		if node.Spec.ConfigSource == nil && machine.Spec.ConfigSource != nil {
			node.Spec.ConfigSource = machine.Spec.ConfigSource
			node, err = c.kubeClient.CoreV1().Nodes().Update(node)
			if err != nil {
				return fmt.Errorf("failed to update node %s after setting the config source: %v", node.Name, err)
			}
			glog.V(4).Infof("Added config source to node %s (machine %s)", node.Name, machine.Name)
		}

		var labelsUpdated bool
		for k, v := range machine.Spec.Labels {
			if _, exists := node.Labels[k]; !exists {
				labelsUpdated = true
				node.Labels[k] = v
			}
		}
		if labelsUpdated {
			node, err = c.kubeClient.CoreV1().Nodes().Update(node)
			if err != nil {
				return fmt.Errorf("failed to update node %s after setting the labels: %v", node.Name, err)
			}
			glog.V(4).Infof("Added labels to node %s (machine %s)", node.Name, machine.Name)
		}

		var annotationsUpdated bool
		for k, v := range machine.Spec.Annotations {
			if _, exists := node.Annotations[k]; !exists {
				annotationsUpdated = true
				node.Annotations[k] = v
			}
		}
		if annotationsUpdated {
			node, err = c.kubeClient.CoreV1().Nodes().Update(node)
			if err != nil {
				return fmt.Errorf("failed to update node %s after setting the annotations: %v", node.Name, err)
			}
			glog.V(4).Infof("Added annotations to node %s (machine %s)", node.Name, machine.Name)
		}

		taintExists := func(node *corev1.Node, taint corev1.Taint) bool {
			for _, t := range node.Spec.Taints {
				if t.MatchTaint(&taint) {
					return true
				}
			}
			return false
		}
		var taintsUpdated bool
		for _, t := range machine.Spec.Taints {
			if !taintExists(node, t) {
				node.Spec.Taints = append(node.Spec.Taints, t)
				taintsUpdated = true
			}
		}
		if taintsUpdated {
			node, err = c.kubeClient.CoreV1().Nodes().Update(node)
			if err != nil {
				return fmt.Errorf("failed to update node %s after setting the taints: %v", node.Name, err)
			}
			glog.V(4).Infof("Added taints to node %s (machine %s)", node.Name, machine.Name)
		}
	}

	err = c.updateMachineStatus(machine, node)
	if err != nil {
		return fmt.Errorf("failed to update machine status: %v", err)
	}
	return nil
}

func (c *Controller) updateMachineStatus(machine *machinev1alpha1.Machine, node *corev1.Node) error {
	if node == nil {
		return nil
	}

	var (
		updated                     bool
		runtimeName, runtimeVersion string
		err                         error
	)
	if machine.Status.NodeRef == nil {
		ref, err := reference.GetReference(scheme.Scheme, node)
		if err != nil {
			return fmt.Errorf("failed to get node reference for %s : %v", node.Name, err)
		}
		machine.Status.NodeRef = ref
		updated = true
	}

	if machine.Status.Versions == nil {
		machine.Status.Versions = &machinev1alpha1.MachineVersionInfo{}
	}

	if node.Status.NodeInfo.ContainerRuntimeVersion != "" {
		runtimeName, runtimeVersion, err = parseContainerRuntime(node.Status.NodeInfo.ContainerRuntimeVersion)
		if err != nil {
			glog.V(2).Infof("failed to parse container runtime from node %s: %v", node.Name, err)
			runtimeName = "unknown"
			runtimeVersion = "unknown"
		}
		if machine.Status.Versions.ContainerRuntime.Name != runtimeName || machine.Status.Versions.ContainerRuntime.Version != runtimeVersion {
			machine.Status.Versions.ContainerRuntime.Name = runtimeName
			machine.Status.Versions.ContainerRuntime.Version = runtimeVersion
			updated = true
		}
	}

	if machine.Status.Versions.Kubelet != node.Status.NodeInfo.KubeletVersion {
		machine.Status.Versions.Kubelet = node.Status.NodeInfo.KubeletVersion
		updated = true
	}

	if updated {
		machine.Status.LastUpdated = metav1.Now()
		if machine, err = c.machineClient.MachineV1alpha1().Machines().Update(machine); err != nil {
			return fmt.Errorf("failed to update machine: %v", err)
		}
	}
	return nil
}

var (
	containerRuntime = regexp.MustCompile(`(docker|cri-o)://(.*)`)
)

func parseContainerRuntime(s string) (runtime, version string, err error) {
	res := containerRuntime.FindStringSubmatch(s)
	if len(res) == 3 {
		return res[1], res[2], nil
	}
	return "", "", fmt.Errorf("invalid format. Expected 'runtime://version'")
}

func (c *Controller) getNode(instance instance.Instance, provider string) (node *corev1.Node, exists bool, err error) {
	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		return nil, false, err
	}

	providerID := fmt.Sprintf("%s:///%s", provider, instance.ID())
	for _, node := range nodes {
		if node.Spec.ProviderID == providerID {
			return node.DeepCopy(), true, nil
		}
		for _, nodeAddress := range node.Status.Addresses {
			for _, instanceAddress := range instance.Addresses() {
				if nodeAddress.Address == instanceAddress {
					return node.DeepCopy(), true, nil
				}
			}
		}
	}
	return nil, false, nil
}

func (c *Controller) defaultContainerRuntime(machine *machinev1alpha1.Machine, prov userdata.Provider) (*machinev1alpha1.Machine, error) {
	var err error
	if machine.Spec.Versions.ContainerRuntime.Name == "" {
		machine.Spec.Versions.ContainerRuntime.Name = containerruntime.Docker
		if machine, err = c.machineClient.MachineV1alpha1().Machines().Update(machine); err != nil {
			return nil, err
		}
	}

	if machine.Spec.Versions.ContainerRuntime.Version == "" {
		var (
			defaultVersions []string
			err             error
		)
		switch machine.Spec.Versions.ContainerRuntime.Name {
		case containerruntime.Docker:
			defaultVersions, err = docker.GetOfficiallySupportedVersions(machine.Spec.Versions.Kubelet)
			if err != nil {
				return nil, fmt.Errorf("failed to get a officially supported docker version for the given kubelet version: %v", err)
			}
		case containerruntime.CRIO:
			defaultVersions, err = crio.GetOfficiallySupportedVersions(machine.Spec.Versions.Kubelet)
			if err != nil {
				return nil, fmt.Errorf("failed to get a officially supported cri-o version for the given kubelet version: %v", err)
			}
		default:
			return nil, fmt.Errorf("invalid container runtime. Supported: '%s', '%s' ", containerruntime.Docker, containerruntime.CRIO)
		}

		providerSupportedVersions := prov.SupportedContainerRuntimes()
		for _, v := range defaultVersions {
			for _, sv := range providerSupportedVersions {
				if sv.Version == v {
					// we should not return asap as we prefer the highest supported version
					machine.Spec.Versions.ContainerRuntime.Version = sv.Version
				}
			}
		}
		if machine.Spec.Versions.ContainerRuntime.Version == "" {
			return nil, fmt.Errorf("no supported versions available for '%s'", machine.Spec.Versions.ContainerRuntime.Name)
		}
		if machine, err = c.machineClient.MachineV1alpha1().Machines().Update(machine); err != nil {
			return nil, err
		}
	}

	return machine, nil
}

func (c *Controller) createProviderInstance(machine *machinev1alpha1.Machine, prov cloud.Provider, providerConfig *providerconfig.Config, userdataProvider userdata.Provider) (instance.Instance, error) {
	kubeconfig, err := c.createBootstrapKubeconfig(machine.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to create bootstrap kubeconfig: %v", err)
	}

	data, err := userdataProvider.UserData(machine.Spec, kubeconfig, prov)
	if err != nil {
		return nil, fmt.Errorf("failed get userdata: %v", err)
	}

	glog.Infof("creating instance...")
	return prov.Create(machine, data)
}

func (c *Controller) enqueueMachine(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

func (c *Controller) handleObject(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		glog.V(4).Infof("Recovered deleted object '%s' from tombstone", object.GetName())
	}
	glog.V(6).Infof("Processing object: %s", object.GetName())
	if ownerRef := metav1.GetControllerOf(object); ownerRef != nil {
		if ownerRef.Kind != "Machine" {
			return
		}
		machine, err := c.machinesLister.Get(ownerRef.Name)
		if err != nil {
			glog.V(4).Infof("ignoring orphaned object '%s' of machine '%s'", object.GetSelfLink(), ownerRef.Name)
			return
		}

		c.enqueueMachine(machine)
		return
	}
}
