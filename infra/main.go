package main

import (
	"fmt"

	"github.com/pulumi/pulumi-azure-native-sdk/authorization/v3"
	"github.com/pulumi/pulumi-azure-native-sdk/eventgrid/v3"
	"github.com/pulumi/pulumi-azure-native-sdk/eventhub/v3"
	"github.com/pulumi/pulumi-azure-native-sdk/resources/v3"
	"github.com/pulumi/pulumi-azure-native-sdk/storage/v3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	containerName = "uploads"
	eventHubName  = "notifications"

	// Built-in Azure role definition GUIDs. These are well-known and stable.
	roleEventHubsDataSender        = "2b629674-e913-4c01-ae53-ef4638d8f975"
	roleEventHubsDataReceiver      = "a638d3c7-ab3a-418d-83e6-5f17a39d4fde"
	roleStorageBlobDataContributor = "ba92f5b4-2d11-453d-a403-e96b0029c9fe"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		clientCfg, err := authorization.GetClientConfig(ctx, nil)
		if err != nil {
			return err
		}
		roleDefID := func(guid string) string {
			return fmt.Sprintf(
				"/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s",
				clientCfg.SubscriptionId, guid,
			)
		}

		rg, err := resources.NewResourceGroup(ctx, "rg", nil)
		if err != nil {
			return err
		}

		sa, err := storage.NewStorageAccount(ctx, "sa", &storage.StorageAccountArgs{
			ResourceGroupName:     rg.Name,
			Kind:                  pulumi.String("StorageV2"),
			Sku:                   &storage.SkuArgs{Name: pulumi.String("Standard_LRS")},
			AllowBlobPublicAccess: pulumi.Bool(false),
			MinimumTlsVersion:     pulumi.String("TLS1_2"),
		})
		if err != nil {
			return err
		}

		container, err := storage.NewBlobContainer(ctx, "uploads", &storage.BlobContainerArgs{
			ResourceGroupName: rg.Name,
			AccountName:       sa.Name,
			ContainerName:     pulumi.String(containerName),
		})
		if err != nil {
			return err
		}

		ehNs, err := eventhub.NewNamespace(ctx, "ehns", &eventhub.NamespaceArgs{
			ResourceGroupName: rg.Name,
			Sku: &eventhub.SkuArgs{
				Name: pulumi.String("Standard"),
				Tier: pulumi.String("Standard"),
			},
		})
		if err != nil {
			return err
		}

		hub, err := eventhub.NewEventHub(ctx, "hub", &eventhub.EventHubArgs{
			ResourceGroupName:      rg.Name,
			NamespaceName:          ehNs.Name,
			EventHubName:           pulumi.String(eventHubName),
			PartitionCount:         pulumi.Float64(1),
			MessageRetentionInDays: pulumi.Float64(1),
		})
		if err != nil {
			return err
		}

		sysTopic, err := eventgrid.NewSystemTopic(ctx, "topic", &eventgrid.SystemTopicArgs{
			ResourceGroupName: rg.Name,
			Source:            sa.ID(),
			TopicType:         pulumi.String("Microsoft.Storage.StorageAccounts"),
			Identity: &eventgrid.IdentityInfoArgs{
				Type: pulumi.String("SystemAssigned"),
			},
		})
		if err != nil {
			return err
		}

		sysTopicPrincipalID := sysTopic.Identity.ApplyT(func(i *eventgrid.IdentityInfoResponse) string {
			if i == nil || i.PrincipalId == nil {
				return ""
			}
			return *i.PrincipalId
		}).(pulumi.StringOutput)

		// Event Grid system topic identity -> Data Sender on the hub.
		egToHubSender, err := authorization.NewRoleAssignment(ctx, "egToHubSender", &authorization.RoleAssignmentArgs{
			Scope:            hub.ID(),
			PrincipalId:      sysTopicPrincipalID,
			PrincipalType:    pulumi.String("ServicePrincipal"),
			RoleDefinitionId: pulumi.String(roleDefID(roleEventHubsDataSender)),
		})
		if err != nil {
			return err
		}

		// Signed-in principal -> Data Receiver on the hub.
		_, err = authorization.NewRoleAssignment(ctx, "userHubReceiver", &authorization.RoleAssignmentArgs{
			Scope:            hub.ID(),
			PrincipalId:      pulumi.String(clientCfg.ObjectId),
			RoleDefinitionId: pulumi.String(roleDefID(roleEventHubsDataReceiver)),
		})
		if err != nil {
			return err
		}

		// Signed-in principal -> Storage Blob Data Contributor on the storage account
		// so the app can upload blobs to trigger the pipeline.
		_, err = authorization.NewRoleAssignment(ctx, "userBlobContributor", &authorization.RoleAssignmentArgs{
			Scope:            sa.ID(),
			PrincipalId:      pulumi.String(clientCfg.ObjectId),
			RoleDefinitionId: pulumi.String(roleDefID(roleStorageBlobDataContributor)),
		})
		if err != nil {
			return err
		}

		// Event subscription on the system topic: filter BlobCreated within the
		// uploads container, deliver to the Event Hub via the system topic's
		// managed identity. DependsOn the role assignment so the identity has
		// permission by the time Event Grid validates the destination.
		_, err = eventgrid.NewSystemTopicEventSubscription(ctx, "blobToHub", &eventgrid.SystemTopicEventSubscriptionArgs{
			ResourceGroupName: rg.Name,
			SystemTopicName:   sysTopic.Name,
			EventDeliverySchema: pulumi.String("EventGridSchema"),
			Filter: &eventgrid.EventSubscriptionFilterArgs{
				IncludedEventTypes: pulumi.StringArray{
					pulumi.String("Microsoft.Storage.BlobCreated"),
				},
				SubjectBeginsWith: pulumi.String("/blobServices/default/containers/" + containerName + "/"),
			},
			DeliveryWithResourceIdentity: &eventgrid.DeliveryWithResourceIdentityArgs{
				Identity: &eventgrid.EventSubscriptionIdentityArgs{
					Type: pulumi.String("SystemAssigned"),
				},
				Destination: eventgrid.EventHubEventSubscriptionDestinationArgs{
					EndpointType: pulumi.String("EventHub"),
					ResourceId:   hub.ID().ToStringOutput().ToStringPtrOutput(),
				},
			},
		}, pulumi.DependsOn([]pulumi.Resource{egToHubSender}))
		if err != nil {
			return err
		}

		ctx.Export("resourceGroupName", rg.Name)
		ctx.Export("storageAccountName", sa.Name)
		ctx.Export("containerName", container.Name)
		ctx.Export("eventHubNamespaceFqdn", pulumi.Sprintf("%s.servicebus.windows.net", ehNs.Name))
		ctx.Export("eventHubName", hub.Name)

		return nil
	})
}
