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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vernaldevv1alpha1 "vernaldev/vernal-operator/api/v1alpha1"
)

var _ = Describe("Application Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			applicationName          = "test-app"
			applicationOwner         = "vernal"
			applicationComponentName = "whoami"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name: applicationName,
		}
		application := &vernaldevv1alpha1.Application{}

		deploymentNamedspacedName := types.NamespacedName{
			Name:      fmt.Sprintf("vernal-%s-%s-%s", applicationOwner, applicationName, applicationComponentName),
			Namespace: fmt.Sprintf("vernal-%s-%s", applicationOwner, applicationName),
		}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Application")
			err := k8sClient.Get(ctx, typeNamespacedName, application)
			if err != nil && errors.IsNotFound(err) {
				resource := &vernaldevv1alpha1.Application{
					ObjectMeta: metav1.ObjectMeta{
						Name:      applicationName,
						Namespace: "default",
					},
					Spec: vernaldevv1alpha1.ApplicationSpec{
						Owner: applicationOwner,
						Repo: vernaldevv1alpha1.ApplicationSpecRepo{
							Url:      "https://github.com/vernaldev/test",
							Revision: "main",
							Path:     "vernal.yaml",
						},
						Components: []vernaldevv1alpha1.ApplicationSpecComponent{{
							Name:  applicationComponentName,
							Image: "traefik/whoami:latest",
							Port:  80,
						}},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &vernaldevv1alpha1.Application{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Application")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &ApplicationReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: &record.FakeRecorder{},
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking if Deployment was successfully created in the reconciliation")
			Eventually(func() error {
				found := &appsv1.Deployment{}
				return k8sClient.Get(ctx, deploymentNamedspacedName, found)
			}, time.Minute, time.Second).Should(Succeed())

			By("Checking the latest Status Condition added to the Application instance")
			Eventually(func() error {
				if application.Status.Conditions != nil && len(application.Status.Conditions) != 0 {
					latestStatusCondition := application.Status.Conditions[len(application.Status.Conditions)-1]
					expectedLatestStatusCondition := metav1.Condition{
						Type:    applicationStatusTypeAvailable,
						Status:  metav1.ConditionTrue,
						Reason:  "Reconciling",
						Message: "Successfully applied desired state for Application",
					}
					if latestStatusCondition != expectedLatestStatusCondition {
						return fmt.Errorf("The latest status condition added to the Application instance is not as expected")
					}
				}
				return nil
			}, time.Minute, time.Second).Should(Succeed())
		})
	})
})
