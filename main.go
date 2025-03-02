/*
Copyright 2024 The Kubernetes Authors.

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

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	networkinginformers "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/kubernetes"
	networkinglisters "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	finalizer          = "policies.networking.k8s.io/finalizer"
	leaseLockName      = "network-policy-finalizer"
	leaseLockNamespace = "kube-system"
)

// Controller demonstrates how to implement a controller with client-go.
type Controller struct {
	client                kubernetes.Interface
	networkpolicyLister   networkinglisters.NetworkPolicyLister
	networkpoliciesSynced cache.InformerSynced
	queue                 workqueue.RateLimitingInterface
}

// NewController creates a new Controller.
func NewController(
	client kubernetes.Interface,
	networkpolicyInformer networkinginformers.NetworkPolicyInformer,
) *Controller {
	c := &Controller{
		client:                client,
		networkpolicyLister:   networkpolicyInformer.Lister(),
		networkpoliciesSynced: networkpolicyInformer.Informer().HasSynced,
		queue:                 workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "network-policy-finalizer"),
	}

	networkpolicyInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{ // nolint:errcheck
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				c.queue.Add(key)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(newObj)
			if err == nil {
				c.queue.Add(key)
			}
		},
	})

	return c
}

func (c *Controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.sync(key.(string))
	c.handleErr(err, key.(string))
	return true
}

func (c *Controller) sync(key string) error {
	start := time.Now()
	klog.V(2).Infof("sync network policy %s", key)
	defer func() {
		klog.V(2).Infof("sync network policy %s took %v", key, time.Since(start))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	networkPolicy, err := c.networkpolicyLister.NetworkPolicies(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	// if is being deleted wait for the Pods to be deleted before removing the finalizer
	if !networkPolicy.DeletionTimestamp.IsZero() {
		klog.V(2).Infof("network policy %s/%s is being deleted", namespace, name)
		ns, err := c.client.CoreV1().Namespaces().Get(context.Background(), namespace, metav1.GetOptions{})
		if err != nil {
			return err
		}
		// namespace is being deleted but the namespace is not
		if ns.DeletionTimestamp.IsZero() {
			klog.V(2).Infof("namespace %s is not being deleted, removing network policy", namespace)
			return c.deleteFinalizer(networkPolicy)
		}

		// namespace is being deleted, wait for the Pods to be deleted
		podStore, podController := cache.NewInformer(
			&cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					podList, err := c.client.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
					return podList, err
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return c.client.CoreV1().Pods(namespace).Watch(context.Background(), metav1.ListOptions{})
				},
			},
			&v1.Pod{},
			0,
			cache.ResourceEventHandlerFuncs{},
		)
		ctx := context.Background()
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		klog.V(2).Infof("waiting for pods on namespace %s to disappear", namespace)
		go podController.Run(ctx.Done())
		if !cache.WaitForCacheSync(ctx.Done(), podController.HasSynced) {
			return fmt.Errorf("timed out waiting for caches to sync")
		}
		err = wait.PollUntilContextCancel(context.Background(), 1*time.Second, true, func(ctx context.Context) (done bool, err error) {
			list := podStore.List()
			if len(list) > 0 {
				klog.V(2).Infof("waiting for %d pods on namespace %s to disappear", len(list), namespace)
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			return err
		}
		return c.deleteFinalizer(networkPolicy)
	}
	return c.addFinalizer(networkPolicy)
}

func (c *Controller) addFinalizer(networkPolicy *networkingv1.NetworkPolicy) error {
	// check if there is a finalizer and add it if is missing
	if slices.Contains(networkPolicy.GetFinalizers(), finalizer) {
		return nil
	}

	// add the finalizer
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": []string{finalizer},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = c.client.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Patch(context.Background(), networkPolicy.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	klog.V(2).InfoS("Added protection finalizer from NetworkPolicy", "NetworkPolicy", networkPolicy)
	return nil
}

func (c *Controller) deleteFinalizer(networkPolicy *networkingv1.NetworkPolicy) error {
	if !slices.Contains(networkPolicy.GetFinalizers(), finalizer) {
		return nil
	}

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"$deleteFromPrimitiveList/finalizers": []string{finalizer},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	_, err = c.client.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Patch(context.Background(), networkPolicy.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	klog.V(2).InfoS("Removed protection finalizer from NetworkPolicy", "NetworkPolicy", networkPolicy)
	return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleErr(err error, key string) {
	if err == nil {
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		klog.Infof("Error syncing NetworkPolicy %v: %v", key, err)

		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	utilruntime.HandleError(err)
	klog.Infof("Dropping network policy %q out of the queue: %v", key, err)
}

// Run begins watching and syncing.
func (c *Controller) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	defer c.queue.ShutDown()
	klog.Info("Starting network policy termination controller")

	if !cache.WaitForCacheSync(stopCh, c.networkpoliciesSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	klog.Info("Stopping Network Policy Terminator controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	klog.Infof("flags: %v", flag.Args())
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	id := uuid.New().String()
	run := func(ctx context.Context) {
		// complete your controller loop here
		klog.Info("Controller loop...")

		informersFactory := informers.NewSharedInformerFactory(clientset, 0)
		controller := NewController(clientset, informersFactory.Networking().V1().NetworkPolicies())

		informersFactory.Start(ctx.Done())
		controller.Run(5, ctx.Done())
	}

	// trap Ctrl+C and call cancel on the context
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	// Enable signal handler
	signalCh := make(chan os.Signal, 2)
	defer func() {
		close(signalCh)
		cancel()
	}()
	signal.Notify(signalCh, os.Interrupt, unix.SIGINT)

	go func() {
		select {
		case <-signalCh:
			klog.Infof("Exiting: received signal")
			cancel()
		case <-ctx.Done():
		}
	}()

	// we use the Lease lock type since edits to Leases are less common
	// and fewer objects in the cluster watch "all Leases".
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      leaseLockName,
			Namespace: leaseLockNamespace,
		},
		Client: clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}

	// start the leader election code loop
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   60 * time.Second,
		RenewDeadline:   15 * time.Second,
		RetryPeriod:     5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				run(ctx)
			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				klog.Infof("leader lost: %s", id)
				os.Exit(0)
			},
			OnNewLeader: func(identity string) {
				if identity == id {
					return
				}
				klog.Infof("new leader elected: %s", identity)
			},
		},
	})
}
