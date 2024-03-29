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

	rediscommon "github.com/OT-CONTAINER-KIT/redis-operator/api"
	redisv1beta2 "github.com/OT-CONTAINER-KIT/redis-operator/api/v1beta2"
	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

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
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=redis.redis.opstreelabs.in,resources=redis,verbs=get;list;watch;create;update;patch;delete

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

	if res, err := r.ReconcileNamespace(ctx, req, &application); !res.IsZero() || err != nil {
		return res, err
	}

	if res, err := r.ReconcilePostgres(ctx, req, &application); !res.IsZero() || err != nil {
		return res, err
	}

	if res, err := r.ReconcileRedis(ctx, req, &application); !res.IsZero() || err != nil {
		return res, err
	}

	if res, err := r.ReconcileDeployments(ctx, req, &application); !res.IsZero() || err != nil {
		return res, err
	}

	if res, err := r.ReconcileHorizontalPodAutoscaler(ctx, req, &application); !res.IsZero() || err != nil {
		return res, err
	}

	if res, err := r.ReconcileServices(ctx, req, &application); !res.IsZero() || err != nil {
		return res, err
	}

	if res, err := r.ReconcileHTTPRoutes(ctx, req, &application); !res.IsZero() || err != nil {
		return res, err
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

func (r *ApplicationReconciler) ReconcileNamespace(ctx context.Context, req ctrl.Request, application *vernaldevv1alpha1.Application) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	namespaceName := fmt.Sprintf("vernal-%s-%s", application.Spec.Owner, application.GetName())
	namespace := v1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: namespaceName}, &namespace)

	if err != nil && apierrors.IsNotFound(err) {
		namespace, err := r.namespaceForApplication(application)

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

			if err := r.Status().Update(ctx, application); err != nil {
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

	return ctrl.Result{}, nil
}

func (r *ApplicationReconciler) ReconcilePostgres(ctx context.Context, req ctrl.Request, application *vernaldevv1alpha1.Application) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	postgresName := make(map[string]struct{})
	postgresExists := struct{}{}
	postgresEnabled := application.Spec.Postgres.Enabled

	postgres, err := r.postgresStandaloneForApplication(application)

	if err != nil {
		log.Error(err, "Failed to define Postgres resource for Application")

		meta.SetStatusCondition(
			&application.Status.Conditions,
			metav1.Condition{
				Type:    applicationStatusTypeAvailable,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Failed to create new Postgres resource for Application: %s", err),
			},
		)

		if err := r.Status().Update(ctx, application); err != nil {
			log.Error(err, "Failed to update Application status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, err
	}

	postgresName[postgres.Name] = postgresExists

	found := cnpgv1.Cluster{}
	err = r.Get(ctx, types.NamespacedName{Name: postgres.Name, Namespace: postgres.Namespace}, &found)

	// If postgres does not exist
	if err != nil && apierrors.IsNotFound(err) {
		// If enabled is true, create a postgres deployment
		if postgresEnabled {
			log.Info("Creating Postgres", "postgresName", postgres.Name, "postgresNamespace", postgres.Namespace)

			if err := r.Create(ctx, postgres); err != nil {
				log.Error(err, "Failed to create Postgres", "postgresName", postgres.Name, "postgresNamespace", postgres.Namespace)
				return ctrl.Result{}, err
			}

			// Deployment created successfully
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}

		return ctrl.Result{}, nil
	} else if err != nil {
		// Actual error occurred
		log.Error(err, "Failed to get Postgres for Application")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	} else {
		//Otherwise, postgres exists, check if postgres is enabled
		if postgresEnabled {
			postgres.SetResourceVersion(found.GetResourceVersion())

			if err := r.Update(ctx, postgres, client.DryRunAll); err != nil {
				log.Error(err, "Failed to perform client dry-run of desired Postgres state for Application component")

				meta.SetStatusCondition(
					&application.Status.Conditions,
					metav1.Condition{
						Type:    applicationStatusTypeAvailable,
						Status:  metav1.ConditionFalse,
						Reason:  "Reconciling",
						Message: fmt.Sprintf("Failed to perform client dry-run of desired Postgres state for Application: %s", err),
					},
				)

				if err := r.Status().Update(ctx, application); err != nil {
					log.Error(err, "Failed to update Application status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}

			if !reflect.DeepEqual(found.Spec, postgres.Spec) {
				// Deployment has changed, so need to update postgres
				log.Info("Updating Postgres", "postgresName", postgres.Name, "postgresNamespace", postgres.Namespace)

				if err := r.Update(ctx, postgres); err != nil {
					log.Error(err, "Failed to apply desired Postgres state for Application")

					// Let's re-fetch the Application Custom Resource after updating the status
					// so that we have the latest state of the resource on the cluster and we will avoid
					// raising the error "the object has been modified, please apply
					// your changes to the latest version and try again" which would re-trigger the reconciliation
					// if we try to update it again in the following operations
					if err := r.Get(ctx, req.NamespacedName, application); err != nil {
						log.Error(err, "Failed to re-fetch Application")
						return ctrl.Result{}, err
					}

					meta.SetStatusCondition(
						&application.Status.Conditions,
						metav1.Condition{
							Type:    applicationStatusTypeAvailable,
							Status:  metav1.ConditionFalse,
							Reason:  "Reconciling",
							Message: fmt.Sprintf("Failed to apply desired Postgres state for Application: %s", err),
						},
					)

					if err := r.Status().Update(ctx, application); err != nil {
						log.Error(err, "Failed to update Application status")
						return ctrl.Result{}, err
					}

					return ctrl.Result{}, err
				}

				// Now, that we updated Postgres, we want to requeue the reconciliation
				// so that we can ensure that we have the latest state of the resource before
				// update. Also, it will help ensure the desired state on the cluster
				return ctrl.Result{Requeue: true}, nil
			}

			return ctrl.Result{}, nil
		} else {
			// Otherwise, postgres is not enabled, so postgres should be removed from the deployment
			log.Info("Postgres not enabled, deleting Postgres", "postgresName", postgres.Name, "postgresNamespace", postgres.Namespace)

			if err := r.Delete(ctx, postgres); err != nil {
				log.Error(err, "Failed to delete Postgres", "postgresName", postgres.Name, "postgresNamespace", postgres.Namespace)

				meta.SetStatusCondition(
					&application.Status.Conditions,
					metav1.Condition{
						Type:    applicationStatusTypeAvailable,
						Status:  metav1.ConditionFalse,
						Reason:  "Reconciling",
						Message: fmt.Sprintf("Failed to delete Postgres resource for Application: %s", err),
					},
				)

				if err := r.Status().Update(ctx, application); err != nil {
					log.Error(err, "Failed to update Application status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}
	}
}

func (r *ApplicationReconciler) ReconcileRedis(ctx context.Context, req ctrl.Request, application *vernaldevv1alpha1.Application) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	/* logic for creating redis

	if redis does not exist
		if enabled is true
			add redis
		if enabled is false
			do nothing
	else if there's an error
		log error and return
	else redis exists
		if enabled is true
			if there is a change
				update deployment
			else
				do nothing
		if enabled is false
			remove redis

	*/

	redisName := make(map[string]struct{})
	redisExists := struct{}{}
	redisEnabled := application.Spec.Redis.Enabled

	redis, err := r.redisStandaloneForApplication(application)

	if err != nil {
		log.Error(err, "Failed to define Redis resource for Application")

		meta.SetStatusCondition(
			&application.Status.Conditions,
			metav1.Condition{
				Type:    applicationStatusTypeAvailable,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Failed to create new Redis resource for Application: %s", err),
			},
		)

		if err := r.Status().Update(ctx, application); err != nil {
			log.Error(err, "Failed to update Application status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, err
	}

	redisName[redis.Name] = redisExists

	found := redisv1beta2.Redis{}
	err = r.Get(ctx, types.NamespacedName{Name: redis.Name, Namespace: redis.Namespace}, &found)

	// If redis does not exist
	if err != nil && apierrors.IsNotFound(err) {
		// If enabled is true, create a redis deployment
		if redisEnabled {
			log.Info("Creating Redis", "redisName", redis.Name, "redisNamespace", redis.Namespace)

			if err := r.Create(ctx, redis); err != nil {
				log.Error(err, "Failed to create Redis", "redisName", redis.Name, "redisNamespace", redis.Namespace)
				return ctrl.Result{}, err
			}

			// Deployment created successfully
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}

		return ctrl.Result{}, nil
	} else if err != nil {
		// Actual error occurred
		log.Error(err, "Failed to get Redis for Application")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	} else {
		//Otherwise, redis exists, check if redis is enabled
		if redisEnabled {
			redis.SetResourceVersion(found.GetResourceVersion())

			if err := r.Update(ctx, redis, client.DryRunAll); err != nil {
				log.Error(err, "Failed to perform client dry-run of desired Redis state for Application component")

				meta.SetStatusCondition(
					&application.Status.Conditions,
					metav1.Condition{
						Type:    applicationStatusTypeAvailable,
						Status:  metav1.ConditionFalse,
						Reason:  "Reconciling",
						Message: fmt.Sprintf("Failed to perform client dry-run of desired Redis state for Application: %s", err),
					},
				)

				if err := r.Status().Update(ctx, application); err != nil {
					log.Error(err, "Failed to update Application status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}

			if !reflect.DeepEqual(found.Spec, redis.Spec) {
				// Deployment has changed, so need to update redis
				log.Info("Updating Redis", "redisName", redis.Name, "redisNamespace", redis.Namespace)

				if err := r.Update(ctx, redis); err != nil {
					log.Error(err, "Failed to apply desired Redis state for Application")

					// Let's re-fetch the Application Custom Resource after updating the status
					// so that we have the latest state of the resource on the cluster and we will avoid
					// raising the error "the object has been modified, please apply
					// your changes to the latest version and try again" which would re-trigger the reconciliation
					// if we try to update it again in the following operations
					if err := r.Get(ctx, req.NamespacedName, application); err != nil {
						log.Error(err, "Failed to re-fetch Application")
						return ctrl.Result{}, err
					}

					meta.SetStatusCondition(
						&application.Status.Conditions,
						metav1.Condition{
							Type:    applicationStatusTypeAvailable,
							Status:  metav1.ConditionFalse,
							Reason:  "Reconciling",
							Message: fmt.Sprintf("Failed to apply desired Redis state for Application: %s", err),
						},
					)

					if err := r.Status().Update(ctx, application); err != nil {
						log.Error(err, "Failed to update Application status")
						return ctrl.Result{}, err
					}

					return ctrl.Result{}, err
				}

				// Now, that we updated Redis, we want to requeue the reconciliation
				// so that we can ensure that we have the latest state of the resource before
				// update. Also, it will help ensure the desired state on the cluster
				return ctrl.Result{Requeue: true}, nil
			}

			return ctrl.Result{}, nil
		} else {
			// Otherwise, redis is not enabled, so redis should be removed from the deployment
			log.Info("Redis not enabled, deleting Redis", "redisName", redis.Name, "redisNamespace", redis.Namespace)

			if err := r.Delete(ctx, redis); err != nil {
				log.Error(err, "Failed to delete Redis", "redisName", redis.Name, "redisNamespace", redis.Namespace)

				meta.SetStatusCondition(
					&application.Status.Conditions,
					metav1.Condition{
						Type:    applicationStatusTypeAvailable,
						Status:  metav1.ConditionFalse,
						Reason:  "Reconciling",
						Message: fmt.Sprintf("Failed to delete Redis resource for Application: %s", err),
					},
				)

				if err := r.Status().Update(ctx, application); err != nil {
					log.Error(err, "Failed to update Application status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}
	}
}

func (r *ApplicationReconciler) ReconcileDeployments(ctx context.Context, req ctrl.Request, application *vernaldevv1alpha1.Application) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	deploymentNames := make(map[string]struct{})
	deploymentExists := struct{}{}

	createdDeployments := false
	updatedDeployments := false
	for _, component := range application.Spec.Components {
		deployment, err := r.deploymentForApplicationComponent(application, &component)
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

			if err := r.Status().Update(ctx, application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		deploymentNames[deployment.Name] = deploymentExists

		found := appsv1.Deployment{}
		err = r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, &found)
		if err != nil && apierrors.IsNotFound(err) {
			log.Info("Creating new Deployment", "deploymentName", deployment.Name, "deploymentNamespace", deployment.Namespace)

			if err := r.Create(ctx, deployment); err != nil {
				log.Error(err, "Failed to create new Deployment", "deploymentName", deployment.Name, "deploymentNamespace", deployment.Namespace)
				return ctrl.Result{}, err
			}

			// Deployment created successfully
			createdDeployments = true
			continue
		} else if err != nil {
			log.Error(err, "Failed to get Deployment for Application component")
			// Let's return the error for the reconciliation be re-trigged again
			return ctrl.Result{}, err
		}

		deployment.Spec.Replicas = found.Spec.Replicas
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

			if err := r.Status().Update(ctx, application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		if reflect.DeepEqual(found.Spec, deployment.Spec) {
			// Deployment has not changed; skip update
			continue
		}

		log.Info("Updating Deployment", "deploymentName", deployment.Name, "deploymentNamespace", deployment.Namespace)

		updatedDeployments = true
		if err := r.Update(ctx, deployment); err != nil {
			log.Error(err, "Failed to apply desired deployment state for Application component")

			// Let's re-fetch the Application Custom Resource after updating the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raising the error "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			// if we try to update it again in the following operations
			if err := r.Get(ctx, req.NamespacedName, application); err != nil {
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

			if err := r.Status().Update(ctx, application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}
	}

	deploymentList := appsv1.DeploymentList{}
	namespaceName := fmt.Sprintf("vernal-%s-%s", application.Spec.Owner, application.Name)

	if err := r.List(
		ctx,
		&deploymentList,
		client.InNamespace(namespaceName),
		client.MatchingLabels{
			"app.kubernetes.io/part-of":    application.Name,
			"app.kubernetes.io/managed-by": "vernal-operator",
		},
	); err != nil {
		log.Error(err, "Failed to list Deployment resources for Application")

		meta.SetStatusCondition(
			&application.Status.Conditions,
			metav1.Condition{
				Type:    applicationStatusTypeAvailable,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Failed to list Deployment resources for Application: %s", err),
			},
		)

		if err := r.Status().Update(ctx, application); err != nil {
			log.Error(err, "Failed to update Application status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, err
	}

	for _, deployment := range deploymentList.Items {
		if _, ok := deploymentNames[deployment.Name]; !ok {
			log.Info("Deleting old Deployment", "deploymentName", deployment.Name, "deploymentNamespace", deployment.Namespace)

			if err := r.Delete(ctx, &deployment); err != nil {
				log.Error(err, "Failed to delete Deployment resource for Application", "deploymentName", deployment.Name, "deploymentNamespace", deployment.Namespace)

				meta.SetStatusCondition(
					&application.Status.Conditions,
					metav1.Condition{
						Type:    applicationStatusTypeAvailable,
						Status:  metav1.ConditionFalse,
						Reason:  "Reconciling",
						Message: fmt.Sprintf("Failed to delete Deployment resource for Application: %s", err),
					},
				)

				if err := r.Status().Update(ctx, application); err != nil {
					log.Error(err, "Failed to update Application status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}
		}
	}

	if createdDeployments {
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

	return ctrl.Result{}, nil
}

func (r *ApplicationReconciler) ReconcileHorizontalPodAutoscaler(ctx context.Context, req ctrl.Request, application *vernaldevv1alpha1.Application) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	autoscalerNames := make(map[string]struct{})
	autoscalerExists := struct{}{}

	createdAutoscaler := false
	updatedAutoscaler := false
	for _, component := range application.Spec.Components {
		autoscaler, err := r.hpaForApplicationComponent(application, &component)
		if err != nil {
			log.Error(err, "Failed to define Autoscaler resource for Application component")

			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Failed to create new Autoscaler resource for Application component %s: %s", component.Name, err),
				},
			)

			if err := r.Status().Update(ctx, application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		autoscalerNames[autoscaler.Name] = autoscalerExists

		found := autoscalingv2.HorizontalPodAutoscaler{}
		err = r.Get(ctx, types.NamespacedName{Name: autoscaler.Name, Namespace: autoscaler.Namespace}, &found)
		if err != nil && apierrors.IsNotFound(err) {
			log.Info("Creating new Autoscaler", "autoscalerName", autoscaler.Name, "autoscalerNamespace", autoscaler.Namespace)

			if err := r.Create(ctx, autoscaler); err != nil {
				log.Error(err, "Failed to create new Autoscaler", "autoscalerName", autoscaler.Name, "autoscalerNamespace", autoscaler.Namespace)
				return ctrl.Result{}, err
			}

			// Autoscaler created successfully
			createdAutoscaler = true
			continue
		} else if err != nil {
			log.Error(err, "Failed to get Autoscaler for Application component")
			// Let's return the error for the reconciliation be re-trigged again
			return ctrl.Result{}, err
		}

		if err := r.Update(ctx, autoscaler, client.DryRunAll); err != nil {
			log.Error(err, "Failed to perform client dry-run of desired autoscaler state for Application component")

			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Failed to perform client dry-run of desired autoscaler state for Application component %s: %s", component.Name, err),
				},
			)

			if err := r.Status().Update(ctx, application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		if reflect.DeepEqual(found.Spec, autoscaler.Spec) {
			// Autoscaler has not changed; skip update
			continue
		}

		log.Info("Updating Autoscaler", "autoscalerName", autoscaler.Name, "autoscalerNamespace", autoscaler.Namespace)

		updatedAutoscaler = true
		if err := r.Update(ctx, autoscaler); err != nil {
			log.Error(err, "Failed to apply desired autoscaler state for Application component")

			// Let's re-fetch the Application Custom Resource after updating the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raising the error "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			// if we try to update it again in the following operations
			if err := r.Get(ctx, req.NamespacedName, application); err != nil {
				log.Error(err, "Failed to re-fetch Application")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Failed to apply desired autoscaler state for Application component %s: %s", component.Name, err),
				},
			)

			if err := r.Status().Update(ctx, application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}
	}

	autoscalerList := autoscalingv2.HorizontalPodAutoscalerList{}
	namespaceName := fmt.Sprintf("vernal-%s-%s", application.Spec.Owner, application.Name)

	if err := r.List(
		ctx,
		&autoscalerList,
		client.InNamespace(namespaceName),
		client.MatchingLabels{
			"app.kubernetes.io/part-of":    application.Name,
			"app.kubernetes.io/managed-by": "vernal-operator",
		},
	); err != nil {
		log.Error(err, "Failed to list Autoscaler resources for Application")

		meta.SetStatusCondition(
			&application.Status.Conditions,
			metav1.Condition{
				Type:    applicationStatusTypeAvailable,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Failed to list Autoscaler resources for Application: %s", err),
			},
		)

		if err := r.Status().Update(ctx, application); err != nil {
			log.Error(err, "Failed to update Application status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, err
	}

	for _, autoscaler := range autoscalerList.Items {
		if _, ok := autoscalerNames[autoscaler.Name]; !ok {
			log.Info("Deleting old Autoscaler", "autoscalerName", autoscaler.Name, "autoscalerNamespace", autoscaler.Namespace)

			if err := r.Delete(ctx, &autoscaler); err != nil {
				log.Error(err, "Failed to delete Autoscaler resource for Application", "autoscalerName", autoscaler.Name, "autoscalerNamespace", autoscaler.Namespace)

				meta.SetStatusCondition(
					&application.Status.Conditions,
					metav1.Condition{
						Type:    applicationStatusTypeAvailable,
						Status:  metav1.ConditionFalse,
						Reason:  "Reconciling",
						Message: fmt.Sprintf("Failed to delete Autoscaler resource for Application: %s", err),
					},
				)

				if err := r.Status().Update(ctx, application); err != nil {
					log.Error(err, "Failed to update Application status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}
		}
	}

	if createdAutoscaler {
		// Autoscalers created successfully
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	if updatedAutoscaler {
		// Now, that we updated the autoscalers, we want to requeue the reconciliation
		// so that we can ensure that we have the latest state of the resource before
		// update. Also, it will help ensure the desired state on the cluster
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, nil
}

func (r *ApplicationReconciler) ReconcileServices(ctx context.Context, req ctrl.Request, application *vernaldevv1alpha1.Application) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	serviceNames := make(map[string]struct{})
	serviceExists := struct{}{}

	createdServices := false
	updatedServices := false
	for _, component := range application.Spec.Components {
		service, err := r.serviceForApplicationComponent(application, &component)
		if err != nil {
			log.Error(err, "Failed to define Service resource for Application component")

			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Failed to create new Service resource for Application component %s: %s", component.Name, err),
				},
			)

			if err := r.Status().Update(ctx, application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		serviceNames[service.Name] = serviceExists

		found := v1.Service{}
		err = r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, &found)
		if err != nil && apierrors.IsNotFound(err) {
			log.Info("Creating new Service", "serviceName", service.Name, "serviceNamespace", service.Namespace)

			if err := r.Create(ctx, service); err != nil {
				log.Error(err, "Failed to create new Service", "serviceName", service.Name, "serviceNamespace", service.Namespace)
				return ctrl.Result{}, err
			}

			// Service created successfully
			createdServices = true
			continue
		} else if err != nil {
			log.Error(err, "Failed to get Service for Application component")
			// Let's return the error for the reconciliation be re-trigged again
			return ctrl.Result{}, err
		}

		if err := r.Update(ctx, service, client.DryRunAll); err != nil {
			log.Error(err, "Failed to perform client dry-run of desired service state for Application component")

			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Failed to perform client dry-run of desired service state for Application component %s: %s", component.Name, err),
				},
			)

			if err := r.Status().Update(ctx, application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		if reflect.DeepEqual(found.Spec, service.Spec) {
			// Service has not changed; skip update
			continue
		}

		log.Info("Updating Service", "serviceName", service.Name, "serviceNamespace", service.Namespace)

		updatedServices = true
		if err := r.Update(ctx, service); err != nil {
			log.Error(err, "Failed to apply desired service state for Application component")

			// Let's re-fetch the Application Custom Resource after updating the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raising the error "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			// if we try to update it again in the following operations
			if err := r.Get(ctx, req.NamespacedName, application); err != nil {
				log.Error(err, "Failed to re-fetch Application")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Failed to apply desired service state for Application component %s: %s", component.Name, err),
				},
			)

			if err := r.Status().Update(ctx, application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}
	}

	serviceList := v1.ServiceList{}
	namespaceName := fmt.Sprintf("vernal-%s-%s", application.Spec.Owner, application.Name)

	if err := r.List(
		ctx,
		&serviceList,
		client.InNamespace(namespaceName),
		client.MatchingLabels{
			"app.kubernetes.io/part-of":    application.Name,
			"app.kubernetes.io/managed-by": "vernal-operator",
		},
	); err != nil {
		log.Error(err, "Failed to list Service resources for Application")

		meta.SetStatusCondition(
			&application.Status.Conditions,
			metav1.Condition{
				Type:    applicationStatusTypeAvailable,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Failed to list Service resources for Application: %s", err),
			},
		)

		if err := r.Status().Update(ctx, application); err != nil {
			log.Error(err, "Failed to update Application status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, err
	}

	for _, service := range serviceList.Items {
		if _, ok := serviceNames[service.Name]; !ok {
			log.Info("Deleting old Service", "serviceName", service.Name, "serviceNamespace", service.Namespace)

			if err := r.Delete(ctx, &service); err != nil {
				log.Error(err, "Failed to delete Service resource for Application", "serviceName", service.Name, "serviceNamespace", service.Namespace)

				meta.SetStatusCondition(
					&application.Status.Conditions,
					metav1.Condition{
						Type:    applicationStatusTypeAvailable,
						Status:  metav1.ConditionFalse,
						Reason:  "Reconciling",
						Message: fmt.Sprintf("Failed to delete Service resource for Application: %s", err),
					},
				)

				if err := r.Status().Update(ctx, application); err != nil {
					log.Error(err, "Failed to update Application status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}
		}
	}

	if createdServices {
		// Services created successfully
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	if updatedServices {
		// Now, that we updated the services, we want to requeue the reconciliation
		// so that we can ensure that we have the latest state of the resource before
		// update. Also, it will help ensure the desired state on the cluster
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, nil
}

func (r *ApplicationReconciler) ReconcileHTTPRoutes(ctx context.Context, req ctrl.Request, application *vernaldevv1alpha1.Application) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	httprouteNames := make(map[string]struct{})
	httprouteExists := struct{}{}

	createdHTTPRoutes := false
	updatedHTTPRoutes := false
	for _, component := range application.Spec.Components {
		httproute, err := r.httprouteForApplicationComponent(application, &component)
		if err != nil {
			log.Error(err, "Failed to define HTTPRoute resource for Application component")

			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Failed to create new HTTPRoute resource for Application component %s: %s", component.Name, err),
				},
			)

			if err := r.Status().Update(ctx, application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		httprouteNames[httproute.Name] = httprouteExists

		found := gwv1.HTTPRoute{}
		err = r.Get(ctx, types.NamespacedName{Name: httproute.Name, Namespace: httproute.Namespace}, &found)
		if err != nil && apierrors.IsNotFound(err) {
			log.Info("Creating new HTTPRoute", "httprouteName", httproute.Name, "httprouteNamespace", httproute.Namespace)

			if err := r.Create(ctx, httproute); err != nil {
				log.Error(err, "Failed to create new HTTPRoute", "httprouteName", httproute.Name, "httprouteNamespace", httproute.Namespace)
				return ctrl.Result{}, err
			}

			// HTTPRoute created successfully
			createdHTTPRoutes = true
			continue
		} else if err != nil {
			log.Error(err, "Failed to get HTTPRoute for Application component")
			// Let's return the error for the reconciliation be re-trigged again
			return ctrl.Result{}, err
		}

		httproute.SetResourceVersion(found.GetResourceVersion())

		if err := r.Update(ctx, httproute, client.DryRunAll); err != nil {
			log.Error(err, "Failed to perform client dry-run of desired httproute state for Application component")

			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Failed to perform client dry-run of desired httproute state for Application component %s: %s", component.Name, err),
				},
			)

			if err := r.Status().Update(ctx, application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		if reflect.DeepEqual(found.Spec, httproute.Spec) {
			// HTTPRoute has not changed; skip update
			continue
		}

		log.Info("Updating HTTPRoute", "httprouteName", httproute.Name, "httprouteNamespace", httproute.Namespace)

		updatedHTTPRoutes = true
		if err := r.Update(ctx, httproute); err != nil {
			log.Error(err, "Failed to apply desired httproute state for Application component")

			// Let's re-fetch the Application Custom Resource after updating the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raising the error "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			// if we try to update it again in the following operations
			if err := r.Get(ctx, req.NamespacedName, application); err != nil {
				log.Error(err, "Failed to re-fetch Application")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(
				&application.Status.Conditions,
				metav1.Condition{
					Type:    applicationStatusTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Failed to apply desired httproute state for Application component %s: %s", component.Name, err),
				},
			)

			if err := r.Status().Update(ctx, application); err != nil {
				log.Error(err, "Failed to update Application status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}
	}

	httprouteList := gwv1.HTTPRouteList{}
	namespaceName := fmt.Sprintf("vernal-%s-%s", application.Spec.Owner, application.Name)

	if err := r.List(
		ctx,
		&httprouteList,
		client.InNamespace(namespaceName),
		client.MatchingLabels{
			"app.kubernetes.io/part-of":    application.Name,
			"app.kubernetes.io/managed-by": "vernal-operator",
		},
	); err != nil {
		log.Error(err, "Failed to list HTTPRoute resources for Application")

		meta.SetStatusCondition(
			&application.Status.Conditions,
			metav1.Condition{
				Type:    applicationStatusTypeAvailable,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Failed to list HTTPRoute resources for Application: %s", err),
			},
		)

		if err := r.Status().Update(ctx, application); err != nil {
			log.Error(err, "Failed to update Application status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, err
	}

	for _, httproute := range httprouteList.Items {
		if _, ok := httprouteNames[httproute.Name]; !ok {
			log.Info("Deleting old HTTPRoute", "httprouteName", httproute.Name, "httprouteNamespace", httproute.Namespace)

			if err := r.Delete(ctx, &httproute); err != nil {
				log.Error(err, "Failed to delete HTTPRoute resource for Application", "httprouteName", httproute.Name, "httprouteNamespace", httproute.Namespace)

				meta.SetStatusCondition(
					&application.Status.Conditions,
					metav1.Condition{
						Type:    applicationStatusTypeAvailable,
						Status:  metav1.ConditionFalse,
						Reason:  "Reconciling",
						Message: fmt.Sprintf("Failed to delete HTTPRoute resource for Application: %s", err),
					},
				)

				if err := r.Status().Update(ctx, application); err != nil {
					log.Error(err, "Failed to update Application status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}
		}
	}

	if createdHTTPRoutes {
		// HTTPRoutes created successfully
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	if updatedHTTPRoutes {
		// Now, that we updated the httproutes, we want to requeue the reconciliation
		// so that we can ensure that we have the latest state of the resource before
		// update. Also, it will help ensure the desired state on the cluster
		return ctrl.Result{Requeue: true}, nil
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

func (r *ApplicationReconciler) hpaForApplicationComponent(application *vernaldevv1alpha1.Application, component *vernaldevv1alpha1.ApplicationSpecComponent) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	namespaceName := fmt.Sprintf("vernal-%s-%s", application.Spec.Owner, application.GetName())
	deploymentName := fmt.Sprintf("vernal-%s-%s-%s", application.Spec.Owner, application.GetName(), component.Name)
	labels := labelsForApplicationNamespace(application.GetName(), namespaceName)
	var averageUtilization int32 = 50

	hpa := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: namespaceName,
			Labels:    labels,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
			MinReplicas: &component.MinReplicas,
			MaxReplicas: component.MaxReplicas,
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: "Resource",
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: "cpu",
						Target: autoscalingv2.MetricTarget{
							Type:               "Utilization",
							AverageUtilization: &averageUtilization,
						},
					},
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(application, &hpa, r.Scheme); err != nil {
		return nil, err
	}

	return &hpa, nil
}

func (r *ApplicationReconciler) postgresStandaloneForApplication(application *vernaldevv1alpha1.Application) (*cnpgv1.Cluster, error) {
	namespaceName := fmt.Sprintf("vernal-%s-%s", application.Spec.Owner, application.Name)

	postgres := cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "postgres",
			Namespace: namespaceName,
		},
		Spec: cnpgv1.ClusterSpec{
			Instances: 1,
			Bootstrap: &cnpgv1.BootstrapConfiguration{
				InitDB: &cnpgv1.BootstrapInitDB{
					Database: application.Name,
					Owner:    application.Name,
				},
			},
			StorageConfiguration: cnpgv1.StorageConfiguration{
				Size: "10Gi",
			},
		},
	}

	if err := ctrl.SetControllerReference(application, &postgres, r.Scheme); err != nil {
		return nil, err
	}

	return &postgres, nil
}

func (r *ApplicationReconciler) redisStandaloneForApplication(application *vernaldevv1alpha1.Application) (*redisv1beta2.Redis, error) {
	namespaceName := fmt.Sprintf("vernal-%s-%s", application.Spec.Owner, application.GetName())
	var securityInt int64 = 1000

	redisStandalone := redisv1beta2.Redis{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redis",
			Namespace: namespaceName,
		},
		Spec: redisv1beta2.RedisSpec{
			KubernetesConfig: redisv1beta2.KubernetesConfig{
				KubernetesConfig: rediscommon.KubernetesConfig{
					Image:           "quay.io/opstree/redis:v7.0.12",
					ImagePullPolicy: v1.PullIfNotPresent,
				},
			},
			Storage: &redisv1beta2.Storage{
				Storage: rediscommon.Storage{
					VolumeClaimTemplate: v1.PersistentVolumeClaim{
						Spec: v1.PersistentVolumeClaimSpec{
							AccessModes: []v1.PersistentVolumeAccessMode{
								"ReadWriteOnce",
							},
							Resources: v1.VolumeResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceStorage: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
			SecurityContext: &v1.SecurityContext{
				RunAsUser:  &securityInt,
				RunAsGroup: &securityInt,
			},
		},
	}

	if err := ctrl.SetControllerReference(application, &redisStandalone, r.Scheme); err != nil {
		return nil, err
	}

	return &redisStandalone, nil
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
			Labels:    labels,
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
					Ports:           []v1.ContainerPort{{ContainerPort: int32(component.ContainerPort)}},
					Env: []v1.EnvVar{
						{
							Name:  application.Spec.Redis.UrlEnvVar,
							Value: fmt.Sprintf("redis://redis.%s.svc.cluster.local:6379/0", namespaceName),
						},
						{
							Name:  application.Spec.Postgres.UrlEnvVar,
							Value: fmt.Sprintf("postgres://%s@postgres-rw.%s.svc.cluster.local:5432/%s", application.Name, namespaceName, application.Name),
						},
					},

					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("100m"),
							v1.ResourceMemory: resource.MustParse("256Mi"),
						},
						Limits: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("100m"),
							v1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},

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

func (r *ApplicationReconciler) serviceForApplicationComponent(application *vernaldevv1alpha1.Application, component *vernaldevv1alpha1.ApplicationSpecComponent) (*v1.Service, error) {
	namespaceName := fmt.Sprintf("vernal-%s-%s", application.Spec.Owner, application.GetName())
	serviceName := fmt.Sprintf("vernal-%s-%s-%s", application.Spec.Owner, application.GetName(), component.Name)
	labels := labelsForApplicationComponent(application.GetName(), component.Name, component.Image)

	service := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespaceName,
			Labels:    labels,
		},
		Spec: v1.ServiceSpec{
			Selector: labels,
			Type:     v1.ServiceTypeClusterIP,
			Ports: []v1.ServicePort{{
				Protocol:   v1.ProtocolTCP,
				Port:       80,
				TargetPort: intstr.FromInt(int(component.ContainerPort)),
			}},
		},
	}

	if err := ctrl.SetControllerReference(application, &service, r.Scheme); err != nil {
		return nil, err
	}

	return &service, nil
}

func (r *ApplicationReconciler) httprouteForApplicationComponent(application *vernaldevv1alpha1.Application, component *vernaldevv1alpha1.ApplicationSpecComponent) (*gwv1.HTTPRoute, error) {
	namespaceName := fmt.Sprintf("vernal-%s-%s", application.Spec.Owner, application.GetName())
	commonName := fmt.Sprintf("vernal-%s-%s-%s", application.Spec.Owner, application.GetName(), component.Name)
	labels := labelsForApplicationComponent(application.GetName(), component.Name, component.Image)

	parentRefName := gwv1.ObjectName("vernal")
	parentRefNamespace := gwv1.Namespace("istio-ingress")
	hostname := gwv1.Hostname(fmt.Sprintf("%s-%s-%s-app.local.lan.vernal.dev", component.Name, application.Name, application.Spec.Owner))
	matchPathType := gwv1.PathMatchPathPrefix
	matchPathValue := "/"
	backendRefPort := gwv1.PortNumber(80)

	httproute := gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      commonName,
			Namespace: namespaceName,
			Labels:    labels,
		},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{Name: parentRefName, Namespace: &parentRefNamespace}},
			},
			Hostnames: []gwv1.Hostname{hostname},
			Rules: []gwv1.HTTPRouteRule{{
				Matches: []gwv1.HTTPRouteMatch{{
					Path: &gwv1.HTTPPathMatch{
						Type:  &matchPathType,
						Value: &matchPathValue,
					},
				}},
				BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{
					Name: gwv1.ObjectName(commonName),
					Port: &backendRefPort,
				}}}},
			}},
		},
	}

	if err := ctrl.SetControllerReference(application, &httproute, r.Scheme); err != nil {
		return nil, err
	}

	return &httproute, nil
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
		Owns(&v1.Service{}).
		Owns(&gwv1.HTTPRoute{}).
		Complete(r)
}
