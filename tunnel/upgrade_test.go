package tunnel

import (
	"bytes"
	"strings"
	"testing"
)

func TestUpgradeFraming(t *testing.T) {
	for _, c := range []*Control{{Upgrade: &Upgrade{Version: "v1.2.3"}}, {UpgradeStatus: &UpgradeStatus{Version: "v1.2.3", State: "started"}}, {UpgradeStatus: &UpgradeStatus{Version: "v1.2.3", State: "failed"}}, {UpgradeStatus: &UpgradeStatus{Version: "v1.2.3", State: "unsupported"}}, {UpgradeStatus: &UpgradeStatus{Version: "v1.2.3", State: "declined"}}} {
		var b bytes.Buffer
		if err := WriteControl(&b, c); err != nil {
			t.Fatal(err)
		}
		got, err := ReadControl(&b)
		if err != nil {
			t.Fatal(err)
		}
		if got.Upgrade == nil && got.UpgradeStatus == nil {
			t.Fatal("missing payload")
		}
	}
	for _, c := range []*Control{{Upgrade: &Upgrade{Version: strings.Repeat("a", 65)}}, {UpgradeStatus: &UpgradeStatus{State: "secret reason"}}, {Upgrade: &Upgrade{}, Heartbeat: &Heartbeat{}}} {
		if err := c.Validate(); err == nil {
			t.Fatal("accepted invalid envelope")
		}
	}
}
