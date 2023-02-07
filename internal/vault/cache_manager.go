// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"fmt"
	"reflect"

	"github.com/hashicorp/vault/api"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	secretsv1alpha1 "github.com/hashicorp/vault-secrets-operator/api/v1alpha1"
	"github.com/hashicorp/vault-secrets-operator/internal/common"
)

type ClientCacheManager interface {
	GetClient(context.Context, ctrlclient.Client, ctrlclient.Object) (Client, error)
	RemoveObject(ctrlclient.Object) bool
}

var _ ClientCacheManager = (*clientCacheManager)(nil)

type clientCacheManager struct {
	clientCache ClientCache
	objKeyCache ObjectKeyCache
}

func (x *clientCacheManager) RemoveObject(obj ctrlclient.Object) bool {
	return x.objKeyCache.Remove(ctrlclient.ObjectKeyFromObject(obj))
}

// GetClient is meant to be called for all resources that require access to Vault.
// It will attempt to fetch a Client from the in-memory cache for the provided object. On a cache miss
// a new Client will be instantiated, and an attempt to login into Vault will be made.
// Upon successful instantiation/login the Client will be cached for future access.
func (x *clientCacheManager) GetClient(ctx context.Context, client ctrlclient.Client, obj ctrlclient.Object) (Client, error) {
	// Lock on cache key
	mu.Lock()
	defer mu.Unlock()
	logger := log.FromContext(ctx)

	// TODO(cache): replace with LRU cache
	objKey := ctrlclient.ObjectKeyFromObject(obj)
	cacheKey, err := GetClientCacheKeyFromObj(ctx, client, obj)
	if err != nil {
		logger.Error(err, "Failed to get cacheKey from obj", "obj", obj)
	}
	logger.Info("Got cacheKey from obj", "obj", objKey, "cacheKey", cacheKey)
	if cacheKey == "" {
		return nil, fmt.Errorf("client cache key cannot be empty")
	}

	if oldCacheKey, ok := x.objKeyCache.Get(objKey); ok {
		if oldCacheKey != cacheKey {
			x.clientCache.Remove(oldCacheKey)
		}
	}

	x.objKeyCache.Add(objKey, cacheKey)

	vClient, ok := x.clientCache.Get(cacheKey)
	if ok {
		ok, err := vClient.CheckExpiry(5)
		if err == nil && ok {
			if err := vClient.Renew(ctx); err == nil {
				logger.Info("Returning cached client from memory")
				return vClient, nil
			}
		}
		// fall through to NewClient()
	} else {
		objKey := clientCacheObjectKey(cacheKey)
		ccObj := &secretsv1alpha1.VaultClientCache{}
		if err := client.Get(ctx, objKey, ccObj); err == nil {
			if c, err := x.restoreClient(ctx, client, ccObj); err == nil {
				logger.Info("Restored cached client from storage", "objKey", objKey)
				return c, nil
			}
		}
		// fall through to NewClient()
	}

	c, err := NewClient(ctx, client, obj)
	if err != nil {
		return nil, err
	}
	if err := c.Login(ctx, client); err != nil {
		return nil, err
	}

	if _, err := x.cacheClient(ctx, client, c); err != nil {
		return nil, err
	}

	return c, nil
}

func (x *clientCacheManager) restoreClient(ctx context.Context, client ctrlclient.Client, obj *secretsv1alpha1.VaultClientCache) (Client, error) {
	logger := log.FromContext(ctx)

	s := &corev1.Secret{}
	objKey := types.NamespacedName{
		Namespace: obj.Namespace,
		Name:      obj.Status.CacheSecretRef,
	}
	if err := client.Get(ctx, objKey, s); err != nil {
		return nil, err
	}

	var secret *api.Secret
	if b, ok := s.Data["secret"]; ok {
		transitRef := s.Labels["vaultTransitRef"]
		if transitRef != "" {
			objKey := ctrlclient.ObjectKey{
				Namespace: obj.Namespace,
				Name:      transitRef,
			}

			decBytes, err := DecryptWithTransitFromObjKey(ctx, client, objKey, b)
			if err != nil {
				logger.Error(err, "Failed to decrypt cached client from transit", "objKey", objKey)
				return nil, err
			}

			logger.Info("Successfully decrypted cached client from transit", "objKey", objKey)
			b = decBytes
		}

		if err := json.Unmarshal(b, &secret); err != nil {
			logger.Error(err, "Failed to unmarshal Vault token Secret from cache",
				"objKey", objKey, "clientCacheObj", obj)
			return nil, err
		}

		logger.Info("Got Vault token Secret from cache",
			"objKey", objKey, "clientCacheObj", obj)
	}

	c, err := NewClient(ctx, client, obj)
	if err != nil {
		return nil, err
	}

	if err := c.Restore(ctx, secret, obj.Spec.CredentialProviderUID); err != nil {
		return nil, err
	}

	if _, err := x.cacheClient(ctx, client, c); err != nil {
		return nil, err
	}

	return c, nil
}

