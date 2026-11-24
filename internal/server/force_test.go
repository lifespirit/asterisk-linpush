package server

import (
	"context"
	"testing"
)

func TestForceWake(t *testing.T) {
	s := newBatchServer(t, &slowSender{})
	addBatch(t, s, "5", "device_token_value_123456789")
	if err := s.DB.SetEnabled("5", false); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	zero, one := 0, 1
	for _, force := range []bool{false, true} {
		want := 0
		if force {
			want = 1
		}
		for _, req := range []WakeRequest{
			{Number: "5", Force: force, WaitSeconds: &zero},
			{Number: "5", Force: force, WaitRegisterSeconds: &zero},
			{Number: "5", Force: force, WaitReadySeconds: &one},
		} {
			out, code, err := s.Wake(ctx, req)
			if err != nil || code != 200 || out.Sent != want {
				t.Fatalf("single force=%v: %+v %d %v", force, out, code, err)
			}
		}
		for _, req := range []WakeRequest{
			{Numbers: []string{"5", "6"}, Force: force, WaitSeconds: &zero},
			{Numbers: []string{"5", "6"}, Force: force, WaitReadySeconds: &one},
		} {
			out, code, err := s.WakeMany(ctx, req)
			if err != nil || code != 200 || out.Sent != want {
				t.Fatalf("batch force=%v: %+v %d %v", force, out, code, err)
			}
		}
	}
	ext, found, err := s.DB.GetExtension("5")
	if err != nil || !found || ext.PushEnabled {
		t.Fatalf("force changed extension policy: %+v %v", ext, err)
	}
	if err := s.DB.DisableDevice("5", "primary", "UNREGISTERED"); err != nil {
		t.Fatal(err)
	}
	out, code, err := s.Wake(ctx, WakeRequest{Number: "5", Force: true, WaitSeconds: &zero})
	if err != nil || code != 200 || out.Status != "no_devices" || out.Sent != 0 {
		t.Fatalf("force used inactive token: %+v %d %v", out, code, err)
	}
}
