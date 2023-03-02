// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hashicorp/vault/api"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	secretsv1alpha1 "github.com/hashicorp/vault-secrets-operator/api/v1alpha1"
	"github.com/hashicorp/vault-secrets-operator/internal/consts"
	"github.com/hashicorp/vault-secrets-operator/internal/helpers"
	"github.com/hashicorp/vault-secrets-operator/internal/vault"
)

const (
	vaultDynamicSecretFinalizer = "vaultdynamicsecret.secrets.hashicorp.com/finalizer"
)

var notSecretOwnerError = fmt.Errorf("not the secret's owner")

// VaultDynamicSecretReconciler reconciles a VaultDynamicSecret object
type VaultDynamicSecretReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       record.EventRecorder
	ClientFactory  vault.ClientFactory
	runtimePodName string
}

//+kubebuilder:rbac:groups=secrets.hashicorp.com,resources=vaultdynamicsecrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=secrets.hashicorp.com,resources=vaultdynamicsecrets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=secrets.hashicorp.com,resources=vaultdynamicsecrets/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
// required for rollout-restart
//+kubebuilder:rbac:groups={"extensions","apps"},resources=deployments,verbs=get;list;watch;patch
//+kubebuilder:rbac:groups={"extensions","apps"},resources=statefulsets,verbs=get;list;watch;patch
//+kubebuilder:rbac:groups={"extensions","apps"},resources=daemonsets,verbs=get;list;watch;patch

