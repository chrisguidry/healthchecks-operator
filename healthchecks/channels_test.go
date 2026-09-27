package healthchecks_test

import (
	"context"
	"testing"
)

func TestChannelsListsTheSeededChannels(t *testing.T) {
	client, server := newTestClient(t)
	id := server.SeedChannel("Pushover", "po")

	channels, err := client.Channels(context.Background())
	if err != nil {
		t.Fatalf("Channels: %v", err)
	}
	if len(channels) != 1 || channels[0].ID != id || channels[0].Name != "Pushover" || channels[0].Kind != "po" {
		t.Errorf("channels = %+v, want one Pushover channel with id %s", channels, id)
	}
}
