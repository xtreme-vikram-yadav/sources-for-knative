/*
Copyright 2020 VMware, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package vspheresource

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	corev1Listers "k8s.io/client-go/listers/core/v1"
	rbacv1listers "k8s.io/client-go/listers/rbac/v1"
	eventingclientset "knative.dev/eventing/pkg/client/clientset/versioned"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/metrics"
	"knative.dev/pkg/reconciler"
	"knative.dev/pkg/resolver"

	sourcesv1alpha1 "github.com/vmware-tanzu/sources-for-knative/pkg/apis/sources/v1alpha1"
	clientset "github.com/vmware-tanzu/sources-for-knative/pkg/client/clientset/versioned"
	vspherereconciler "github.com/vmware-tanzu/sources-for-knative/pkg/client/injection/reconciler/sources/v1alpha1/vspheresource"
	v1alpha1lister "github.com/vmware-tanzu/sources-for-knative/pkg/client/listers/sources/v1alpha1"
	"github.com/vmware-tanzu/sources-for-knative/pkg/reconciler/vspheresource/resources"
	resourcenames "github.com/vmware-tanzu/sources-for-knative/pkg/reconciler/vspheresource/resources/names"
)

const (
	component = "vspheresource"
)

// Reconciler implements vspherereconciler.Interface for VSphereSource
// resources.
type Reconciler struct {
	resolver *resolver.URIResolver

	kubeclient     kubernetes.Interface
	eventingclient eventingclientset.Interface
	client         clientset.Interface

	deploymentLister     appsv1listers.DeploymentLister
	vspherebindingLister v1alpha1lister.VSphereBindingLister
	rbacLister           rbacv1listers.RoleBindingLister
	cmLister             corev1Listers.ConfigMapLister
	saLister             corev1Listers.ServiceAccountLister

	loggingContext context.Context
	adapterImage   string
	loggingConfig  *logging.Config
	metricsConfig  *metrics.ExporterOptions
}

// Check that our Reconciler implements Interface
var _ vspherereconciler.Interface = (*Reconciler)(nil)

// ReconcileKind implements Interface.ReconcileKind.
func (r *Reconciler) ReconcileKind(ctx context.Context, vms *sourcesv1alpha1.VSphereSource) reconciler.Event {
	if err := r.reconcileVSphereBinding(ctx, vms); err != nil {
		return err
	}

	// Make sure the ConfigMap for storing state exists before we
	// create the deployment so that it gets created as owned
	// by the source and hence won't be leaked.
	if err := r.reconcileConfigMap(ctx, vms); err != nil {
		return err
	}
	if err := r.reconcileServiceAccount(ctx, vms); err != nil {
		return err
	}
	if err := r.reconcileRoleBinding(ctx, vms); err != nil {
		return err
	}

	uri, err := r.resolver.URIFromDestinationV1(ctx, vms.Spec.Sink, vms)
	if err != nil {
		return err
	}
	vms.Status.SinkURI = uri

	if err = r.reconcileDeployment(ctx, vms); err != nil {
		return err
	}
	logging.FromContext(ctx).Infof("Reconciled vspheresource %q", vms.Name)

	return nil
}

func (r *Reconciler) reconcileVSphereBinding(ctx context.Context, vms *sourcesv1alpha1.VSphereSource) error {
	ns := vms.Namespace
	vspherebindingName := resourcenames.VSphereBinding(vms)

	vspherebinding, err := r.vspherebindingLister.VSphereBindings(ns).Get(vspherebindingName)
	if apierrs.IsNotFound(err) {
		vspherebinding = resources.MakeVSphereBinding(ctx, vms)
		vspherebinding, err = r.client.SourcesV1alpha1().VSphereBindings(ns).Create(ctx, vspherebinding, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create vspherebinding %q: %w", vspherebindingName, err)
		}
		logging.FromContext(ctx).Infof("Created vspherebinding %q", vspherebindingName)
	} else if err != nil {
		return fmt.Errorf("failed to get vspherebinding %q: %w", vspherebindingName, err)
	} else {
		// The vspherebinding exists, but make sure that it has the shape that we expect.
		desiredVSphereBinding := resources.MakeVSphereBinding(ctx, vms)
		vspherebinding = vspherebinding.DeepCopy()
		vspherebinding.Spec = desiredVSphereBinding.Spec
		vspherebinding, err = r.client.SourcesV1alpha1().VSphereBindings(ns).Update(ctx, vspherebinding, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create vspherebinding %q: %w", vspherebindingName, err)
		}
	}

	// Reflect the state of the VSphereBinding in the VSphereSource
	vms.Status.PropagateAuthStatus(vspherebinding.Status.Status)

	return nil
}

func (r *Reconciler) reconcileConfigMap(ctx context.Context, vms *sourcesv1alpha1.VSphereSource) error {
	ns := vms.Namespace
	name := resourcenames.ConfigMap(vms)

	_, err := r.cmLister.ConfigMaps(ns).Get(name)
	// Note that we only create the configmap if it does not exist so that we get the
	// OwnerRefs set up properly so it gets Garbage Collected.
	if apierrs.IsNotFound(err) {
		cm := resources.MakeConfigMap(ctx, vms)
		_, err := r.kubeclient.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create configmap %q: %w", name, err)
		}
		logging.FromContext(ctx).Infof("Created configmap %q", name)
	} else if err != nil {
		return fmt.Errorf("failed to get configmap %q: %w", name, err)
	}

	return nil
}

func (r *Reconciler) reconcileServiceAccount(ctx context.Context, vms *sourcesv1alpha1.VSphereSource) error {
	ns := vms.Namespace
	name := resourcenames.ServiceAccount(vms)

	_, err := r.saLister.ServiceAccounts(ns).Get(name)
	if apierrs.IsNotFound(err) {
		sa := resources.MakeServiceAccount(ctx, vms)
		_, err := r.kubeclient.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create serviceaccount %q: %w", name, err)
		}
		logging.FromContext(ctx).Infof("Created serviceaccount %q", name)
	} else if err != nil {
		return fmt.Errorf("failed to get serviceaccount %q: %w", name, err)
	}

	return nil
}

func (r *Reconciler) reconcileRoleBinding(ctx context.Context, vms *sourcesv1alpha1.VSphereSource) error {
	ns := vms.Namespace
	name := resourcenames.RoleBinding(vms)
	_, err := r.rbacLister.RoleBindings(ns).Get(name)
	if apierrs.IsNotFound(err) {
		roleBinding := resources.MakeRoleBinding(ctx, vms)
		_, err := r.kubeclient.RbacV1().RoleBindings(ns).Create(ctx, roleBinding, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create rolebinding %q: %w", name, err)
		}
		logging.FromContext(ctx).Infof("Created rolebinding %q", name)
	} else if err != nil {
		return fmt.Errorf("failed to get rolebinding %q: %w", name, err)
	}
	// TODO: diff the roleref / subjects and update as necessary.
	return nil
}

func (r *Reconciler) reconcileDeployment(ctx context.Context, vms *sourcesv1alpha1.VSphereSource) error {
	ns := vms.Namespace
	deploymentName := resourcenames.Deployment(vms)

	loggingConfig, err := logging.ConfigToJSON(r.loggingConfig)
	if err != nil {
		return fmt.Errorf("marshal logging config to JSON: %w", err)
	}

	metricsConfig, err := metrics.OptionsToJSON(r.metricsConfig)
	if err != nil {
		return fmt.Errorf("marshal metrics config to JSON: %w", err)
	}

	args := resources.AdapterArgs{
		Image:         r.adapterImage,
		LoggingConfig: loggingConfig,
		MetricsConfig: metricsConfig,
	}

	deployment, err := r.deploymentLister.Deployments(ns).Get(deploymentName)
	if apierrs.IsNotFound(err) {
		deployment, err = resources.MakeDeployment(ctx, vms, args)
		if err != nil {
			return fmt.Errorf("failed to create deployment %q: %w", deploymentName, err)
		}

		deployment, err = r.kubeclient.AppsV1().Deployments(ns).Create(ctx, deployment, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create deployment %q: %w", deploymentName, err)
		}
		logging.FromContext(ctx).Infof("Created deployment %q", deploymentName)
	} else if err != nil {
		return fmt.Errorf("failed to get deployment %q: %w", deploymentName, err)
	} else {
		// The deployment exists, but make sure that it has the shape that we expect.
		desiredDeployment, err := resources.MakeDeployment(ctx, vms, args)
		if err != nil {
			return fmt.Errorf("failed to create deployment %q: %w", deploymentName, err)
		}

		deployment = deployment.DeepCopy()
		deployment.Spec = desiredDeployment.Spec
		deployment, err = r.kubeclient.AppsV1().Deployments(ns).Update(ctx, deployment, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create deployment %q: %w", deploymentName, err)
		}
		logging.FromContext(ctx).Infof("Updated deployment %q", deploymentName)
	}

	// Reflect the state of the Adapter Deployment in the VSphereSource
	vms.Status.PropagateAdapterStatus(deployment.Status)

	return nil
}

func (r *Reconciler) UpdateFromLoggingConfigMap(cfg *corev1.ConfigMap) {
	if cfg != nil {
		delete(cfg.Data, "_example")
	}

	logcfg, err := logging.NewConfigFromConfigMap(cfg)
	if err != nil {
		logging.FromContext(r.loggingContext).Warn("failed to create logging config from configmap", zap.String("cfg.Name", cfg.Name))
		return
	}

	r.loggingConfig = logcfg
	logging.FromContext(r.loggingContext).Info("update from logging ConfigMap", zap.Any("ConfigMap", cfg))
}

func (r *Reconciler) UpdateFromMetricsConfigMap(cfg *corev1.ConfigMap) {
	if cfg != nil {
		delete(cfg.Data, "_example")
	}

	r.metricsConfig = &metrics.ExporterOptions{
		Domain:    metrics.Domain(),
		Component: component,
		ConfigMap: cfg.Data,
	}
	logging.FromContext(r.loggingContext).Info("update from metrics ConfigMap", zap.Any("ConfigMap", cfg))
}
