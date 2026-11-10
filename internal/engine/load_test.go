package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/results"
)

// refuseData makes every new TCP data connection fail, as a firewall that
// lets the control session and UDP through but not new connections would.
// The control session is already open and UDP has its own socket.
func refuseData(c *Client) { c.Addr = "127.0.0.1:1" }

// Tests that measure "under load" must fail when the load did not run:
// their numbers would be from an idle link and look excellent.
func TestLoadedTestsFailWithoutTheirLoad(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	t.Run("bufferbloat", func(t *testing.T) {
		c := dial(t, startEndpoint(t))
		refuseData(c)
		res, err := RunBufferbloat(ctx, c, BloatParams{Idle: 300 * time.Millisecond, Phase: time.Second, Streams: 2})
		if err == nil {
			t.Fatalf("want an error, got grade %q", res.Grade)
		}
		if !strings.Contains(err.Error(), "download load did not run") {
			t.Errorf("the error should say which load failed: %v", err)
		}
	})
	t.Run("voip-loaded", func(t *testing.T) {
		c := dial(t, startEndpoint(t))
		refuseData(c)
		res, err := RunVoIP(ctx, c, VoIPParams{Duration: time.Second, Load: true})
		if err == nil {
			t.Fatalf("want an error, got verdict %q", res.Verdict)
		}
		if !strings.Contains(err.Error(), "load did not run") {
			t.Errorf("the error should say the load failed: %v", err)
		}
	})
	t.Run("voip without load still works", func(t *testing.T) {
		c := dial(t, startEndpoint(t))
		refuseData(c)
		if _, err := RunVoIP(ctx, c, VoIPParams{Duration: time.Second}); err != nil {
			t.Errorf("a call without load needs no data connection: %v", err)
		}
	})
	t.Run("mix with transfers", func(t *testing.T) {
		c := dial(t, startEndpoint(t))
		refuseData(c)
		res, err := RunMix(ctx, c, MixParams{Spec: "voice:1,download:1", Phase: time.Second})
		if err == nil {
			t.Fatalf("want an error, got verdict %q", res.Verdict)
		}
		if !strings.Contains(err.Error(), "file download did not run") {
			t.Errorf("the error should name the transfer: %v", err)
		}
	})
	t.Run("capacity with transfers", func(t *testing.T) {
		c := dial(t, startEndpoint(t))
		voice, _ := lookupMixKind("voice")
		// The search without transfers needs no data connection; refuse
		// them only once it has finished.
		cp := CapacityParams{Kind: "voice", Step: time.Second, RateCap: 1.5 * voice.wireBps(), WithTransfers: true}
		cp.OnStep = func(string, results.CapacityStep) { refuseData(c) }
		if _, err := RunCapacity(ctx, c, cp); err == nil {
			t.Fatal("want an error when the transfers cannot start")
		}
	})
}

func TestLoadErr(t *testing.T) {
	if err := loadErr("the load", &results.TCP{Mbps: 12}, nil); err != nil {
		t.Errorf("a load that ran is fine: %v", err)
	}
	if err := loadErr("the load", &results.TCP{}, nil); err == nil {
		t.Error("a load that carried nothing is not a load")
	}
	if err := loadErr("the load", nil, nil); err == nil {
		t.Error("a missing result is not a load")
	}
	cause := errors.New("connection refused")
	if err := loadErr("the load", nil, cause); !errors.Is(err, cause) {
		t.Errorf("the cause should be kept: %v", err)
	}
}
