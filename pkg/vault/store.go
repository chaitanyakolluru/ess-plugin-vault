/*
 Copyright 2023 The Crossplane Authors.

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

package kv

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"path/filepath"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/api/auth/approle"
	"github.com/hashicorp/vault/api/auth/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/connection/store"
	"github.com/crossplane/crossplane-runtime/pkg/connection/store/vault/kv"
	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/crossplane-contrib/ess-plugin-vault/apis/config/v1alpha1"
)

// Error strings.
const (
	errNoConfig                  = "no Vault config provided"
	errNewClient                 = "cannot create new client"
	errExtractCABundle           = "cannot extract ca bundle"
	errAppendCABundle            = "cannot append ca bundle"
	errExtractToken              = "cannot extract token"
	errExtractAppRoleCredentials = "cannot extract approle credentials"
	errSetupKubernetesAuth       = "cannot setup kubernetes auth"
	errSetupAppRoleAuth          = "cannot setup approle auth"
	errLoginKubernetesAuth       = "cannot logging in with kubernetes auth"
	errLoginAppRoleAuth          = "cannot login with approle auth"
	errNoTokenProvided           = "token auth configured but no token provided"
	errNoRoleProvided            = "kubernetes auth configured but no role provided"
	errNoAppRoleProvided         = "app role auth configured but no approle provided"

	errGet    = "cannot get secret"
	errApply  = "cannot apply secret"
	errDelete = "cannot delete secret"
)

// KVClient is a Vault AdditiveKVClient Secrets engine client that supports both v1 and v2.
type KVClient interface {
	Get(path string, secret *kv.Secret) error
	Apply(path string, secret *kv.Secret, ao ...kv.ApplyOption) error
	Delete(path string) error
}

// SecretStore is a Vault Secret Store.
type SecretStore struct {
	client KVClient
}

// appRoleAuthCredentials is a struct that holds the app role credentials.
type appRoleAuthCredentials struct {
	RoleID    string `json:"role_id"`
	SecretID  string `json:"secret_id"`
	MountPath string `json:"mountPath"`
}

// NewVaultStore returns a new Vault SecretStore.
func NewVaultStore(ctx context.Context, kube client.Client, cfg *v1alpha1.VaultConfig) (*SecretStore, error) { // nolint: gocyclo
	if cfg.Spec == nil {
		return nil, errors.New(errNoConfig)
	}
	vCfg := api.DefaultConfig()
	vCfg.Address = cfg.Spec.Server

	if cfg.Spec.CABundle != nil {
		ca, err := resource.CommonCredentialExtractor(ctx, cfg.Spec.CABundle.Source, kube, cfg.Spec.CABundle.CommonCredentialSelectors)
		if err != nil {
			return nil, errors.Wrap(err, errExtractCABundle)
		}
		pool := x509.NewCertPool()
		if ok := pool.AppendCertsFromPEM(ca); !ok {
			return nil, errors.Wrap(err, errAppendCABundle)
		}
		vCfg.HttpClient.Transport.(*http.Transport).TLSClientConfig.RootCAs = pool
	}

	c, err := api.NewClient(vCfg)
	if err != nil {
		return nil, errors.Wrap(err, errNewClient)
	}

	if cfg.Spec.Namespace != nil {
		c.SetNamespace(*cfg.Spec.Namespace)
	}

	switch cfg.Spec.Auth.Method {
	case v1alpha1.VaultAuthToken:
		if cfg.Spec.Auth.Token == nil {
			return nil, errors.New(errNoTokenProvided)
		}
		t, err := resource.CommonCredentialExtractor(ctx, cfg.Spec.Auth.Token.Source, kube, cfg.Spec.Auth.Token.CommonCredentialSelectors)
		if err != nil {
			return nil, errors.Wrap(err, errExtractToken)
		}
		c.SetToken(string(t))
	case v1alpha1.VaultAuthKubernetes:
		if cfg.Spec.Auth.Kubernetes == nil {
			return nil, errors.New(errNoRoleProvided)
		}

		var loginOpts []kubernetes.LoginOption
		if cfg.Spec.Auth.Kubernetes.MountPath != "" {
			loginOpts = append(loginOpts, kubernetes.WithMountPath(cfg.Spec.Auth.Kubernetes.MountPath))
		}

		if cfg.Spec.Auth.Kubernetes.ServiceAccountTokenSource != nil {
			t, err := resource.CommonCredentialExtractor(ctx, cfg.Spec.Auth.Kubernetes.ServiceAccountTokenSource.Source, kube, cfg.Spec.Auth.Kubernetes.ServiceAccountTokenSource.CommonCredentialSelectors)
			if err != nil {
				return nil, errors.Wrap(err, errExtractToken)
			}
			loginOpts = append(loginOpts, kubernetes.WithServiceAccountToken(string(t)))
		}

		auth, err := kubernetes.NewKubernetesAuth(cfg.Spec.Auth.Kubernetes.Role, loginOpts...)
		if err != nil {
			return nil, errors.Wrap(err, errSetupKubernetesAuth)
		}

		_, err = c.Auth().Login(ctx, auth)
		if err != nil {
			return nil, errors.Wrap(err, errLoginKubernetesAuth)
		}
	case v1alpha1.VaultAuthAppRole:
		if cfg.Spec.Auth.AppRole == nil {
			return nil, errors.New(errNoAppRoleProvided)
		}

		d, err := resource.CommonCredentialExtractor(ctx, cfg.Spec.Auth.AppRole.Source, kube, cfg.Spec.Auth.AppRole.CommonCredentialSelectors)
		if err != nil {
			return nil, errors.Wrap(err, errExtractAppRoleCredentials)
		}

		var cred appRoleAuthCredentials
		if err := json.Unmarshal(d, &cred); err != nil {
			return nil, errors.Wrap(err, errExtractAppRoleCredentials)
		}

		// default mount path is "approle", specify if different in the secret
		var loginOpts []approle.LoginOption
		if cred.MountPath != "" {
			loginOpts = append(loginOpts, approle.WithMountPath(cred.MountPath))
		}

		auth, err := approle.NewAppRoleAuth(cred.RoleID, &approle.SecretID{FromString: cred.SecretID}, loginOpts...)
		if err != nil {
			return nil, errors.Wrap(err, errSetupAppRoleAuth)
		}

		_, err = c.Auth().Login(ctx, auth)
		if err != nil {
			return nil, errors.Wrap(err, errLoginAppRoleAuth)
		}
	default:
		return nil, errors.Errorf("%q is not supported as an auth method", cfg.Spec.Auth.Method)
	}

	var kvClient KVClient
	switch *cfg.Spec.Version {
	case v1alpha1.VaultKVVersionV1:
		kvClient = kv.NewV1Client(c.Logical(), cfg.Spec.MountPath)
	case v1alpha1.VaultKVVersionV2:
		kvClient = kv.NewV2Client(c.Logical(), cfg.Spec.MountPath)
	}

	return &SecretStore{
		client: kvClient,
	}, nil
}

// ReadKeyValues reads and returns key value pairs for a given Vault Secret.
func (ss *SecretStore) ReadKeyValues(_ context.Context, n store.ScopedName, s *store.Secret) error {
	kvs := &kv.Secret{}
	if err := ss.client.Get(ss.path(n), kvs); resource.Ignore(kv.IsNotFound, err) != nil {
		return errors.Wrap(err, errGet)
	}

	s.ScopedName = n
	s.Data = keyValuesFromData(kvs.Data)
	if len(kvs.CustomMeta) > 0 {
		s.Metadata = &v1.ConnectionSecretMetadata{
			Labels: kvs.CustomMeta,
		}
	}
	return nil
}

// WriteKeyValues writes key value pairs to a given Vault Secret.
func (ss *SecretStore) WriteKeyValues(_ context.Context, s *store.Secret, wo ...store.WriteOption) (changed bool, err error) {
	ao := applyOptions(wo...)

	ao = append(ao, kv.AllowUpdateIf(func(current, desired *kv.Secret) bool {
		return !cmp.Equal(current, desired, cmpopts.EquateEmpty(), cmpopts.IgnoreUnexported(kv.Secret{}))
	}))

	err = ss.client.Apply(ss.path(s.ScopedName), kv.NewSecret(dataFromKeyValues(s.Data), s.GetLabels()), ao...)

	if resource.IsNotAllowed(err) {
		// The update was not allowed because it was a no-op.
		return false, nil
	}
	if err != nil {
		return false, errors.Wrap(err, errApply)
	}
	return true, nil
}

// DeleteKeyValues delete key value pairs from a given Vault Secret.
// If no kv specified, the whole secret instance is deleted.
// If kv specified, those would be deleted and secret instance will be deleted
// only if there is no Data left.
func (ss *SecretStore) DeleteKeyValues(_ context.Context, s *store.Secret, do ...store.DeleteOption) error {
	Secret := &kv.Secret{}
	err := ss.client.Get(ss.path(s.ScopedName), Secret)

	if kv.IsNotFound(err) {
		// Secret already deleted, nothing to do.
		return nil
	}
	if err != nil {
		return errors.Wrap(err, errGet)
	}

	for _, o := range do {
		if err = o(context.Background(), s); err != nil {
			return err
		}
	}

	for k := range s.Data {
		delete(Secret.Data, k)
	}
	if len(s.Data) == 0 || len(Secret.Data) == 0 {
		// Secret is deleted only if:
		// - No kv to delete specified as input
		// - No data left in the secret
		return errors.Wrap(ss.client.Delete(ss.path(s.ScopedName)), errDelete)
	}
	// If there are still keys left, update the secret with the remaining.
	return errors.Wrap(ss.client.Apply(ss.path(s.ScopedName), Secret), errApply)
}

func (ss *SecretStore) path(s store.ScopedName) string {
	return filepath.Join(s.Scope, s.Name)
}

func applyOptions(wo ...store.WriteOption) []kv.ApplyOption {
	ao := make([]kv.ApplyOption, len(wo))
	for i := range wo {
		o := wo[i]
		ao[i] = func(current, desired *kv.Secret) error {
			cs := &store.Secret{
				Metadata: &v1.ConnectionSecretMetadata{
					Labels: current.CustomMeta,
				},
				Data: keyValuesFromData(current.Data),
			}

			ds := &store.Secret{
				Metadata: &v1.ConnectionSecretMetadata{
					Labels: desired.CustomMeta,
				},
				Data: keyValuesFromData(desired.Data),
			}

			if err := o(context.Background(), cs, ds); err != nil {
				return err
			}
			desired.CustomMeta = ds.GetLabels()
			desired.Data = dataFromKeyValues(ds.Data)

			return nil
		}
	}
	return ao
}

func keyValuesFromData(data map[string]string) store.KeyValues {
	if len(data) == 0 {
		return nil
	}
	kv := make(store.KeyValues, len(data))
	for k, v := range data {
		kv[k] = []byte(v)
	}
	return kv
}

func dataFromKeyValues(kv store.KeyValues) map[string]string {
	if len(kv) == 0 {
		return nil
	}

	data := make(map[string]string, len(kv))
	for k, v := range kv {
		// NOTE(turkenh): vault stores values as strings. So we convert []byte
		// to string before writing to Vault.
		data[k] = string(v)

	}
	return data
}
