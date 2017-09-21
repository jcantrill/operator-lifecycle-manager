package alm

import (
	"time"

	"fmt"

	"github.com/coreos-inc/alm/operators"
	"github.com/coreos-inc/operator-client/pkg/client"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	defaultQPS   = 100
	defaultBurst = 100
)

type Operator struct {
	queue    workqueue.RateLimitingInterface
	informer cache.SharedIndexInformer
	opClient client.Interface
}

func New(kubeconfig string) (*Operator, error) {
	client := client.NewClient(kubeconfig)

	operator := &Operator{
		opClient: client,
	}
	operator.queue = workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "alm")
	operatorVersionWatcher := cache.NewListWatchFromClient(
		client.KubernetesInterface().CoreV1().RESTClient(),
		"operatorversions",
		metav1.NamespaceAll,
		fields.Everything(),
	)
	operator.informer = cache.NewSharedIndexInformer(
		operatorVersionWatcher,
		&OperatorVersion{},
		15*time.Minute,
		cache.Indexers{},
	)
	operator.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: operator.handleAddOperatorVersion,
	})
	return operator, nil
}

func (o *Operator) Run(stopc <-chan struct{}) error {
	defer o.queue.ShutDown()

	errChan := make(chan error)
	go func() {
		v, err := o.opClient.KubernetesInterface().Discovery().ServerVersion()
		if err != nil {
			errChan <- errors.Wrap(err, "communicating with server failed")
			return
		}
		log.Info("msg", "connection established", "cluster-version", v)
		errChan <- nil
	}()

	select {
	case err := <-errChan:
		if err != nil {
			return err
		}
		log.Info("msg", "Operator ready")
	case <-stopc:
		return nil
	}

	go o.worker()
	go o.informer.Run(stopc)

	<-stopc
	return nil
}

func (o *Operator) keyFunc(obj interface{}) (string, bool) {
	k, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Info("msg", "creating key failed", "err", err)
		return k, false
	}

	return k, true
}

// enqueue adds a key to the queue. If obj is a key already it gets added directly.
// Otherwise, the key is extracted via keyFunc.
func (o *Operator) enqueue(obj interface{}) {
	if obj == nil {
		return
	}

	key, ok := obj.(string)
	if !ok {
		key, ok = o.keyFunc(obj)
		if !ok {
			return
		}
	}

	o.queue.Add(key)
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (c *Operator) worker() {
	for c.processNextWorkItem() {
	}
}

func (o *Operator) processNextWorkItem() bool {
	key, quit := o.queue.Get()
	if quit {
		return false
	}
	defer o.queue.Done(key)

	err := o.sync(key.(string))
	if err == nil {
		o.queue.Forget(key)
		return true
	}

	utilruntime.HandleError(errors.Wrap(err, fmt.Sprintf("Sync %q failed", key)))
	o.queue.AddRateLimited(key)

	return true
}

func (o *Operator) sync(key string) error {
	obj, exists, err := o.informer.GetIndexer().GetByKey(key)
	if err != nil {
		return err
	}

	if !exists {
		// For now, we ignore the case where an OperatorVersionSpec used to exist but no longer does
		return nil
	}

	operatorVersion, ok := obj.(*OperatorVersion)
	if !ok {
		return fmt.Errorf("casting OperatorVersionSpec failed")
	}

	log.Info("msg", "sync OperatorVersionSpec", "key", key)
	install := operatorVersion.Spec.InstallStrategy.UnstructuredContent()
	strategy := install["strategy"]
	strategyString, ok := strategy.(string)
	if !ok {
		return fmt.Errorf("casting strategy failed")
	}
	if strategyString == "deployment" {
		kubeDeployment := alm.NewKubeDeployment(o.opClient)
		kubeDeployment.Install(operatorVersion.ObjectMeta.Namespace, install["deployments"])
	}

	return nil
}

func (o *Operator) handleAddOperatorVersion(obj interface{}) {
	key, ok := o.keyFunc(obj)
	if !ok {
		return
	}
	log.Info("msg", "OperatorVersionSpec added", "key", key)
	o.enqueue(key)
}
