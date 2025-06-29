/*
Portions Copyright (c) Microsoft Corporation.

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

package imagefamily

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1alpha2"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"k8s.io/client-go/kubernetes"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

type Provider struct {
	kubernetesVersionCache *cache.Cache
	cm                     *pretty.ChangeMonitor
	location               string
	kubernetesInterface    kubernetes.Interface
	imageCache             *cache.Cache
	imageVersionsClient    CommunityGalleryImageVersionsAPI
	subscription           string
	NodeImageVersions      NodeImageVersionsAPI
}

const (
	kubernetesVersionCacheKey = "kubernetesVersion"

	imageExpirationInterval    = time.Hour * 24 * 3
	imageCacheCleaningInterval = time.Hour * 1

	sharedImageKey                  = "%s/%s" // imageGallery + imageDefinition
	sharedImageGalleryImageIDFormat = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/galleries/%s/images/%s/versions/%s"
	communityImageKey               = "%s/%s" // PublicGalleryURL + communityImageName
	communityImageIDFormat          = "/CommunityGalleries/%s/images/%s/versions/%s"
)

func NewProvider(kubernetesInterface kubernetes.Interface, kubernetesVersionCache *cache.Cache, versionsClient CommunityGalleryImageVersionsAPI, location, subscription string, nodeImageVersionsClient NodeImageVersionsAPI) *Provider {
	return &Provider{
		kubernetesVersionCache: kubernetesVersionCache,
		imageCache:             cache.New(imageExpirationInterval, imageCacheCleaningInterval),
		location:               location,
		imageVersionsClient:    versionsClient,
		cm:                     pretty.NewChangeMonitor(),
		kubernetesInterface:    kubernetesInterface,
		subscription:           subscription,
		NodeImageVersions:      nodeImageVersionsClient,
	}
}

// Get returns Distro and Image ID for the given instance type. Images may vary due to architecture, accelerator, etc
func (p *Provider) Get(ctx context.Context, nodeClass *v1alpha2.AKSNodeClass, instanceType *cloudprovider.InstanceType, imageFamily ImageFamily) (string, string, error) {
	if reflect.DeepEqual(nodeClass.Spec.CustomImageTerm, v1alpha2.CustomImageTerm{}) {
		defaultImages := imageFamily.DefaultImages()
		for _, defaultImage := range defaultImages {
			if err := instanceType.Requirements.Compatible(defaultImage.Requirements, v1alpha2.AllowUndefinedWellKnownAndRestrictedLabels); err == nil {
				communityImageName, publicGalleryURL := defaultImage.ImageDefinition, defaultImage.PublicGalleryURL
				imageID, err := p.getCIGImageID(communityImageName, publicGalleryURL)
				if err != nil {
					return "", "", err
				}
				return defaultImage.Distro, imageID, nil
			}
		}
	} else {
		imageID, err := p.GetCustomImageID(ctx, &nodeClass.Spec.CustomImageTerm)
		if err != nil {
			return "", "", err
		}
		return nodeClass.Spec.CustomImageTerm.DistroName, imageID, nil
	}

	return "", "", fmt.Errorf("no compatible images found for instance type %s", instanceType.Name)
}

func (p *Provider) GetLatestImageID(ctx context.Context, defaultImage DefaultImageOutput) (string, error) {
	// Note: one could argue that we could narrow the key one level further to ImageDefinition since no two AKS ImageDefinitions that are supported
	// by karpenter have the same name, but for EdgeZone support this is not the case.
	key := lo.Ternary(options.FromContext(ctx).UseSIG,
		fmt.Sprintf(sharedImageKey, defaultImage.GalleryName, defaultImage.ImageDefinition),
		fmt.Sprintf(communityImageKey, defaultImage.PublicGalleryURL, defaultImage.ImageDefinition),
	)
	if imageID, ok := p.imageCache.Get(key); ok {
		return imageID.(string), nil
	}

	// retrieve ARM Resource ID for the image and write it to the cache
	imageID, err := p.resolveImageID(ctx, defaultImage, options.FromContext(ctx).UseSIG)
	if err != nil {
		return "", err
	}
	p.imageCache.Set(key, imageID, imageExpirationInterval)
	logging.FromContext(ctx).With("image-id", imageID).Info("discovered new image id")
	return imageID, nil
}

func (p *Provider) GetCustomImageID(ctx context.Context, imageTerm *v1alpha2.CustomImageTerm) (string, error) {
	key := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/galleries/%s/images/%s/versions/%s", imageTerm.GallerySubscriptionID, imageTerm.GalleryResourceGroupName, imageTerm.GalleryName, imageTerm.Name, imageTerm.Version)
	imageID, found := p.imageCache.Get(key)
	if found {
		return imageID.(string), nil
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		log.Fatalf("failed to obtain a credential: %v", err)
	}
	clientFactory, err := armcompute.NewClientFactory(imageTerm.GallerySubscriptionID, cred, nil)
	if err != nil {
		log.Fatalf("failed to create client: %v", err)
	}
	imageCandidate := armcompute.GalleryImageVersion{}
	if imageTerm.Version != "" {
		imageInfo, err := clientFactory.NewGalleryImageVersionsClient().Get(ctx, imageTerm.GalleryResourceGroupName, imageTerm.GalleryName, imageTerm.Name, imageTerm.Version, nil)
		if err != nil {
			return "", err
		}
		imageCandidate = imageInfo.GalleryImageVersion
	} else {
		pager := clientFactory.NewGalleryImageVersionsClient().NewListByGalleryImagePager(imageTerm.GalleryResourceGroupName, imageTerm.GalleryName, imageTerm.Name, nil)
		for pager.More() {
			page, err := pager.NextPage(context.Background())
			if err != nil {
				return "", err
			}
			for _, imageVersion := range page.GalleryImageVersionList.Value {
				if lo.IsEmpty(imageCandidate.ID) || imageVersion.Properties.PublishingProfile.PublishedDate.After(*imageCandidate.Properties.PublishingProfile.PublishedDate) {
					imageCandidate = *imageVersion
				}
			}
		}
	}

	if p.cm.HasChanged(key, *imageCandidate.ID) {
		logging.FromContext(ctx).With("image-id", imageCandidate.ID).Info("discovered new image id")
	}
	p.imageCache.Set(key, *imageCandidate.ID, imageExpirationInterval)
	return *imageCandidate.ID, nil
}

func (p *Provider) KubeServerVersion(ctx context.Context) (string, error) {
	if version, ok := p.kubernetesVersionCache.Get(kubernetesVersionCacheKey); ok {
		return version.(string), nil
	}
	serverVersion, err := p.kubernetesInterface.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	version := strings.TrimPrefix(serverVersion.GitVersion, "v") // v1.24.9 -> 1.24.9
	p.kubernetesVersionCache.SetDefault(kubernetesVersionCacheKey, version)
	if p.cm.HasChanged("kubernetes-version", version) {
		logging.FromContext(ctx).With("kubernetes-version", version).Debugf("discovered kubernetes version")
	}
	return version, nil
}

func (p *Provider) resolveImageID(ctx context.Context, defaultImage DefaultImageOutput, useSIG bool) (string, error) {
	if useSIG {
		return p.getSIGImageID(ctx, defaultImage)
	}
	return p.getCIGImageID(defaultImage.PublicGalleryURL, defaultImage.ImageDefinition)
}

func (p *Provider) getSIGImageID(ctx context.Context, imgStub DefaultImageOutput) (string, error) {
	versions, err := p.NodeImageVersions.List(ctx, p.location, p.subscription)
	if err != nil {
		return "", err
	}
	for _, version := range versions.Values {
		if imgStub.ImageDefinition == version.SKU {
			imageID := fmt.Sprintf(sharedImageGalleryImageIDFormat, options.FromContext(ctx).SIGSubscriptionID, imgStub.GalleryResourceGroup, imgStub.GalleryName, imgStub.ImageDefinition, version.Version)
			return imageID, nil
		}
	}
	return "", fmt.Errorf("failed to get the latest version of the image %s", imgStub.ImageDefinition)
}

func (p *Provider) getCIGImageID(publicGalleryURL, communityImageName string) (string, error) {
	imageVersion, err := p.latestNodeImageVersionCommunity(publicGalleryURL, communityImageName)
	if err != nil {
		return "", err
	}
	return BuildImageIDCIG(publicGalleryURL, communityImageName, imageVersion), nil
}

func (p *Provider) latestNodeImageVersionCommunity(publicGalleryURL, communityImageName string) (string, error) {
	pager := p.imageVersionsClient.NewListPager(p.location, publicGalleryURL, communityImageName, nil)
	topImageVersionCandidate := armcompute.CommunityGalleryImageVersion{}
	for pager.More() {
		page, err := pager.NextPage(context.Background())
		if err != nil {
			return "", err
		}
		for _, imageVersion := range page.CommunityGalleryImageVersionList.Value {
			if lo.IsEmpty(topImageVersionCandidate) || imageVersion.Properties.PublishedDate.After(*topImageVersionCandidate.Properties.PublishedDate) {
				topImageVersionCandidate = *imageVersion
			}
		}
	}
	return lo.FromPtr(topImageVersionCandidate.Name), nil
}

func BuildImageIDCIG(publicGalleryURL, communityImageName, imageVersion string) string {
	return fmt.Sprintf(communityImageIDFormat, publicGalleryURL, communityImageName, imageVersion)
}
