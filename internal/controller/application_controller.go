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
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	vernaldevv1alpha1 "vernaldev/vernal-operator/api/v1alpha1"
)

const (
	applicationFinalizer           = "vernal.dev/app-cleanup"
	applicationStatusTypeAvailable = "Available"
	applicationStatusTypeDegraded  = "Degraded"
)

// ApplicationReconciler reconciles a Application object
type ApplicationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

//+kubebuilder:rbac:groups=vernal.dev,resources=applications,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=vernal.dev,resources=applications/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=vernal.dev,resources=applications/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.0/pkg/reconcile
func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var application vernaldevv1alpha1.Application
	if err := r.Get(ctx, req.NamespacedName, &application); err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("Application resource not found; Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to fetch Application")
		return ctrl.Result{}, err
	}

	// Let's just set the status as Unknown when no status is available
	if application.Status.Conditions == nil || len(application.Status.Conditions) == 0 {
		meta.SetStatusCondition(
			&application.Status.Conditions,
			metav1.Condition{
				Type:    applicationStatusTypeAvailable,
				Status:  metav1.ConditionUnknown,
				Reason:  "Reconciling",
				Message: "Starting reconciliation",
			},
		)

		if err := r.Status().Update(ctx, &application); err != nil {
			log.Error(err, "Failed to update Application status")
			return ctrl.Result{}, err
		}

		// Let's re-fetch the Application Custom Resource after updating the status
		// so that we have the latest state of the resource on the cluster and we will avoid
		// raising the error "the object has been modified, please apply
		// your changes to the latest version and try again" which would re-trigger the reconciliation
		// if we try to update it again in the following operations
		if err := r.Get(ctx, req.NamespacedName, &application); err != nil {
			log.Error(err, "Failed to re-fetch Application")
			return ctrl.Result{}, err
		}
	}

	if !controllerutil.ContainsFinalizer(&application, applicationFinalizer) {
		log.Info("Adding finalizer to Application")
		if ok := controllerutil.AddFinalizer(&application, applicationFinalizer); !ok {
			log.Error(nil, "Failed to add finalizer to Application")
			return ctrl.Result{Requeue: true}, nil
		}

		if err := r.Update(ctx, &application); err != nil {
			log.Error(err, "Failed to update Application to add finalizer")
			return ctrl.Result{}, err
		}
	}

	isApplicationMarkedToBeDeleted := application.GetDeletionTimestamp() != nil
	if isApplicationMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(&application, applicationFinalizer) {
			log.Info("Performing resource cleanup for Application before deletion")

			// Let's add here a status "Downgrade" to reflect that this resource began its process to be terminated.
			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeDegraded,
					Status:  metav1.ConditionUnknown,
					Reason:  "Finalizing",
					Message: "Performing resource cleanup for Application before deletion",
				},
			)

			if err := r.Status().Update(ctx, &application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			// Perform all operations required before removing the finalizer and allow
			// the Kubernetes API to remove the custom resource.
			r.performApplicationCleanup(&application)

			// Re-fetch the Application Custom Resource before updating the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raising the error "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, &application); err != nil {
				log.Error(err, "Failed to re-fetch Application")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeDegraded,
					Status:  metav1.ConditionTrue,
					Reason:  "Finalizing",
					Message: "Resource cleanup for Application was successful",
				},
			)

			if err := r.Status().Update(ctx, &application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			log.Info("Removing finalizer for Application after successful resource cleanup")
			if ok := controllerutil.RemoveFinalizer(&application, applicationFinalizer); !ok {
				log.Error(nil, "Failed to remove finalizer for Application")
				return ctrl.Result{Requeue: true}, nil
			}

			if err := r.Update(ctx, &application); err != nil {
				log.Error(err, "Failed to remove finalizer for Application")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	namespaceName := fmt.Sprintf("vernal-%s-%s", application.Spec.Owner, application.GetName())
	namespace := v1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: namespaceName}, &namespace)

	if err != nil && apierrors.IsNotFound(err) {
		namespace, err := r.namespaceForApplication(&application)

		if err != nil {
			log.Error(err, "Failed to define new Namespace resource for Application")

			// The following implementation will update the status
			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Failed to create new Namespace resource for Application: %s", err),
				},
			)

			if err := r.Status().Update(ctx, &application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating new Namespace", "namespaceName", namespace.Name)

		if err := r.Create(ctx, namespace); err != nil {
			log.Error(err, "Failed to create new Namespace", "namespaceName", namespace.Name)
			return ctrl.Result{}, err
		}

		// Namespace created successfully
	} else if err != nil {
		log.Error(err, "Failed to get Namespace for Application")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// TODO: apply namespace changes?

	createdNewDeployments := false
	updatedDeployments := false
	for _, component := range application.Spec.Components {
		deployment, err := r.deploymentForApplicationComponent(&application, &component)
		if err != nil {
			log.Error(err, "Failed to define Deployment resource for Application component")

			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Failed to create new Deployment resource for Application component %s: %s", component.Name, err),
				},
			)

			if err := r.Status().Update(ctx, &application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		found := appsv1.Deployment{}
		err = r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, &found)
		if err != nil && apierrors.IsNotFound(err) {
			log.Info("Creating new Deployment", "deploymentName", deployment.Name, "deploymentNamespace", deployment.Namespace)

			if err := r.Create(ctx, deployment); err != nil {
				log.Error(err, "Failed to create new Deployment", "deploymentName", deployment.Name, "deploymentNamespace", deployment.Namespace)
				return ctrl.Result{}, err
			}

			// Deployment created successfully
			createdNewDeployments = true
			continue
		} else if err != nil {
			log.Error(err, "Failed to get Deployment for Application component")
			// Let's return the error for the reconciliation be re-trigged again
			return ctrl.Result{}, err
		}

		if err := r.Update(ctx, deployment, client.DryRunAll); err != nil {
			log.Error(err, "Failed to perform client dry-run of desired deployment state for Application component")

			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Failed to perform client dry-run of desired deployment state for Application component %s: %s", component.Name, err),
				},
			)

			if err := r.Status().Update(ctx, &application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		if reflect.DeepEqual(found.Spec, deployment.Spec) {
			// Deployment has not changed; skip update
			continue
		}

		updatedDeployments = true
		if err := r.Update(ctx, deployment); err != nil {
			log.Error(err, "Failed to apply desired deployment state for Application component")

			// Let's re-fetch the Application Custom Resource after updating the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raising the error "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			// if we try to update it again in the following operations
			if err := r.Get(ctx, req.NamespacedName, &application); err != nil {
				log.Error(err, "Failed to re-fetch Application")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Failed to apply desired deployment state for Application component %s: %s", component.Name, err),
				},
			)

			if err := r.Status().Update(ctx, &application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}
	}

	if createdNewDeployments {
		// Deployments created successfully
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	if updatedDeployments {
		// Now, that we updated the deployments, we want to requeue the reconciliation
		// so that we can ensure that we have the latest state of the resource before
		// update. Also, it will help ensure the desired state on the cluster
		return ctrl.Result{Requeue: true}, nil
	}

	meta.SetStatusCondition(
		&application.Status.Conditions,
		metav1.Condition{
			Type:    applicationStatusTypeAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  "Reconciling",
			Message: "Successfully applied desired state for Application",
		},
	)

	if err := r.Status().Update(ctx, &application); err != nil {
		log.Error(err, "Failed to update Application status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ApplicationReconciler) performApplicationCleanup(application *vernaldevv1alpha1.Application) {
	// Note: It is not recommended to use finalizers with the purpose of deleting resources which are
	// created and managed in the reconciliation. These ones, such as the Deployment created on this reconcile,
	// are defined as dependent of the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	// The following implementation will raise an event
	r.Recorder.Event(
		application,
		"Warning",
		"Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s", application.Name, application.Namespace),
	)
}

func (r *ApplicationReconciler) namespaceForApplication(application *vernaldevv1alpha1.Application) (*v1.Namespace, error) {
	namespaceName := fmt.Sprintf("vernal-%s-%s", application.Spec.Owner, application.GetName())
	labels := labelsForApplicationNamespace(application.GetName(), namespaceName)

	namespace := v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   namespaceName,
			Labels: labels,
		},
	}

	if err := ctrl.SetControllerReference(application, &namespace, r.Scheme); err != nil {
		return nil, err
	}

	return &namespace, nil
}