func (r *VaultDynamicSecretReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if r.runtimePodName == "" {
		r.runtimePodName = os.Getenv("OPERATOR_POD_NAME")
	}

	o := &secretsv1alpha1.VaultDynamicSecret{}
	if err := r.Client.Get(ctx, req.NamespacedName, o); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		logger.Error(err, "error getting resource from k8s", "obj", o)
		return ctrl.Result{}, err
	}

	logger.Info("Handling request")
	if o.GetDeletionTimestamp() == nil {
		if err := r.addFinalizer(ctx, o); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		logger.Info("Got deletion timestamp", "obj", o)
		// status update will be taken care of in the call to handleFinalizer()
		return r.handleFinalizer(ctx, o)
	}

	var doRolloutRestart bool
	leaseID := o.Status.SecretLease.ID
	// logger.Info("Last secret lease", "secretLease", o.Status.SecretLease, "epoch", r.epoch)
	if leaseID != "" {
		if r.runtimePodName != "" && r.runtimePodName != o.Status.LastRuntimePodName {
			// don't take part in the thundering herd on start up,
			// and the lease is still within the renewal window.
			leaseDuration := time.Duration(o.Status.SecretLease.LeaseDuration) * time.Second
			ts := time.Unix(o.Status.LastRenewalTime, 0).Add(leaseDuration).Unix()
			now := time.Now().Unix()
			diff := ts - now
			if diff > 0 {
				horizon, err := computeHorizonWithJitter(time.Duration(diff) * time.Second)
				if err != nil {
					logger.Error(err, "Failed to compute the new horizon")
				} else {
					o.Status.LastRuntimePodName = r.runtimePodName
					if err := r.updateStatus(ctx, o); err != nil {
						return ctrl.Result{}, err
					}
					r.Recorder.Eventf(o, corev1.EventTypeNormal, consts.ReasonSecretLeaseRenewal,
						"Not in renewal window after transitioning to a new leader/pod, lease_id=%s, horizon=%s", leaseID, horizon)
					return ctrl.Result{RequeueAfter: horizon}, nil
				}
			}
		}

		vClient, err := r.ClientFactory.GetClient(ctx, r.Client, o)
		if err != nil {
			r.Recorder.Eventf(o, corev1.EventTypeWarning, consts.ReasonVaultClientConfigError,
				"Failed to get Vault client: %s, lease_id=%s", err, leaseID)
			return ctrl.Result{}, err
		}

		if secretLease, err := r.renewLease(ctx, vClient, o); err == nil {
			if !r.checkRenewableLease(secretLease, o) {
				return ctrl.Result{}, nil
			}

			if secretLease.ID != leaseID {
				// the new lease ID does not match, this should never happen.
				err := fmt.Errorf("lease ID changed after renewal, expected=%s, actual=%s", leaseID, secretLease.ID)
				r.Recorder.Eventf(o, corev1.EventTypeWarning, consts.ReasonSecretLeaseRenewal, err.Error())
				return ctrl.Result{}, err
			}

			o.Status.SecretLease = *secretLease
			o.Status.LastRenewalTime = time.Now().Unix()
			o.Status.LastRuntimePodName = r.runtimePodName
			if err := r.updateStatus(ctx, o); err != nil {
				return ctrl.Result{}, err
			}

			leaseDuration := time.Duration(secretLease.LeaseDuration) * time.Second
			horizon, _ := computeHorizonWithJitter(leaseDuration)
			r.Recorder.Eventf(o, corev1.EventTypeNormal, consts.ReasonSecretLeaseRenewal,
				"Renewed lease, lease_id=%s, horizon=%s", leaseID, horizon)

			return ctrl.Result{RequeueAfter: horizon}, nil
		} else {
			doRolloutRestart = true
			r.Recorder.Eventf(o, corev1.EventTypeWarning, consts.ReasonSecretLeaseRenewalError,
				"Could not renew lease, lease_id=%s, err=%s", leaseID, err)
		}
	}

	vClient, err := r.ClientFactory.GetClient(ctx, r.Client, o)
	if err != nil {
		r.Recorder.Eventf(o, corev1.EventTypeWarning, consts.ReasonVaultClientConfigError,
			"Failed to get Vault client: %s, lease_id=%s", err, leaseID)
		return ctrl.Result{}, err
	}

	s, err := r.getDestinationSecret(ctx, o)
	if err != nil {
		if errors.Is(err, notSecretOwnerError) {
			// requeue the sync until the problem has been resolved.
			horizon, _ := computeHorizonWithJitter(time.Second * 10)
			r.Recorder.Eventf(o, corev1.EventTypeWarning, consts.ReasonSecretSyncError,
				"Not the destination secret's owner, horizon=%s, err=%s", horizon, err)
			return ctrl.Result{RequeueAfter: horizon}, nil
		}
		return ctrl.Result{}, err
	}

	secretLease, err := r.writeCreds(ctx, vClient, o, s)
	if err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("Wrote credentials", "dest", client.ObjectKeyFromObject(s))

	o.Status.SecretLease = *secretLease
	o.Status.LastRenewalTime = time.Now().Unix()
	o.Status.LastRuntimePodName = r.runtimePodName
	if err := r.updateStatus(ctx, o); err != nil {
		return ctrl.Result{}, err
	}

	reason := consts.ReasonSecretSynced
	if doRolloutRestart {
		reason = consts.ReasonSecretRotated
		for _, target := range o.Spec.RolloutRestartTargets {
			if err := helpers.RolloutRestart(ctx, s, target, r.Client); err != nil {
				r.Recorder.Eventf(s, corev1.EventTypeWarning, "RolloutRestartFailed",
					"failed to execute rollout restarts for target %#v: %s", target, err)
			} else {
				r.Recorder.Eventf(s, corev1.EventTypeNormal, "RolloutRestartTriggered",
					"Rollout restart triggered for %s", target)
			}
		}
	}

	if !r.checkRenewableLease(secretLease, o) {
		return ctrl.Result{}, nil
	}

	leaseDuration := time.Duration(secretLease.LeaseDuration) * time.Second
	horizon, _ := computeHorizonWithJitter(leaseDuration)
	r.Recorder.Eventf(o, corev1.EventTypeNormal, reason,
		"Secret synced, lease_id=%s, horizon=%s", secretLease.ID, horizon)

	return ctrl.Result{RequeueAfter: horizon}, nil
}

func (r *VaultDynamicSecretReconciler) checkRenewableLease(resp *secretsv1alpha1.VaultSecretLease, o *secretsv1alpha1.VaultDynamicSecret) bool {
	if !resp.Renewable {
		r.Recorder.Eventf(o, corev1.EventTypeWarning, consts.ReasonSecretLeaseRenewal,
			"Lease is not renewable, info=%#v", resp)
		return false
	}
	return true
}

