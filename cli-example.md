# Walking the pipeline by hand with `az`

This is the same flow the eventual Go app will perform, but driven from
the Azure CLI so you can see each step in isolation.

```
upload a blob -> Event Grid system topic fires -> message lands in Event Hub
```

The producer half (uploading a blob) maps cleanly onto a single `az`
command. The consumer half does **not** — Event Hub receives are AMQP-only
and there is no `az eventhubs` data-plane command for reading messages.
The Go app will use the AMQP-based `azeventhubs` SDK for that. To verify
the pipeline end-to-end from the CLI we lean on **metrics** instead: if
the system topic shows a delivery and the hub shows an incoming message
for the same time window, the wiring is correct.

## Prerequisites

- `pulumi up` has been run in `infra/` and the stack is healthy.
- You are logged in: `az login` (and the right subscription is selected:
  `az account show`).
- Pulumi resolved the same principal — the role assignments in
  `infra/main.go` were granted to `azuread.GetClientConfig().ObjectId`,
  which is the AAD object ID of whoever ran `pulumi up`. If that is the
  same user you are `az`-logged-in as, you already have
  `Storage Blob Data Contributor` on the account.

## 1. Pull the stack outputs into shell variables

```bash
cd infra

RG=$(pulumi stack output resourceGroupName)
SA=$(pulumi stack output storageAccountName)
CONTAINER=$(pulumi stack output containerName)
NS_FQDN=$(pulumi stack output eventHubNamespaceFqdn)
HUB=$(pulumi stack output eventHubName)

NS=${NS_FQDN%.servicebus.windows.net}    # short namespace name
SUB=$(az account show --query id -o tsv)
```

Sanity check:

```bash
echo "rg=$RG sa=$SA container=$CONTAINER ns=$NS hub=$HUB"
```

## 2. Drop a file into the container (this is the trigger)

`--auth-mode login` tells the CLI to authenticate via AAD using your
`az login` session, matching the keyless model the Pulumi stack sets up.
No connection string or account key is used.

```bash
echo "hello from cli $(date -Is)" > /tmp/hello.txt

az storage blob upload \
  --account-name "$SA" \
  --container-name "$CONTAINER" \
  --name "hello-$(date +%s).txt" \
  --file /tmp/hello.txt \
  --auth-mode login
```

The moment this returns, the storage account emits a
`Microsoft.Storage.BlobCreated` event. The Event Grid system topic
matches it against the subscription's filter
(`subjectBeginsWith=/blobServices/default/containers/uploads/`) and
delivers it to the Event Hub via the system topic's managed identity.

**Portal**: `Home -> Storage accounts -> $SA -> Data storage -> Containers
-> uploads`. The blob you just uploaded will be listed here. Click it to
see its properties and **Download** to confirm the contents.

## 3. Verify delivery via metrics

There is a 1–3 minute lag before metrics are queryable. If the first
call comes back empty, wait a minute and re-run.

### 3a. Event Grid delivered the event

```bash
SYS_TOPIC=$(az eventgrid system-topic list -g "$RG" --query "[0].name" -o tsv)
TOPIC_ID=$(az eventgrid system-topic show -g "$RG" -n "$SYS_TOPIC" --query id -o tsv)

az monitor metrics list \
  --resource "$TOPIC_ID" \
  --metric "PublishSuccessCount,DeliverySuccessCount,DeliveryAttemptFailCount" \
  --interval PT1M \
  --query "value[].{name:name.value, points:timeseries[0].data[?total>\`0\`].[timeStamp,total]}"
```

A non-empty `DeliverySuccessCount` series is the proof that Event Grid
handed the event off to the Event Hub.

**Portal**: open the system topic directly from the storage account at
`Home -> Storage accounts -> $SA -> Events`, or navigate to it from
`Home -> Resource groups -> $RG` (look for the *Event Grid System
Topic* resource). On the topic:

- **Monitoring -> Metrics** -> add `Delivery Success Count`,
  `Publish Success Count`, `Delivery Failure Count`. Same data as the
  CLI, with a chart.
- **Event Subscriptions** -> click `blobToHub` -> the *Metrics* tab on
  the subscription shows per-subscription delivery stats; the
  *Filters* tab shows the configured event type and subject prefix.

### 3b. The Event Hub received it

```bash
HUB_ID="/subscriptions/$SUB/resourceGroups/$RG/providers/Microsoft.EventHub/namespaces/$NS/eventhubs/$HUB"

az monitor metrics list \
  --resource "$HUB_ID" \
  --metric "IncomingMessages,IncomingBytes" \
  --interval PT1M \
  --query "value[].{name:name.value, points:timeseries[0].data[?total>\`0\`].[timeStamp,total]}"
```

A non-empty `IncomingMessages` series is the proof that the message
arrived in the hub.

**Portal**: `Home -> Event Hubs -> $NS -> Entities -> Event Hubs ->
notifications -> Monitoring -> Metrics`. Add `Incoming Messages` and
`Incoming Bytes`. The namespace-level **Overview** page also has a
live throughput chart that shows recent activity at a glance.

## 4. Peek at the actual message body (no CLI for this)

Reading from an Event Hub requires AMQP. Options, in order of effort:

- **Portal**: `Home -> Event Hubs -> $NS -> Entities -> Event Hubs ->
  notifications -> Data Explorer (preview)`. Pick a partition, click
  **View events**. You will see the JSON body of the
  `Microsoft.Storage.BlobCreated` event Event Grid forwarded — that's
  the literal payload your Go consumer will receive.
- **The Go app**, once we build it: `azeventhubs.NewConsumerClient` with
  `azidentity.NewDefaultAzureCredential`, pointed at `$NS_FQDN` and
  `$HUB`. This is what step 4 will become.

There is intentionally no `kcat`/`kafkacat` snippet here: the
Kafka-compatible surface of Event Hubs is real, but it expects SASL with
a connection string by default, and this project's whole point is to
demonstrate the keyless model. Wiring Kafka clients to authenticate via
AAD/OAuth is a meaningful detour and not what the example is about.

## 5. Clean up the test blob

```bash
az storage blob delete \
  --account-name "$SA" \
  --container-name "$CONTAINER" \
  --name "<the name you used above>" \
  --auth-mode login
```

(Deletes do **not** fire on this subscription — we filter to
`BlobCreated` only, so no spurious events.)

---

## Portal cheat sheet

If you'd rather start from the portal than the CLI, every resource the
stack creates lives under `Home -> Resource groups -> $RG`. From there:

| You want to see…                  | Click through                                                                            |
|-----------------------------------|------------------------------------------------------------------------------------------|
| The blob you uploaded             | `$SA` -> Data storage -> Containers -> `uploads`                                         |
| Event Grid wiring & filters       | `$SA` -> Events  *(or)*  the system topic resource -> Event Subscriptions -> `blobToHub` |
| Did Event Grid deliver?           | System topic -> Monitoring -> Metrics -> `Delivery Success Count`                        |
| Did the hub receive?              | `$NS` -> Entities -> Event Hubs -> `notifications` -> Monitoring -> Metrics              |
| The actual message body           | `$NS` -> Entities -> Event Hubs -> `notifications` -> Data Explorer                      |
| Role assignments granted by Pulumi| `$SA` and `notifications` hub -> Access control (IAM) -> Role assignments tab            |
| Anything failing / diagnostics    | `$RG` -> Activity log                                                                    |
