/*
Copyright The Ratify Authors.
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

package oras

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	paths "path/filepath"
	"sync"
	"time"

	oci "github.com/opencontainers/image-spec/specs-go/v1"
	ocitarget "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	ratifyconfig "github.com/deislabs/ratify/config"
	"github.com/deislabs/ratify/pkg/common"
	"github.com/deislabs/ratify/pkg/homedir"
	"github.com/deislabs/ratify/pkg/ocispecs"
	"github.com/deislabs/ratify/pkg/referrerstore"
	"github.com/deislabs/ratify/pkg/referrerstore/config"
	"github.com/deislabs/ratify/pkg/referrerstore/factory"
	"github.com/deislabs/ratify/pkg/referrerstore/oras/authprovider"
	_ "github.com/deislabs/ratify/pkg/referrerstore/oras/authprovider/aws"
	_ "github.com/deislabs/ratify/pkg/referrerstore/oras/authprovider/azure"
	"github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"
)

const (
	HttpMaxIdleConns        = 100
	HttpMaxConnsPerHost     = 100
	HttpMaxIdleConnsPerHost = 100
)

const (
	storeName             = "oras"
	defaultLocalCachePath = "local_oras_cache"
	dockerConfigFileName  = "config.json"
	ratifyUserAgent       = "ratify"
)

// OrasStoreConf describes the configuration of ORAS store
type OrasStoreConf struct {
	Name           string                          `json:"name"`
	UseHttp        bool                            `json:"useHttp,omitempty"`
	CosignEnabled  bool                            `json:"cosignEnabled,omitempty"`
	AuthProvider   authprovider.AuthProviderConfig `json:"authProvider,omitempty"`
	LocalCachePath string                          `json:"localCachePath,omitempty"`
}

type orasStoreFactory struct{}

type authCacheEntry struct {
	client    *remote.Repository
	expiresOn time.Time
}

type orasStore struct {
	config             *OrasStoreConf
	rawConfig          config.StoreConfig
	localCache         *ocitarget.Store
	authProvider       authprovider.AuthProvider
	authCache          sync.Map
	httpClient         *http.Client
	httpClientInsecure *http.Client
}

func init() {
	factory.Register(storeName, &orasStoreFactory{})
}

func (s *orasStoreFactory) Create(version string, storeConfig config.StorePluginConfig) (referrerstore.ReferrerStore, error) {
	conf := OrasStoreConf{}

	storeConfigBytes, err := json.Marshal(storeConfig)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(storeConfigBytes, &conf); err != nil {
		return nil, fmt.Errorf("failed to parse oras store configuration: %v", err)
	}

	authenticationProvider, err := authprovider.CreateAuthProviderFromConfig(conf.AuthProvider)
	if err != nil {
		return nil, fmt.Errorf("failed to create auth provider from configuration: %v", err)
	}

	// Set up the local cache where content will land when we pull
	if conf.LocalCachePath == "" {
		conf.LocalCachePath = paths.Join(homedir.Get(), ratifyconfig.ConfigFileDir, defaultLocalCachePath)
	}

	localRegistry, err := ocitarget.New(conf.LocalCachePath)
	if err != nil {
		return nil, fmt.Errorf("could not create local oras cache at path %s: %s", conf.LocalCachePath, err)
	}

	// define the http Transport for TLS enabled
	secureTransport := http.DefaultTransport.(*http.Transport).Clone()
	secureTransport.MaxIdleConns = HttpMaxIdleConns
	secureTransport.MaxConnsPerHost = HttpMaxConnsPerHost
	secureTransport.MaxIdleConnsPerHost = HttpMaxIdleConnsPerHost

	// define the http Transport for TLS disabled
	insecureTransport := http.DefaultTransport.(*http.Transport).Clone()
	insecureTransport.MaxIdleConns = HttpMaxIdleConns
	insecureTransport.MaxConnsPerHost = HttpMaxConnsPerHost
	insecureTransport.MaxIdleConnsPerHost = HttpMaxIdleConnsPerHost
	insecureTransport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
	}

	return &orasStore{config: &conf,
		rawConfig:          config.StoreConfig{Version: version, Store: storeConfig},
		localCache:         localRegistry,
		authProvider:       authenticationProvider,
		httpClient:         &http.Client{Timeout: 10 * time.Second, Transport: secureTransport},
		httpClientInsecure: &http.Client{Timeout: 10 * time.Second, Transport: insecureTransport}}, nil
}

func (store *orasStore) Name() string {
	return storeName
}

func (store *orasStore) GetConfig() *config.StoreConfig {
	return &store.rawConfig
}

func (store *orasStore) ListReferrers(ctx context.Context, subjectReference common.Reference, artifactTypes []string, nextToken string, subjectDesc *ocispecs.SubjectDescriptor) (referrerstore.ListReferrersResult, error) {
	repository, expiry, err := store.createRepository(ctx, subjectReference)
	if err != nil {
		return referrerstore.ListReferrersResult{}, err
	}

	// resolve subject descriptor if not provided
	var resolvedSubjectDesc *ocispecs.SubjectDescriptor
	if subjectDesc != nil {
		resolvedSubjectDesc = subjectDesc
	} else {
		resolvedSubjectDesc, err = store.GetSubjectDescriptor(ctx, subjectReference)
		if err != nil {
			store.evictAuthCache(subjectReference.Original, err)
			return referrerstore.ListReferrersResult{}, err
		}
	}

	// find all referrers referencing subject descriptor
	artifactTypeFilter := ""
	var referrerDescriptors []oci.Descriptor
	if err := repository.Referrers(ctx, resolvedSubjectDesc.Descriptor, artifactTypeFilter, func(referrers []oci.Descriptor) error {
		referrerDescriptors = append(referrerDescriptors, referrers...)
		return nil
	}); err != nil && !errors.Is(err, errdef.ErrNotFound) {
		store.evictAuthCache(subjectReference.Original, err)
		return referrerstore.ListReferrersResult{}, err
	}
	// add the repository client to the auth cache if all repository operations successful
	store.addAuthCache(subjectReference.Original, repository, expiry)

	// convert artifact descriptors to oci descriptor with artifact type
	var referrers []ocispecs.ReferenceDescriptor
	for _, referrer := range referrerDescriptors {
		referrers = append(referrers, OciDescriptorToReferenceDescriptor(referrer))
	}

	if store.config.CosignEnabled {
		cosignReferences, err := getCosignReferences(subjectReference)
		if err != nil {
			return referrerstore.ListReferrersResult{}, err
		}
		referrers = append(referrers, *cosignReferences...)
	}

	return referrerstore.ListReferrersResult{Referrers: referrers}, nil
}

func (store *orasStore) GetBlobContent(ctx context.Context, subjectReference common.Reference, digest digest.Digest) ([]byte, error) {
	var err error
	repository, expiry, err := store.createRepository(ctx, subjectReference)
	if err != nil {
		return nil, err
	}

	// create a dummy Descriptor to check the local store cache
	blobDescriptor := oci.Descriptor{
		Digest: digest,
		Size:   0, // dummy size value
	}

	// check if blob exists in local ORAS cache
	isCached, err := store.localCache.Exists(ctx, blobDescriptor)
	if err != nil {
		return nil, err
	}

	if !isCached {
		// generate the reference path with digest
		ref := fmt.Sprintf("%s@%s", subjectReference.Path, digest)

		// fetch blob content from remote repository
		blobDesc, rc, err := repository.Blobs().FetchReference(ctx, ref)
		if err != nil {
			store.evictAuthCache(subjectReference.Original, err)
			return nil, err
		}

		// push fetched content to local ORAS cache
		orasExistsExpectedError := fmt.Errorf("%s: %s: %w", blobDesc.Digest, blobDesc.MediaType, errdef.ErrAlreadyExists)
		err = store.localCache.Push(ctx, blobDesc, rc)
		if err != nil && err.Error() != orasExistsExpectedError.Error() {
			return nil, err
		}
	}
	// add the repository client to the auth cache if all repository operations successful
	store.addAuthCache(subjectReference.Original, repository, expiry)

	return store.getRawContentFromCache(ctx, blobDescriptor)
}

func (store *orasStore) GetReferenceManifest(ctx context.Context, subjectReference common.Reference, referenceDesc ocispecs.ReferenceDescriptor) (ocispecs.ReferenceManifest, error) {
	repository, expiry, err := store.createRepository(ctx, subjectReference)
	if err != nil {
		return ocispecs.ReferenceManifest{}, err
	}
	var manifestBytes []byte
	// check if manifest exists in local ORAS cache
	isCached, err := store.localCache.Exists(ctx, referenceDesc.Descriptor)
	if err != nil {
		return ocispecs.ReferenceManifest{}, err
	}

	if !isCached {
		// fetch manifest content from repository
		manifestReader, err := repository.Fetch(ctx, referenceDesc.Descriptor)
		if err != nil {
			store.evictAuthCache(subjectReference.Original, err)
			return ocispecs.ReferenceManifest{}, err
		}

		manifestBytes, err = io.ReadAll(manifestReader)
		if err != nil {
			return ocispecs.ReferenceManifest{}, err
		}

		// push fetched manifest to local ORAS cache
		orasExistsExpectedError := fmt.Errorf("%s: %s: %w", referenceDesc.Descriptor.Digest, referenceDesc.Descriptor.MediaType, errdef.ErrAlreadyExists)
		store.localCache.Push(ctx, referenceDesc.Descriptor, bytes.NewReader(manifestBytes))
		if err != nil && err.Error() != orasExistsExpectedError.Error() {
			return ocispecs.ReferenceManifest{}, err
		}

		// add the repository client to the auth cache if all repository operations successful
		store.addAuthCache(subjectReference.Original, repository, expiry)
	} else {
		manifestBytes, err = store.getRawContentFromCache(ctx, referenceDesc.Descriptor)
		if err != nil {
			return ocispecs.ReferenceManifest{}, err
		}
	}

	// marshal manifest bytes into reference manifest descriptor
	referenceManifest := ocispecs.ReferenceManifest{}
	if err := json.Unmarshal(manifestBytes, &referenceManifest); err != nil {
		return ocispecs.ReferenceManifest{}, err
	}

	return referenceManifest, nil
}

func (store *orasStore) GetSubjectDescriptor(ctx context.Context, subjectReference common.Reference) (*ocispecs.SubjectDescriptor, error) {
	repository, expiry, err := store.createRepository(ctx, subjectReference)
	if err != nil {
		return nil, err
	}

	desc, err := repository.Resolve(ctx, subjectReference.Original)
	if err != nil {
		store.evictAuthCache(subjectReference.Original, err)
		return nil, err
	}

	// add the repository client to the auth cache if all repository operations successful
	store.addAuthCache(subjectReference.Original, repository, expiry)

	return &ocispecs.SubjectDescriptor{Descriptor: desc}, nil
}

func (store *orasStore) createRepository(ctx context.Context, targetRef common.Reference) (*remote.Repository, time.Time, error) {
	if store.authProvider == nil || !store.authProvider.Enabled(ctx) {
		return nil, time.Now(), fmt.Errorf("auth provider not properly enabled")
	}

	if entry, ok := store.authCache.Load(targetRef.Original); ok {
		// if the auth cache entry expiration has not expired or it was never set
		cacheEntry := entry.(authCacheEntry)
		if cacheEntry.expiresOn.IsZero() || cacheEntry.expiresOn.After(time.Now()) {
			return cacheEntry.client, cacheEntry.expiresOn, nil
		}
	}

	authConfig, err := store.authProvider.Provide(ctx, targetRef.Original)
	if err != nil {
		logrus.Warningf("auth provider failed with err, %v", err)
		logrus.Info("attempting to use anonymous credentials")
	}

	// create new ORAS repository target to the image/repository reference
	repository, err := remote.NewRepository(targetRef.Original)
	if err != nil {
		return nil, time.Now(), err
	}

	// set the provider to return the resolved credentials
	credentialProvider := func(ctx context.Context, registry string) (auth.Credential, error) {
		if authConfig.Username != "" || authConfig.Password != "" || authConfig.IdentityToken != "" {
			return auth.Credential{
				Username:     authConfig.Username,
				Password:     authConfig.Password,
				RefreshToken: authConfig.IdentityToken,
			}, nil
		}
		return auth.EmptyCredential, nil
	}

	// set the repository client credentials
	repoClient := &auth.Client{
		Client: store.httpClient,
		Header: http.Header{
			"User-Agent": {ratifyUserAgent},
		},
		Cache:      auth.NewCache(),
		Credential: credentialProvider,
	}

	// enable insecure if specified in config
	if isInsecureRegistry(targetRef.Original, store.config) {
		repoClient.Client = store.httpClientInsecure
	}

	repository.Client = repoClient
	// enable plain HTTP if specified in config
	repository.PlainHTTP = store.config.UseHttp

	return repository, authConfig.ExpiresOn, nil
}

func (store *orasStore) getRawContentFromCache(ctx context.Context, descriptor oci.Descriptor) ([]byte, error) {
	reader, err := store.localCache.Fetch(ctx, descriptor)
	if err != nil {
		return nil, err
	}

	buf, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func (store *orasStore) addAuthCache(ref string, repository *remote.Repository, expiry time.Time) {
	store.authCache.LoadOrStore(ref, authCacheEntry{
		client:    repository,
		expiresOn: expiry,
	})
}

func (store *orasStore) evictAuthCache(ref string, err error) {
	store.authCache.Delete(ref)
	// TODO: add reliable way to conditionally evict based on error code
}
