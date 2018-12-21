// Copyright 2018 Google Inc. All Rights Reserved.
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

package fleetautoscalers

import (
	"encoding/json"
	"fmt"
	"time"

	"agones.dev/agones/pkg/apis/stable"
	stablev1alpha1 "agones.dev/agones/pkg/apis/stable/v1alpha1"
	"agones.dev/agones/pkg/client/clientset/versioned"
	getterv1alpha1 "agones.dev/agones/pkg/client/clientset/versioned/typed/stable/v1alpha1"
	"agones.dev/agones/pkg/client/informers/externalversions"
	listerv1alpha1 "agones.dev/agones/pkg/client/listers/stable/v1alpha1"
	"agones.dev/agones/pkg/util/crd"
	"agones.dev/agones/pkg/util/runtime"
	"agones.dev/agones/pkg/util/webhooks"
	"agones.dev/agones/pkg/util/workerqueue"
	"github.com/heptiolabs/healthcheck"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	admv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	extclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1beta1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

// Controller is a the FleetAutoscaler controller
type Controller struct {
	logger                *logrus.Entry
	crdGetter             v1beta1.CustomResourceDefinitionInterface
	fleetGetter           getterv1alpha1.FleetsGetter
	fleetLister           listerv1alpha1.FleetLister
	fleetAutoscalerGetter getterv1alpha1.FleetAutoscalersGetter
	fleetAutoscalerLister listerv1alpha1.FleetAutoscalerLister
	fleetAutoscalerSynced cache.InformerSynced
	workerqueue           *workerqueue.WorkerQueue
	recorder              record.EventRecorder
}

// NewController returns a controller for a FleetAutoscaler
func NewController(
	wh *webhooks.WebHook,
	health healthcheck.Handler,
	kubeClient kubernetes.Interface,
	extClient extclientset.Interface,
	agonesClient versioned.Interface,
	agonesInformerFactory externalversions.SharedInformerFactory) *Controller {

	agonesInformer := agonesInformerFactory.Stable().V1alpha1()
	fasInformer := agonesInformer.FleetAutoscalers().Informer()

	c := &Controller{
		crdGetter:             extClient.ApiextensionsV1beta1().CustomResourceDefinitions(),
		fleetGetter:           agonesClient.StableV1alpha1(),
		fleetLister:           agonesInformer.Fleets().Lister(),
		fleetAutoscalerGetter: agonesClient.StableV1alpha1(),
		fleetAutoscalerLister: agonesInformer.FleetAutoscalers().Lister(),
		fleetAutoscalerSynced: fasInformer.HasSynced,
	}
	c.logger = runtime.NewLoggerWithType(c)
	c.workerqueue = workerqueue.NewWorkerQueue(c.syncFleetAutoscaler, c.logger, stable.GroupName+".FleetAutoscalerController")
	health.AddLivenessCheck("fleetautoscaler-workerqueue", healthcheck.Check(c.workerqueue.Healthy))

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(c.logger.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	c.recorder = eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "fleetautoscaler-controller"})

	kind := stablev1alpha1.Kind("FleetAutoscaler")
	wh.AddHandler("/validate", kind, admv1beta1.Create, c.validationHandler)
	wh.AddHandler("/validate", kind, admv1beta1.Update, c.validationHandler)

	fasInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.workerqueue.Enqueue,
		UpdateFunc: func(_, newObj interface{}) {
			c.workerqueue.Enqueue(newObj)
		},
	})

	return c
}

// Run the FleetAutoscaler controller. Will block until stop is closed.
// Runs threadiness number workers to process the rate limited queue
func (c *Controller) Run(workers int, stop <-chan struct{}) error {
	err := crd.WaitForEstablishedCRD(c.crdGetter, "fleetautoscalers."+stable.GroupName, c.logger)
	if err != nil {
		return err
	}

	c.logger.Info("Wait for cache sync")
	if !cache.WaitForCacheSync(stop, c.fleetAutoscalerSynced) {
		return errors.New("failed to wait for caches to sync")
	}

	c.workerqueue.Run(workers, stop)
	return nil
}

// validationHandler will intercept when a FleetAutoscaler is created, and
// validate its settings.
func (c *Controller) validationHandler(review admv1beta1.AdmissionReview) (admv1beta1.AdmissionReview, error) {
	c.logger.WithField("review", review).Info("validationHandler")
	obj := review.Request.Object
	fas := &stablev1alpha1.FleetAutoscaler{}
	err := json.Unmarshal(obj.Raw, fas)
	if err != nil {
		return review, errors.Wrapf(err, "error unmarshalling original FleetAutoscaler json: %s", obj.Raw)
	}

	var causes []metav1.StatusCause
	causes = fas.Validate(causes)
	if len(causes) != 0 {
		review.Response.Allowed = false
		details := metav1.StatusDetails{
			Name:   review.Request.Name,
			Group:  review.Request.Kind.Group,
			Kind:   review.Request.Kind.Kind,
			Causes: causes,
		}
		review.Response.Result = &metav1.Status{
			Status:  metav1.StatusFailure,
			Message: "FleetAutoscaler is invalid",
			Reason:  metav1.StatusReasonInvalid,
			Details: &details,
		}
	}

	return review, nil
}

