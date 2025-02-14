// Copyright 2020 The Kubernetes Authors.
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

package spoketoken

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	ocinfrav1 "github.com/openshift/api/config/v1"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"open-cluster-management.io/multicloud-operators-subscription/pkg/utils"
)

const (
	secretSuffix             = "-cluster-secret"
	requeuAfter              = 5
	infrastructureConfigName = "cluster"
)

// Add creates a new agent token controller and adds it to the Manager if standalone is false.
func Add(mgr manager.Manager, hubconfig *rest.Config, syncid *types.NamespacedName, standalone bool) error {
	if !standalone {
		hubclient, err := client.New(hubconfig, client.Options{})

		if err != nil {
			klog.Error("Failed to generate client to hub cluster with error:", err)
			return err
		}

		return add(mgr, newReconciler(mgr, hubclient, syncid, mgr.GetConfig().Host))
	}

	return nil
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, hubclient client.Client, syncid *types.NamespacedName, host string) reconcile.Reconciler {
	rec := &ReconcileAgentToken{
		Client:    mgr.GetClient(),
		scheme:    mgr.GetScheme(),
		hubclient: hubclient,
		syncid:    syncid,
		host:      host,
	}

	return rec
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	klog.Info("Adding klusterlet token controller.")
	// Create a new controller
	c, err := controller.New("klusterlet-token-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to klusterlet-addon-appmgr service account in open-cluster-management-agent-addon namespace.
	err = c.Watch(&source.Kind{Type: &corev1.ServiceAccount{}}, &handler.EnqueueRequestForObject{}, utils.ServiceAccountPredicateFunctions)
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileSubscription implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileAgentToken{}

// ReconcileAppMgrToken syncid is the namespaced name of this managed cluster.
// host is the API server URL of this managed cluster.
type ReconcileAgentToken struct {
	client.Client
	hubclient client.Client
	scheme    *runtime.Scheme
	syncid    *types.NamespacedName
	host      string
}

type Config struct {
	BearerToken     string          `json:"bearerToken"`
	TLSClientConfig map[string]bool `json:"tlsClientConfig"`
}

// Reconciles <clusterName>-cluster-secret secret in the managed cluster's namespace
// on the hub cluster to the klusterlet-addon-appmgr service account's token secret.
// If it is running on the hub, don't do anything.
func (r *ReconcileAgentToken) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	klog.Infof("Reconciling %s", request.NamespacedName)

	appmgrsa := &corev1.ServiceAccount{}

	err := r.Client.Get(context.TODO(), request.NamespacedName, appmgrsa)

	if err != nil {
		if kerrors.IsNotFound(err) {
			klog.Infof("%s is not found. Deleting the secret from the hub.", request.NamespacedName)

			err := r.hubclient.Delete(context.TODO(), r.prepareAgentTokenSecret(""))

			if err != nil {
				klog.Error("Failed to delete the secret from the hub.")
				return reconcile.Result{RequeueAfter: requeuAfter * time.Minute}, err
			}

			return reconcile.Result{}, nil
		}

		klog.Errorf("Failed to get serviceaccount %v, error: %v", request.NamespacedName, err)

		return reconcile.Result{RequeueAfter: requeuAfter * time.Minute}, err
	}

	// Get the service account token from the service account's secret list
	token := r.getServiceAccountTokenSecret()

	if token == "" {
		klog.Error("Failed to find the service account token.")
		return reconcile.Result{}, errors.New("failed to find the klusterlet agent addon service account token secret")
	}

	// Prepare the secret to be created/updated in the managed cluster namespace on the hub
	secret := r.prepareAgentTokenSecret(token)

	// Get the existing secret in the managed cluster namespace from the hub
	hubSecret := &corev1.Secret{}
	hubSecretName := types.NamespacedName{Namespace: r.syncid.Namespace, Name: r.syncid.Name + secretSuffix}
	err = r.hubclient.Get(context.TODO(), hubSecretName, hubSecret)

	if err != nil {
		if kerrors.IsNotFound(err) {
			klog.Info("Secret " + hubSecretName.String() + " not found on the hub.")

			err := r.hubclient.Create(context.TODO(), secret)

			if err != nil {
				klog.Error(err.Error())
				return reconcile.Result{RequeueAfter: requeuAfter * time.Minute}, err
			}

			klog.Info("The cluster secret " + secret.Name + " was created in " + secret.Namespace + " on the hub successfully.")
		} else {
			klog.Error("Failed to get secret from the hub: ", err)
			return reconcile.Result{RequeueAfter: requeuAfter * time.Minute}, err
		}
	} else {
		// Update
		err := r.hubclient.Update(context.TODO(), secret)

		if err != nil {
			klog.Error("Failed to update secret : ", err)
			return reconcile.Result{RequeueAfter: time.Duration(requeuAfter * time.Minute.Milliseconds())}, err
		}

		klog.Info("The cluster secret " + secret.Name + " was updated successfully in " + secret.Namespace + " on the hub.")
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileAgentToken) prepareAgentTokenSecret(token string) *corev1.Secret {
	mcSecret := &corev1.Secret{}
	mcSecret.Name = r.syncid.Name + secretSuffix
	mcSecret.Namespace = r.syncid.Namespace

	labels := make(map[string]string)
	labels["argocd.argoproj.io/secret-type"] = "cluster"
	labels["apps.open-cluster-management.io/secret-type"] = "acm-cluster"

	configData := &Config{}
	configData.BearerToken = token
	tlsClientConfig := make(map[string]bool)
	tlsClientConfig["insecure"] = true
	configData.TLSClientConfig = tlsClientConfig

	jsonConfigData, err := json.MarshalIndent(configData, "", "  ")

	if err != nil {
		klog.Error(err)
	}

	apiServerURL, err := r.getKubeAPIServerAddress()

	if err != nil {
		klog.Error(err)
	}

	data := make(map[string]string)
	data["name"] = r.syncid.Name
	data["server"] = apiServerURL
	data["config"] = string(jsonConfigData)

	mcSecret.StringData = data

	labels["apps.open-cluster-management.io/cluster-name"] = data["name"]

	u, err := url.Parse(data["server"])
	if err != nil {
		klog.Error(err)
	}

	truncatedServerURL := utils.ValidateK8sLabel(u.Hostname())

	if truncatedServerURL == "" {
		klog.Error("Invalid hostname in the API URL:", u)
	} else {
		labels["apps.open-cluster-management.io/cluster-server"] = truncatedServerURL
	}

	klog.Infof("managed cluster secret label: %v", labels)
	mcSecret.SetLabels(labels)

	return mcSecret
}

func (r *ReconcileAgentToken) getServiceAccountTokenSecret() string {
	// Grab application-manager service account
	sa := &corev1.ServiceAccount{}

	err := r.Client.Get(context.TODO(), types.NamespacedName{Name: "application-manager", Namespace: "open-cluster-management-agent-addon"}, sa)
	if err != nil {
		klog.Error(err.Error())
		return ""
	}

	// loop through secrets to find application-manager-dockercfg secret
	for _, secret := range sa.Secrets {
		if strings.HasPrefix(secret.Name, "application-manager-dockercfg") {
			klog.Info("found the application-manager-dockercfg secret " + secret.Name)

			// application-manager-token secret is owned by the dockercfg secret
			dockerSecret := &corev1.Secret{}

			err = r.Client.Get(context.TODO(), types.NamespacedName{Name: secret.Name, Namespace: "open-cluster-management-agent-addon"}, dockerSecret)
			if err != nil {
				klog.Error(err.Error())
				return ""
			}

			anno := dockerSecret.GetAnnotations()
			klog.Info("found the application-manager-token secret " + anno["openshift.io/token-secret.name"])

			return anno["openshift.io/token-secret.value"]
		}
	}

	return ""
}

// getKubeAPIServerAddress - Get the API server address from OpenShift kubernetes cluster. This does not work with other kubernetes.
func (r *ReconcileAgentToken) getKubeAPIServerAddress() (string, error) {
	infraConfig := &ocinfrav1.Infrastructure{}

	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: infrastructureConfigName}, infraConfig); err != nil {
		return "", err
	}

	return infraConfig.Status.APIServerURL, nil
}
