package protocol

import (
	"encoding/json"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := Progress{
		TestID: "t1", TS: 42, Requests: 3, Bytes: 4096, Mbps: 1.5, P95Ms: 12.5,
		NewErrors: []RequestError{{TS: 41, URL: "https://x.example/", Class: "blocked", Status: 403, Message: "HTTP 403 Forbidden"}},
		Wifi:      &WifiInfo{SSID: "lab", BSSID: "aa:bb:cc:dd:ee:ff", SignalDBm: -55, Band: "5", Channel: 44},
	}
	b, err := Encode(TypeProgress, in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	env, err := Decode(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Type != TypeProgress {
		t.Fatalf("type = %q, want %q", env.Type, TypeProgress)
	}
	var out Progress
	if err := json.Unmarshal(env.Data, &out); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if out.TestID != in.TestID || out.Requests != in.Requests || out.Bytes != in.Bytes || out.Mbps != in.Mbps || out.P95Ms != in.P95Ms {
		t.Fatalf("round trip changed fields: %+v", out)
	}
	if len(out.NewErrors) != 1 || out.NewErrors[0].Status != 403 || out.NewErrors[0].Class != "blocked" {
		t.Fatalf("error log lost: %+v", out.NewErrors)
	}
	if out.Wifi == nil || out.Wifi.BSSID != "aa:bb:cc:dd:ee:ff" || out.Wifi.SignalDBm != -55 {
		t.Fatalf("wifi lost: %+v", out.Wifi)
	}
}

func TestEncodeWithoutData(t *testing.T) {
	b, err := Encode(TypePing, nil)
	if err != nil {
		t.Fatal(err)
	}
	env, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypePing || len(env.Data) != 0 {
		t.Fatalf("got %+v", env)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := Decode([]byte("{not json")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestOptionalFieldsOmitted(t *testing.T) {
	b, _ := Encode(TypeHeartbeat, Heartbeat{TS: 1})
	if s := string(b); contains(s, "wifi") {
		t.Fatalf("nil wifi should be omitted: %s", s)
	}
	b, _ = Encode(TypeProgress, Progress{TestID: "x"})
	if s := string(b); contains(s, "new_errors") {
		t.Fatalf("empty error batch should be omitted: %s", s)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
