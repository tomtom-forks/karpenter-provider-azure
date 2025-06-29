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

package instance

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resourcegraph/armresourcegraph"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/karpenter-provider-azure/pkg/auth"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/imagefamily"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instance/skuclient"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/loadbalancer"

	armopts "github.com/Azure/karpenter-provider-azure/pkg/utils/opts"
	klog "k8s.io/klog/v2"
)

type VirtualMachinesAPI interface {
	BeginCreateOrUpdate(ctx context.Context, resourceGroupName string, vmName string, parameters armcompute.VirtualMachine, options *armcompute.VirtualMachinesClientBeginCreateOrUpdateOptions) (*runtime.Poller[armcompute.VirtualMachinesClientCreateOrUpdateResponse], error)
	Get(ctx context.Context, resourceGroupName string, vmName string, options *armcompute.VirtualMachinesClientGetOptions) (armcompute.VirtualMachinesClientGetResponse, error)
	BeginUpdate(ctx context.Context, resourceGroupName string, vmName string, parameters armcompute.VirtualMachineUpdate, options *armcompute.VirtualMachinesClientBeginUpdateOptions) (*runtime.Poller[armcompute.VirtualMachinesClientUpdateResponse], error)
	BeginDelete(ctx context.Context, resourceGroupName string, vmName string, options *armcompute.VirtualMachinesClientBeginDeleteOptions) (*runtime.Poller[armcompute.VirtualMachinesClientDeleteResponse], error)
}

type AzureResourceGraphAPI interface {
	Resources(ctx context.Context, query armresourcegraph.QueryRequest, options *armresourcegraph.ClientResourcesOptions) (armresourcegraph.ClientResourcesResponse, error)
}

type VirtualMachineExtensionsAPI interface {
	BeginCreateOrUpdate(ctx context.Context, resourceGroupName string, vmName string, vmExtensionName string, extensionParameters armcompute.VirtualMachineExtension, options *armcompute.VirtualMachineExtensionsClientBeginCreateOrUpdateOptions) (*runtime.Poller[armcompute.VirtualMachineExtensionsClientCreateOrUpdateResponse], error)
}

type NetworkInterfacesAPI interface {
	BeginCreateOrUpdate(ctx context.Context, resourceGroupName string, networkInterfaceName string, parameters armnetwork.Interface, options *armnetwork.InterfacesClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.InterfacesClientCreateOrUpdateResponse], error)
	BeginDelete(ctx context.Context, resourceGroupName string, networkInterfaceName string, options *armnetwork.InterfacesClientBeginDeleteOptions) (*runtime.Poller[armnetwork.InterfacesClientDeleteResponse], error)
	Get(ctx context.Context, resourceGroupName string, networkInterfaceName string, options *armnetwork.InterfacesClientGetOptions) (armnetwork.InterfacesClientGetResponse, error)
}

// TODO: Move this to another package that more correctly reflects its usage across multiple providers
type AZClient struct {
	azureResourceGraphClient       AzureResourceGraphAPI
	virtualMachinesClient          VirtualMachinesAPI
	virtualMachinesExtensionClient VirtualMachineExtensionsAPI
	networkInterfacesClient        NetworkInterfacesAPI

	NodeImageVersionsClient imagefamily.NodeImageVersionsAPI
	ImageVersionsClient     imagefamily.CommunityGalleryImageVersionsAPI
	// SKU CLIENT is still using track 1 because skewer does not support the track 2 path. We need to refactor this once skewer supports track 2
	SKUClient           skuclient.SkuClient
	LoadBalancersClient loadbalancer.LoadBalancersAPI
}

func NewAZClientFromAPI(
	virtualMachinesClient VirtualMachinesAPI,
	azureResourceGraphClient AzureResourceGraphAPI,
	virtualMachinesExtensionClient VirtualMachineExtensionsAPI,
	interfacesClient NetworkInterfacesAPI,
	loadBalancersClient loadbalancer.LoadBalancersAPI,
	imageVersionsClient imagefamily.CommunityGalleryImageVersionsAPI,
	nodeImageVersionsClient imagefamily.NodeImageVersionsAPI,
	skuClient skuclient.SkuClient,
) *AZClient {
	return &AZClient{
		virtualMachinesClient:          virtualMachinesClient,
		azureResourceGraphClient:       azureResourceGraphClient,
		virtualMachinesExtensionClient: virtualMachinesExtensionClient,
		networkInterfacesClient:        interfacesClient,
		ImageVersionsClient:            imageVersionsClient,
		NodeImageVersionsClient:        nodeImageVersionsClient,
		SKUClient:                      skuClient,
		LoadBalancersClient:            loadBalancersClient,
	}
}

func CreateAZClient(ctx context.Context, cfg *auth.Config) (*AZClient, error) {
	// Defaulting env to Azure Public Cloud.
	env := azure.PublicCloud
	var err error
	if cfg.Cloud != "" {
		env, err = azure.EnvironmentFromName(cfg.Cloud)
		if err != nil {
			return nil, err
		}
	}

	azClient, err := NewAZClient(ctx, cfg, &env)
	if err != nil {
		return nil, err
	}

	return azClient, nil
}

func NewAZClient(ctx context.Context, cfg *auth.Config, env *azure.Environment) (*AZClient, error) {
	defaultAzureCred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	cred := auth.NewTokenWrapper(defaultAzureCred)
	opts := armopts.DefaultArmOpts()
	extensionsClient, err := armcompute.NewVirtualMachineExtensionsClient(cfg.SubscriptionID, cred, opts)
	if err != nil {
		return nil, err
	}

	interfacesClient, err := armnetwork.NewInterfacesClient(cfg.SubscriptionID, cred, opts)
	if err != nil {
		return nil, err
	}
	klog.V(5).Infof("Created network interface client %v using token credential", interfacesClient)

	virtualMachinesClient, err := armcompute.NewVirtualMachinesClient(cfg.SubscriptionID, cred, opts)
	if err != nil {
		return nil, err
	}
	klog.V(5).Infof("Created virtual machines client %v, using a token credential", virtualMachinesClient)
	azureResourceGraphClient, err := armresourcegraph.NewClient(cred, opts)
	if err != nil {
		return nil, err
	}
	klog.V(5).Infof("Created azure resource graph client %v, using a token credential", azureResourceGraphClient)

	communityImageVersionsClient, err := armcompute.NewCommunityGalleryImageVersionsClient(cfg.SubscriptionID, cred, opts)
	if err != nil {
		return nil, err
	}
	klog.V(5).Infof("Created image versions client %v, using a token credential", communityImageVersionsClient)

	nodeImageVersionsClient := imagefamily.NewNodeImageVersionsClient(cred)

	loadBalancersClient, err := armnetwork.NewLoadBalancersClient(cfg.SubscriptionID, cred, opts)
	if err != nil {
		return nil, err
	}
	klog.V(5).Infof("Created load balancers client %v, using a token credential", loadBalancersClient)

	// TODO: this one is not enabled for rate limiting / throttling ...
	// TODO Move this over to track 2 when skewer is migrated
	skuClient := skuclient.NewSkuClient(ctx, cfg, env)

	return NewAZClientFromAPI(virtualMachinesClient,
		azureResourceGraphClient,
		extensionsClient,
		interfacesClient,
		loadBalancersClient,
		communityImageVersionsClient,
		nodeImageVersionsClient,
		skuClient), nil
}
