package notify

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func TestModuleUrgentNoticesStillHonourTheMute(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_ = e.s.SetPref(ctx, "user:a", "forms.reminder", ChannelInApp, false)
	if _, c, _ := e.s.Notify(ctx, Notice{Subject: "user:a", Type: "forms.reminder", Urgent: true, RespectMute: true}); c {
		t.Fatal("a module cannot claim urgency against a mute")
	}
	if _, c, _ := e.s.Notify(ctx, Notice{Subject: "user:a", Type: "forms.reminder", Urgent: true}); !c {
		t.Fatal("platform urgent notices still bypass the mute")
	}
}

func TestInAppNoticesPerRecipientPerHourAreCapped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < MaxInAppPerHour+30; i++ {
		_, _, _ = e.s.Notify(ctx, Notice{Subject: "user:a", Type: "x.y"})
	}
	var n int
	_ = e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications`).Scan(&n)
	if n != MaxInAppPerHour {
		t.Fatalf("stored %d, want the cap %d", n, MaxInAppPerHour)
	}
}

func TestRuleTypesStayInTheModuleNamespaceAndKeysAreModuleScoped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mk := func(id string, rule config.NotifyRule) {
		m := store.Module{ID: id, Version: "1", Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, HMACSecret: "s", Manifest: config.Module{ID: id, Notifications: []config.NotifyRule{rule}}}
		if err := e.db.UpsertModule(ctx, m, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	mk("m1", config.NotifyRule{Event: "lead.created.v1", Type: "approval.requested", Recipients: []string{"user:user:a"}}) // spoofing a platform type
	mk("m2", config.NotifyRule{Event: "lead.created.v1", Recipients: []string{"user:user:b"}})
	mk("m3", config.NotifyRule{Event: "lead.created.v1", Recipients: []string{"user:user:b"}}) // same default type and event as m2
	e.bus.SubscribeAll(e.s.HandleEvent)
	if _, err := e.bus.Publish(ctx, "m2", "lead.created.v1", json.RawMessage(`{"lead_id":"l1"}`)); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.s.UnreadCount(ctx, "user:a"); n != 0 {
		t.Fatal("a rule with a type outside its module namespace must be ignored")
	}
	if n, _ := e.s.UnreadCount(ctx, "user:b"); n != 2 {
		t.Fatalf("b got %d, want one per module: one module's key must not suppress another's", n)
	}
}

func TestWebhookDialerRefusesPrivateAddresses(t *testing.T) {
	for _, a := range []string{"127.0.0.1:443", "10.0.0.5:443", "192.168.1.1:443", "169.254.169.254:80", "[::1]:443", "[fd00::1]:443", "0.0.0.0:80", "[::ffff:10.1.1.1]:443", "100.64.0.1:443", "[64:ff9b::a00:1]:443", "[64:ff9b::7f00:1]:443", "[2002:c0a8:101::1]:443", "192.0.0.8:443", "198.18.0.1:443", "0.1.2.3:443", "198.19.255.255:443"} {
		if err := denyPrivate("tcp", a, nil); err == nil {
			t.Errorf("%s accepted", a)
		}
	}
	for _, a := range []string{"93.184.216.34:443", "[2606:2800:220:1::1]:443", "[64:ff9b::5db8:d822]:443", "100.63.255.255:443", "198.20.0.1:443"} {
		if err := denyPrivate("tcp", a, nil); err != nil {
			t.Errorf("%s refused: %v", a, err)
		}
	}
}
