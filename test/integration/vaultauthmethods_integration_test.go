// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/files"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	secretsv1alpha1 "github.com/hashicorp/vault-secrets-operator/api/v1alpha1"
	"github.com/hashicorp/vault-secrets-operator/internal/consts"
)

func TestVaultAuthMethods(t *testing.T) {
	testID := strings.ToLower(random.UniqueId())
	testK8sNamespace := "k8s-tenant-" + testID
	testKvv2MountPath := consts.KVSecretTypeV2 + testID
	testVaultNamespace := ""
	k8sConfigContext := "kind-" + clusterName
	appRoleMountPath := "approle"

	require.NotEmpty(t, clusterName, "KIND_CLUSTER_NAME is not set")
	operatorNS := os.Getenv("OPERATOR_NAMESPACE")
	require.NotEmpty(t, operatorNS, "OPERATOR_NAMESPACE is not set")

	// TF related setup
	tempDir, err := os.MkdirTemp(os.TempDir(), t.Name())
	require.Nil(t, err)
	tfDir, err := files.CopyTerraformFolderToDest(
		path.Join(testRoot, "vaultauthmethods/terraform"),
		tempDir,
		"terraform",
	)
	require.Nil(t, err)

	// Construct the terraform options with default retryable errors to handle the most common
	// retryable errors in terraform testing.
	terraformOptions := &terraform.Options{
		// Set the path to the Terraform code that will be tested.
		TerraformDir: tfDir,
		Vars: map[string]interface{}{
			"k8s_vault_connection_address": testVaultAddress,
			"k8s_test_namespace":           testK8sNamespace,
			"k8s_config_context":           k8sConfigContext,
			"vault_kvv2_mount_path":        testKvv2MountPath,
			"operator_helm_chart_path":     chartPath,
			"approle_mount_path":           appRoleMountPath,
		},
	}
	if operatorImageRepo != "" {
		terraformOptions.Vars["operator_image_repo"] = operatorImageRepo
	}
	if operatorImageTag != "" {
		terraformOptions.Vars["operator_image_tag"] = operatorImageTag
	}
	if entTests {
		testVaultNamespace = "vault-tenant-" + testID
		terraformOptions.Vars["vault_enterprise"] = true
		terraformOptions.Vars["vault_test_namespace"] = testVaultNamespace
	}
	terraformOptions = setCommonTFOptions(t, terraformOptions)

	ctx := context.Background()
	crdClient := getCRDClient(t)
	var created []ctrlclient.Object
	t.Cleanup(func() {
		for _, c := range created {
			// test that the custom resources can be deleted before tf destroy
			// removes the k8s namespace
			assert.Nil(t, crdClient.Delete(ctx, c))
		}
		exportKindLogs(t)
		// Clean up resources with "terraform destroy" at the end of the test.
		terraform.Destroy(t, terraformOptions)
		assert.NoError(t, os.RemoveAll(tempDir))
	})

	// Run "terraform init" and "terraform apply". Fail the test if there are any errors.
	terraform.InitAndApply(t, terraformOptions)

	// Parse terraform output
	b, err := json.Marshal(terraform.OutputAll(t, terraformOptions))
	require.Nil(t, err)

	var outputs authMethodsK8sOutputs
	require.Nil(t, json.Unmarshal(b, &outputs))

	// Set the secrets in vault to be synced to kubernetes
	vClient := getVaultClient(t, testVaultNamespace)

	// Create a jwt auth token secret
	secretName := "jwt-auth-secret"
	secretKey := "token"
	secretObj := createJWTTokenSecret(t, ctx, crdClient, testK8sNamespace, secretName, secretKey)
	created = append(created, secretObj)

	auths := []*secretsv1alpha1.VaultAuth{
		// Create a non-default VaultAuth CR
		{
			ObjectMeta: v1.ObjectMeta{
				Name:      "vaultauth-test-kubernetes",
				Namespace: testK8sNamespace,
			},
			Spec: secretsv1alpha1.VaultAuthSpec{
				Namespace: testVaultNamespace,
				Method:    "kubernetes",
				Mount:     "kubernetes",
				Kubernetes: &secretsv1alpha1.VaultAuthConfigKubernetes{
					Role:           outputs.AuthRole,
					ServiceAccount: "default",
					TokenAudiences: []string{"vault"},
				},
			},
		},
		{
			ObjectMeta: v1.ObjectMeta{
				Name:      "vaultauth-test-jwt-serviceaccount",
				Namespace: testK8sNamespace,
			},
			Spec: secretsv1alpha1.VaultAuthSpec{
				Namespace: testVaultNamespace,
				Method:    "jwt",
				Mount:     "jwt",
				JWT: &secretsv1alpha1.VaultAuthConfigJWT{
					Role:           outputs.AuthRole,
					ServiceAccount: "default",
					TokenAudiences: []string{"vault"},
				},
			},
		},
		{
			ObjectMeta: v1.ObjectMeta{
				Name:      "vaultauth-test-jwt-secret",
				Namespace: testK8sNamespace,
			},
			Spec: secretsv1alpha1.VaultAuthSpec{
				Namespace: testVaultNamespace,
				Method:    "jwt",
				Mount:     "jwt",
				JWT: &secretsv1alpha1.VaultAuthConfigJWT{
					Role: outputs.AuthRole,
					SecretKeyRef: &secretsv1alpha1.SecretKeySelector{
						Name: secretName,
						Key:  secretKey,
					},
				},
			},
		},
		{
			ObjectMeta: v1.ObjectMeta{
				Name:      "vaultauth-test-approle",
				Namespace: testK8sNamespace,
			},
			Spec: secretsv1alpha1.VaultAuthSpec{
				// No VaultConnectionRef - using the default.
				Namespace: testVaultNamespace,
				Method:    "approle",
				Mount:     appRoleMountPath,
				AppRole: &secretsv1alpha1.VaultAuthConfigAppRole{
					RoleID: outputs.AppRoleRoleID,
					SecretKeyRef: &secretsv1alpha1.SecretKeySelector{
						Name: "secretid",
						Key:  "id",
					},
				},
			},
		},
	}
	expectedData := map[string]interface{}{"foo": "bar"}

	// Apply all the Auth Methods
	for _, a := range auths {
		require.Nil(t, crdClient.Create(ctx, a))
		created = append(created, a)
	}
	secrets := []*secretsv1alpha1.VaultStaticSecret{}
	// create the VSS secrets
	for _, a := range auths {
		dest := fmt.Sprintf("kv-%s", a.Name)
		secretName := fmt.Sprintf("test-secret-%s", a.Name)
		secrets = append(secrets,
			&secretsv1alpha1.VaultStaticSecret{
				ObjectMeta: v1.ObjectMeta{
					Name:      secretName,
					Namespace: testK8sNamespace,
				},
				Spec: secretsv1alpha1.VaultStaticSecretSpec{
					VaultAuthRef: a.Name,
					Namespace:    testVaultNamespace,
					Mount:        testKvv2MountPath,
					Type:         consts.KVSecretTypeV2,
					Name:         dest,
					Destination: secretsv1alpha1.Destination{
						Name:   dest,
						Create: true,
					},
				},
			})
	}
	// Add to the created for cleanup
	for _, secret := range secrets {
		created = append(created, secret)
	}

	putKV := func(t *testing.T, vssObj *secretsv1alpha1.VaultStaticSecret) {
		_, err := vClient.KVv2(testKvv2MountPath).Put(ctx, vssObj.Spec.Name, expectedData)
		require.NoError(t, err)
	}

	deleteKV := func(t *testing.T, vssObj *secretsv1alpha1.VaultStaticSecret) {
		require.NoError(t, vClient.KVv2(testKvv2MountPath).Delete(ctx, vssObj.Spec.Name))
	}

	assertSync := func(t *testing.T, obj *secretsv1alpha1.VaultStaticSecret) {
		secret, err := waitForSecretData(t, ctx, crdClient, 30, 1*time.Second, obj.Spec.Destination.Name,
			obj.ObjectMeta.Namespace, expectedData)
		assert.NoError(t, err)
		assertSyncableSecret(t, obj,
			"secrets.hashicorp.com/v1alpha1",
			"VaultStaticSecret", secret)
	}

	for idx, tt := range auths {
		t.Run(tt.Spec.Method, func(t *testing.T) {
			// Create the KV secret in Vault.
			putKV(t, secrets[idx])
			// Create the VSS object referencing the object in Vault.
			require.Nil(t, crdClient.Create(ctx, secrets[idx]))
			// Assert that the Kube secret exists + has correct Data.
			assertSync(t, secrets[idx])
			t.Cleanup(func() {
				deleteKV(t, secrets[idx])
			})
		})
	}
}