func (r *ApplicationReconciler) deploymentForApplicationComponent(application *vernaldevv1alpha1.Application, component *vernaldevv1alpha1.ApplicationSpecComponent) (*appsv1.Deployment, error) {
	namespaceName := fmt.Sprintf("vernal-%s-%s", application.Spec.Owner, application.GetName())
	deploymentName := fmt.Sprintf("vernal-%s-%s-%s", application.Spec.Owner, application.GetName(), component.Name)

	// TODO: Implement environment variables through Sealed Secrets
	// secretName := fmt.Sprintf("vernal-%s-%s-%s-secret", application.Spec.Owner, application.GetName(), component.Name)

	labels := labelsForApplicationComponent(application.GetName(), component.Name, component.Image)
	var replicas int32 = 1

	deployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: namespaceName,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: v1.PodSpec{Containers: []v1.Container{{
					Name:            component.Name,
					Image:           component.Image,
					ImagePullPolicy: v1.PullAlways,
					Ports:           []v1.ContainerPort{{ContainerPort: int32(component.Port)}},

					// TODO: Implement environment variables through Sealed Secrets
					// EnvFrom: []v1.EnvFromSource{{SecretRef: &v1.SecretEnvSource{LocalObjectReference: v1.LocalObjectReference{
					// 	Name: secretName,
					// }}}},
				}}},
			},
		},
	}

	if err := ctrl.SetControllerReference(application, &deployment, r.Scheme); err != nil {
		return nil, err
	}

	return &deployment, nil
}

// labelsForApplicationNamespace returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForApplicationNamespace(appName string, namespace string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       namespace,
		"app.kubernetes.io/part-of":    appName,
		"app.kubernetes.io/managed-by": "vernal-operator",

		// For injection of Istio sidecars
		"istio-injection": "enabled",
	}
}

// labelsForApplicationComponent returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForApplicationComponent(appName string, componentName string, image string) map[string]string {
	imageTag := strings.Split(image, ":")[1]

	return map[string]string{
		"app.kubernetes.io/name":       componentName,
		"app.kubernetes.io/version":    imageTag,
		"app.kubernetes.io/part-of":    appName,
		"app.kubernetes.io/managed-by": "vernal-operator",
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vernaldevv1alpha1.Application{}).
		Owns(&appsv1.Deployment{}).
		Owns(&v1.Namespace{}).
		Complete(r)
}
