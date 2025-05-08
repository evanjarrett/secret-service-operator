/*
Copyright 2025.

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
	"net"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/evanjarrett/secret-service-operator/api/v1"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretServiceReconciler reconciles a SecretService object
type SecretServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apps.j5t.io,resources=secretservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.j5t.io,resources=secretservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.j5t.io,resources=secretservices/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SecretService object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile
func (r *SecretServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get the SecretService object
	instance := &appsv1.SecretService{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil // Don't requeue if the object is gone
		}
		return ctrl.Result{}, err
	}

	// Get the Secret
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: instance.Spec.SecretName, Namespace: instance.Namespace}, secret); err != nil {
		if errors.IsNotFound(err) {
			// If the secret doesn't exist, requeue after a delay
			logger.Error(err, "Secret not found", "secretName", instance.Spec.SecretName)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	for serviceName, value := range secret.Data {

		endpointPort := string(value)
		parts := strings.Split(endpointPort, ":")
		if len(parts) != 2 {
			// Log a warning or skip if it doesn't match the expected format, but don't return an error
			logger.Info("Skipping secret key with invalid format:", "key", serviceName, "value", endpointPort)
			continue // Skip this key and process the next one
		}

		endpointIP := parts[0]
		portStr := parts[1]

		// Validate IP address format
		if ip := net.ParseIP(endpointIP); ip == nil {
			logger.Info("Skipping secret key with invalid IP address:", "key", serviceName, "ip", endpointIP)
			continue
		}

		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			logger.Info("Skipping secret key with invalid port:", "key", serviceName, "port", portStr)
			continue
		}

		// Create or update the Service (Example: ClusterIP service)
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: instance.Namespace,
			},
			Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeClusterIP,
				Ports: []corev1.ServicePort{
					{
						Name:       "http",
						Port:       int32(port),
						TargetPort: intstr.FromInt(port), // Or string target port if needed
						Protocol:   corev1.ProtocolTCP,
					},
				},
			},
		}

		if err := r.CreateOrUpdate(ctx, service, instance); err != nil {
			return ctrl.Result{}, err
		}

		httpName := "http"
		tcpProtocol := corev1.ProtocolTCP
		readyTrue := true
		portInt32 := int32(port)

		// Create or update EndpointSlice (modern replacement for Endpoints)
		endpointSlice := &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName + "-" + uuid.NewString()[:8], // EndpointSlices need unique names
				Namespace: instance.Namespace,
				Labels: map[string]string{
					discoveryv1.LabelServiceName: serviceName, // Required label to associate with Service
				},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{
				{
					Addresses: []string{endpointIP},
					Conditions: discoveryv1.EndpointConditions{
						Ready: &readyTrue,
					},
				},
			},
			Ports: []discoveryv1.EndpointPort{
				{
					Name:     &httpName,
					Port:     &portInt32,
					Protocol: &tcpProtocol,
				},
			},
		}

		if err := r.CreateOrUpdate(ctx, endpointSlice, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// CreateOrUpdate helper function
func (r *SecretServiceReconciler) CreateOrUpdate(ctx context.Context, obj client.Object, owner metav1.Object) error {
	logger := log.FromContext(ctx)
	key := client.ObjectKeyFromObject(obj)

	// 1. Attempt to get the object.  This checks if it already exists.
	if err := r.Client.Get(ctx, key, obj); err != nil {
		// 2. If the object is not found, create it.
		if !errors.IsNotFound(err) {
			return err // Return any error other than NotFound
		}

		// Use Controllerutil to manage the obj
		if err := controllerutil.SetControllerReference(owner, obj, r.Scheme); err != nil {
			return err
		}

		logger.Info("Creating resource", "object", key)
		return r.Client.Create(ctx, obj)
	}

	// 3. If the object exists, update it.
	existing := obj.DeepCopyObject().(client.Object) // Deep copy to avoid modifying the cache

	// Use Patch to apply only the changes, improving efficiency
	if err := r.Client.Patch(ctx, obj, client.MergeFrom(existing)); err != nil {
		return err
	}

	logger.Info("Updated resource", "object", key)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SecretServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.SecretService{}).
		Complete(r)
}
