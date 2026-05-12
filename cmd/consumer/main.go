// Command consumer reads Event Grid events that the storage account
// forwarded into the Event Hub and prints a one-line notice for every
// BlobCreated event it sees.
//
// Required environment (sourced from the repo-root .env):
//
//	EVENT_HUB_NAMESPACE_FQDN   e.g. xxxxxxx.servicebus.windows.net
//	EVENT_HUB_NAME             e.g. notifications
//
// Authentication is via azidentity.DefaultAzureCredential. The Pulumi
// stack grants the signed-in principal the "Azure Event Hubs Data
// Receiver" role on the hub, so `az login` is sufficient.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azeventhubs/v2"
	"golang.org/x/sync/errgroup"
)

const (
	blobCreatedEventType = "Microsoft.Storage.BlobCreated"
	receiveTimeout       = 30 * time.Second
	receiveBatchSize     = 50
	closeTimeout         = 5 * time.Second
)

// eventGridEvent is the subset of the Event Grid schema we care about.
// Event Grid delivers events to Event Hub as a JSON array per message.
type eventGridEvent struct {
	ID        string          `json:"id"`
	Topic     string          `json:"topic"`
	Subject   string          `json:"subject"`
	EventType string          `json:"eventType"`
	EventTime time.Time       `json:"eventTime"`
	Data      json.RawMessage `json:"data"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("consumer exiting with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	fqdn := strings.TrimSpace(os.Getenv("EVENT_HUB_NAMESPACE_FQDN"))
	hubName := strings.TrimSpace(os.Getenv("EVENT_HUB_NAME"))
	if fqdn == "" || hubName == "" {
		return errors.New("EVENT_HUB_NAMESPACE_FQDN and EVENT_HUB_NAME must be set; run `set -a; source .env; set +a`")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return fmt.Errorf("acquire azure credential: %w", err)
	}

	client, err := azeventhubs.NewConsumerClient(fqdn, hubName, azeventhubs.DefaultConsumerGroup, cred, nil)
	if err != nil {
		return fmt.Errorf("new consumer client: %w", err)
	}
	defer closeWithTimeout("consumer client", client.Close)

	props, err := client.GetEventHubProperties(ctx, nil)
	if err != nil {
		return fmt.Errorf("fetch event hub properties: %w", err)
	}
	slog.Info("starting consumer",
		"fqdn", fqdn,
		"hub", hubName,
		"consumer_group", azeventhubs.DefaultConsumerGroup,
		"partitions", props.PartitionIDs,
	)

	g, gctx := errgroup.WithContext(ctx)
	for _, pid := range props.PartitionIDs {
		g.Go(func() error {
			return consumePartition(gctx, client, pid)
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	slog.Info("consumer shut down cleanly")
	return nil
}

func consumePartition(ctx context.Context, client *azeventhubs.ConsumerClient, partitionID string) error {
	pc, err := client.NewPartitionClient(partitionID, &azeventhubs.PartitionClientOptions{
		StartPosition: azeventhubs.StartPosition{Latest: to.Ptr(true)},
	})
	if err != nil {
		return fmt.Errorf("partition %s: new client: %w", partitionID, err)
	}
	defer closeWithTimeout(fmt.Sprintf("partition %s client", partitionID), pc.Close)

	slog.Info("partition ready", "partition", partitionID)

	for {
		receiveCtx, cancel := context.WithTimeout(ctx, receiveTimeout)
		events, err := pc.ReceiveEvents(receiveCtx, receiveBatchSize, nil)
		cancel()

		for _, ev := range events {
			handleEventGridBatch(ev.Body)
		}

		switch {
		case err == nil, errors.Is(err, context.DeadlineExceeded):
			// Empty batch or per-call timeout — keep going.
		case errors.Is(err, context.Canceled):
			return nil
		default:
			return fmt.Errorf("partition %s: receive: %w", partitionID, err)
		}
	}
}

func handleEventGridBatch(body []byte) {
	var events []eventGridEvent
	if err := json.Unmarshal(body, &events); err != nil {
		slog.Warn("could not parse event payload", "err", err, "body", string(body))
		return
	}
	for _, e := range events {
		if e.EventType != blobCreatedEventType {
			slog.Debug("ignoring non-BlobCreated event", "type", e.EventType, "subject", e.Subject)
			continue
		}
		fmt.Printf("File %s has arrived, starting work with file\n", blobNameFromSubject(e.Subject))
	}
}

// blobNameFromSubject extracts the blob name from an Event Grid subject
// like "/blobServices/default/containers/uploads/blobs/path/to/file.txt".
func blobNameFromSubject(subject string) string {
	const marker = "/blobs/"
	if idx := strings.Index(subject, marker); idx != -1 {
		return subject[idx+len(marker):]
	}
	return subject
}

// closeWithTimeout invokes a Close(ctx)-style function with a fresh
// background context that has a short deadline, so shutdown still works
// when the main context has already been cancelled.
func closeWithTimeout(label string, closer func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	if err := closer(ctx); err != nil {
		slog.Warn("close failed", "what", label, "err", err)
	}
}
