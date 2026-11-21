package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestHistogramAndAMI(t *testing.T) {
	r := New()
	r.Start()
	r.FCM("accepted")
	r.AMI(true, true, false, 7, 1)
	r.Wake("batch", "sent", 110*time.Millisecond, 0, 3)
	var b bytes.Buffer
	r.Write(&b)
	txt := b.String()
	for _, want := range []string{
		`linpush_ami_connected 1`,
		`linpush_ami_contacts 7`,
		`linpush_ami_fcm_contacts 1`,
		`linpush_wake_inflight 0`,
		`linpush_wake_targets_count 1`,
		`linpush_wake_targets_bucket{le="2"} 0`,
		`linpush_wake_targets_bucket{le="3"} 1`,
		`linpush_wake_duration_seconds_count 1`,
		`linpush_fcm_messages_total{outcome="accepted"} 1`,
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing metric: %s", want)
		}
	}
}