func (r *VaultDynamicSecretReconciler) getDestinationSecret(ctx context.Context, o *secretsv1alpha1.VaultDynamicSecret) (*corev1.Secret, error) {
	secretObjKey := types.NamespacedName{
		Namespace: o.Namespace,
		Name:      o.Spec.Dest,
	}

	s := &corev1.Secret{}
	if err := r.Client.Get(ctx, secretObjKey, s); err != nil {
		if apierrors.IsNotFound(err) && !o.Spec.Create {
			r.Recorder.Eventf(o, corev1.EventTypeWarning, consts.ReasonSecretSyncError,
				"Cannot sync secret, destination %s does not exist and secret creation is disabled.", secretObjKey)
			return nil, err
		}

		if !o.Spec.Create {
			return nil, err
		}
	}

	exists := s.Name != ""
	if exists && !o.Spec.Create {
		return s, nil
	}

	ownerRefs := []metav1.OwnerReference{
		{
			APIVersion: o.APIVersion,
			Kind:       o.Kind,
			Name:       o.Name,
			UID:        o.UID,
		},
	}
	if exists {
		if !helpers.ObjectIsOwnedBy(s, o) {
			return nil, fmt.Errorf("secret %s exists: %w", secretObjKey, notSecretOwnerError)
		}
		s.OwnerReferences = ownerRefs
		return s, nil
	}

	s = helpers.NewSecretWithOwnerRefs(secretObjKey, ownerRefs...)
	if err := r.Client.Create(ctx, s); err != nil {
		return nil, err
	}

	return s, nil
}

func (r *VaultDynamicSecretReconciler) writeCreds(ctx context.Context, vClient vault.Client,
	o *secretsv1alpha1.VaultDynamicSecret, s *corev1.Secret,
) (*secretsv1alpha1.VaultSecretLease, error) {
	path := fmt.Sprintf("%s/creds/%s", o.Spec.Mount, o.Spec.Role)
	resp, err := vClient.Read(ctx, path)
	if err != nil {
		return nil, err
	}

	if resp == nil {
		return nil, fmt.Errorf("nil response from vault for path %s", path)
	}

	data, err := vault.MarshalSecretData(resp)
	if err != nil {
		return nil, err
	}

	s.Data = data
	if err := r.Client.Update(ctx, s); err != nil {
		return nil, err
	}

	return r.getVaultSecretLease(resp), nil
}

func (r *VaultDynamicSecretReconciler) updateStatus(ctx context.Context, o *secretsv1alpha1.VaultDynamicSecret) error {
	if err := r.Status().Update(ctx, o); err != nil {
		r.Recorder.Eventf(o, corev1.EventTypeWarning, consts.ReasonStatusUpdateError,
			"Failed to update the resource's status, err=%s", err)
	}
	return nil
}

func (r *VaultDynamicSecretReconciler) getVaultSecretLease(resp *api.Secret) *secretsv1alpha1.VaultSecretLease {
	return &secretsv1alpha1.VaultSecretLease{
		ID:            resp.LeaseID,
		LeaseDuration: resp.LeaseDuration,
		Renewable:     resp.Renewable,
		RequestID:     resp.RequestID,
	}
}

func (r *VaultDynamicSecretReconciler) renewLease(ctx context.Context, c vault.Client, o *secretsv1alpha1.VaultDynamicSecret) (*secretsv1alpha1.VaultSecretLease, error) {
	resp, err := c.Write(ctx, "/sys/leases/renew", map[string]interface{}{
		"lease_id":  o.Status.SecretLease.ID,
		"increment": o.Status.SecretLease.LeaseDuration,
	})
	if err != nil {
		return nil, err
	}

	return r.getVaultSecretLease(resp), nil
}

func (r *VaultDynamicSecretReconciler) handleFinalizer(ctx context.Context, o *secretsv1alpha1.VaultDynamicSecret) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(o, vaultDynamicSecretFinalizer) {
		controllerutil.RemoveFinalizer(o, vaultDynamicSecretFinalizer)
		r.ClientFactory.RemoveObject(o)
		if err := r.Update(ctx, o); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *VaultDynamicSecretReconciler) addFinalizer(ctx context.Context, o *secretsv1alpha1.VaultDynamicSecret) error {
	if !controllerutil.ContainsFinalizer(o, vaultDynamicSecretFinalizer) {
		controllerutil.AddFinalizer(o, vaultDynamicSecretFinalizer)
		if err := r.Client.Update(ctx, o); err != nil {
			return err
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VaultDynamicSecretReconciler) SetupWithManager(mgr ctrl.Manager, opts controller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&secretsv1alpha1.VaultDynamicSecret{}).
		WithOptions(opts).
		WithEventFilter(ignoreUpdatePredicate()).
		Complete(r)
}