// CacheClient in the global in-memory cache, and create a corresponding
// VaultClientCache resource to handle Client Token renewal, and in-memory cache management.
func (x *clientCacheManager) cacheClient(ctx context.Context, client ctrlclient.Client, c Client) (string, error) {
	logger := log.FromContext(ctx)

	cacheKey, err := c.GetCacheKey()
	if err != nil {
		return "", err
	}

	authObj, err := c.GetVaultAuthObj()
	if err != nil {
		return "", err
	}

	connObj, err := c.GetVaultConnectionObj()
	if err != nil {
		return "", err
	}

	providerUID, err := c.GetProviderID()
	if err != nil {
		return "", err
	}

	objKey := clientCacheObjectKey(cacheKey)
	obj := &secretsv1alpha1.VaultClientCache{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      objKey.Name,
			Namespace: objKey.Namespace,
			// These labels are required for cache eviction done by either the VaultAuth
			// or VaultConnection controllers. They are used to find any referent VaultClientCache resources.
			// Those controllers will evict/delete a referent VaultClientCache on update or delete.
			Labels: map[string]string{
				"vaultAuthRef":                authObj.Name,
				"vaultAuthRefNamespace":       authObj.Namespace,
				"vaultConnectionRef":          connObj.Name,
				"vaultConnectionRefNamespace": connObj.Namespace,
			},
		},
		Spec: secretsv1alpha1.VaultClientCacheSpec{
			CacheKey:                  cacheKey,
			VaultAuthName:             authObj.Name,
			VaultAuthNamespace:        authObj.Namespace,
			VaultAuthMethod:           authObj.Spec.Method,
			VaultAuthUID:              authObj.UID,
			VaultAuthGeneration:       authObj.Generation,
			VaultConnectionUID:        connObj.UID,
			VaultConnectionGeneration: connObj.Generation,
			CredentialProviderUID:     providerUID,
			VaultTransitRef:           authObj.Spec.VaultTransitRef,
		},
	}

	action := "created"
	if err := client.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			action = "patched"
			cur := &secretsv1alpha1.VaultClientCache{}
			if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(obj), cur); err != nil {
				return "", err
			}

			patch := ctrlclient.MergeFrom(cur.DeepCopy())
			cur.Spec = obj.Spec
			cur.ObjectMeta.OwnerReferences = obj.ObjectMeta.OwnerReferences
			cur.ObjectMeta.Labels = obj.ObjectMeta.Labels
			if err := client.Patch(ctx, cur, patch); err != nil {
				return "", err
			}
		} else {
			return "", err
		}
	}

	clientSize := reflect.TypeOf(c).Size()
	logger.Info("Handled VaultClientCache",
		"action", action, "objKey", objKey, "cacheKey", cacheKey, "clientSize", clientSize)
	x.clientCache.Add(cacheKey, c)
	return cacheKey, nil
}

func NewClientCacheManager(clientCache ClientCache, objKeyCache ObjectKeyCache) (ClientCacheManager, error) {
	return &clientCacheManager{
		clientCache: clientCache,
		objKeyCache: objKeyCache,
	}, nil
}

func clientCacheObjectKey(cacheKey string) ctrlclient.ObjectKey {
	return ctrlclient.ObjectKey{
		Namespace: common.OperatorNamespace,
		Name:      "vso-client-cache-" + cacheKey,
	}
}