// syncFleetAutoscaler scales the attached fleet and
// synchronizes the FleetAutoscaler CRD
func (c *Controller) syncFleetAutoscaler(key string) error {
	c.logger.WithField("key", key).Info("Synchronising")

	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		// don't return an error, as we don't want this retried
		runtime.HandleError(c.logger.WithField("key", key), errors.Wrapf(err, "invalid resource key"))
		return nil
	}

	fas, err := c.fleetAutoscalerLister.FleetAutoscalers(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			c.logger.WithField("key", key).Info(fmt.Sprintf("FleetAutoscaler %s from namespace %s is no longer available for syncing", name, namespace))
			return nil
		}
		return errors.Wrapf(err, "error retrieving FleetAutoscaler %s from namespace %s", name, namespace)
	}

	// Retrieve the fleet by spec name
	fleet, err := c.fleetLister.Fleets(namespace).Get(fas.Spec.FleetName)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			logrus.WithError(err).WithField("fleetAutoscaler", fas.Name).
				WithField("fleet", fas.Spec.FleetName).
				WithField("namespace", namespace).
				Warn("Could not find fleet for autoscaler. Skipping.")

			c.recorder.Eventf(fas, corev1.EventTypeWarning, "FailedGetFleet",
				"could not fetch fleet: %s", fas.Spec.FleetName)

			// don't retry. Pick it up next sync.
			err = nil
		}

		if err := c.updateStatusUnableToScale(fas); err != nil {
			return err
		}

		return err
	}

	currentReplicas := fleet.Status.Replicas
	desiredReplicas, scalingLimited, err := computeDesiredFleetSize(fas, fleet)
	if err != nil {
		if err := c.updateStatusUnableToScale(fas); err != nil {
			return err
		}
		return errors.Wrapf(err, "error calculating autoscaling fleet: %s", fleet.ObjectMeta.Name)
	}

	// Scale the fleet to the new size
	if err = c.scaleFleet(fas, fleet, desiredReplicas); err != nil {
		return errors.Wrapf(err, "error autoscaling fleet %s to %d replicas", fas.Spec.FleetName, desiredReplicas)
	}

	return c.updateStatus(fas, currentReplicas, desiredReplicas, desiredReplicas != fleet.Spec.Replicas, scalingLimited)
}

// scaleFleet scales the fleet of the autoscaler to a new number of replicas
func (c *Controller) scaleFleet(fas *stablev1alpha1.FleetAutoscaler, f *stablev1alpha1.Fleet, replicas int32) error {
	if replicas != f.Spec.Replicas {
		fCopy := f.DeepCopy()
		fCopy.Spec.Replicas = replicas
		fCopy, err := c.fleetGetter.Fleets(f.ObjectMeta.Namespace).Update(fCopy)
		if err != nil {
			return errors.Wrapf(err, "error updating replicas for fleet %s", f.ObjectMeta.Name)
		}

		c.recorder.Eventf(fas, corev1.EventTypeNormal, "AutoScalingFleet",
			"Scaling fleet %s from %d to %d", fCopy.ObjectMeta.Name, f.Spec.Replicas, fCopy.Spec.Replicas)
	}

	return nil
}

// updateStatus updates the status of the given FleetAutoscaler
func (c *Controller) updateStatus(fas *stablev1alpha1.FleetAutoscaler, currentReplicas int32, desiredReplicas int32, scaled bool, scalingLimited bool) error {
	fasCopy := fas.DeepCopy()
	fasCopy.Status.AbleToScale = true
	fasCopy.Status.ScalingLimited = scalingLimited
	fasCopy.Status.CurrentReplicas = currentReplicas
	fasCopy.Status.DesiredReplicas = desiredReplicas
	if scaled {
		now := metav1.NewTime(time.Now())
		fasCopy.Status.LastScaleTime = &now
	}

	if !apiequality.Semantic.DeepEqual(fas.Status, fasCopy.Status) {
		if scalingLimited {
			c.recorder.Eventf(fas, corev1.EventTypeWarning, "ScalingLimited", "Scaling fleet %s was limited to maximum size of %d", fas.Spec.FleetName, desiredReplicas)
		}

		_, err := c.fleetAutoscalerGetter.FleetAutoscalers(fas.ObjectMeta.Namespace).Update(fasCopy)
		if err != nil {
			return errors.Wrapf(err, "error updating status for fleetautoscaler %s", fas.ObjectMeta.Name)
		}
	}

	return nil
}

// updateStatus updates the status of the given FleetAutoscaler in the case we're not able to scale
func (c *Controller) updateStatusUnableToScale(fas *stablev1alpha1.FleetAutoscaler) error {
	fasCopy := fas.DeepCopy()
	fasCopy.Status.AbleToScale = false
	fasCopy.Status.ScalingLimited = false
	fasCopy.Status.CurrentReplicas = 0
	fasCopy.Status.DesiredReplicas = 0

	if !apiequality.Semantic.DeepEqual(fas.Status, fasCopy.Status) {
		_, err := c.fleetAutoscalerGetter.FleetAutoscalers(fas.ObjectMeta.Namespace).Update(fasCopy)
		if err != nil {
			return errors.Wrapf(err, "error updating status for fleetautoscaler %s", fas.ObjectMeta.Name)
		}
	}

	return nil
}
