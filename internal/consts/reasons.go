// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package consts

const (
	ReasonAccepted                  = "Accepted"
	ReasonVaultClientConfigError    = "VaultClientConfigError"
	ReasonVaultClientError          = "VaultClientError"
	ReasonVaultStaticSecret         = "VaultStaticSecretError"
	ReasonK8sClientError            = "K8sClientError"
	ReasonInvalidAuthConfiguration  = "InvalidAuthConfiguration"
	ReasonConnectionNotFound        = "ConnectionNotFound"
	ReasonInvalidConnection         = "InvalidVaultConnection"
	ReasonStatusUpdateError         = "StatusUpdateError"
	ReasonInvalidResourceRef        = "InvalidResourceRef"
	ReasonSecretLeaseRenewal        = "SecretLeaseRenewal"
	ReasonSecretLeaseRenewalError   = "SecretLeaseRenewalError"
	ReasonTokenLookupError          = "TokenLookupError"
	ReasonInvalidTokenTTL           = "InvalidTokenTTL"
	ReasonClientTokenRenewal        = "ClientTokenRenewal"
	ReasonClientTokenNotInCache     = "ClientTokenNotInCache"
	ReasonUnrecoverable             = "Unrecoverable"
	ReasonSecretSynced              = "SecretSynced"
	ReasonSecretRotated             = "SecretRotated"
	ReasonTransitError              = "TransitError"
	ReasonTransitEncryptError       = "TransitEncryptError"
	ReasonTransitEncryptSuccessful  = "TransitEncryptSuccessful"
	ReasonTransitDecryptError       = "TransitDecryptError"
	ReasonTransitDecryptSuccessful  = "TransitDecryptSuccessful"
	ReasonErrorGettingRef           = "ErrorGettingRef"
	ReasonMaxCacheMisses            = "MaxCacheMisses"
	ReasonInvalidCacheKey           = "InvalidCacheKey"
	ReasonVaultClientCacheEviction  = "VaultClientCacheEviction"
	ReasonVaultClientCacheCreation  = "VaultClientCacheCreation"
	ReasonVaultClientInstantiation  = "VaultClientCacheInstantiation"
	ReasonInvalidHorizon            = "InvalidHorizon"
	ReasonPersistentCacheCleanup    = "PersistentCacheCleanup"
	ReasonInvalidLeaseError         = "InvalidLeaseError"
	ReasonSecretSyncError           = "SecretSyncError"
	ReasonPersistenceForbidden      = "PersistenceForbidden"
	ReasonCacheRestorationFailed    = "CacheRestorationFailed"
	ReasonCacheRestorationSucceeded = "CacheRestorationSucceeded"
)
