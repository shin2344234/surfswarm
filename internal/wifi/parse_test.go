package wifi

import "testing"

const iwSample = `Connected to 04:D4:C4:12:34:56 (on wlan0)
	SSID: LabNet
	freq: 5180
	RX: 12345678 bytes (12345 packets)
	TX: 1234567 bytes (1234 packets)
	signal: -52 dBm
	rx bitrate: 866.7 MBit/s VHT-MCS 9 80MHz short GI VHT-NSS 2
	tx bitrate: 780.0 MBit/s VHT-MCS 8 80MHz short GI VHT-NSS 2
	bss flags: short-slot-time
	dtim period: 1
	beacon int: 100
`

func TestParseIWLink(t *testing.T) {
	w := ParseIWLink(iwSample)
	if w == nil {
		t.Fatal("nil")
	}
	if w.BSSID != "04:d4:c4:12:34:56" || w.SSID != "LabNet" || w.FreqMHz != 5180 || w.Channel != 36 || w.Band != "5" {
		t.Fatalf("identity: %+v", w)
	}
	if w.SignalDBm != -52 || w.TxMbps != 780 || w.RxMbps != 866.7 || w.WidthMHz != 80 || w.PHY != "11ac" {
		t.Fatalf("link: %+v", w)
	}
	if ParseIWLink("Not connected.\n") != nil {
		t.Fatal("not connected should be nil")
	}
	if ParseIWLink("") != nil {
		t.Fatal("empty should be nil")
	}
}

const netshSample = `
There is 1 interface on the system:

    Name                   : Wi-Fi
    Description            : Intel(R) Wi-Fi 6E AX211 160MHz
    GUID                   : 11111111-2222-3333-4444-555555555555
    Physical address       : aa:bb:cc:dd:ee:01
    Interface type         : Primary
    State                  : connected
    SSID                   : LabNet
    BSSID                  : 04:d4:c4:12:34:56
    Network type           : Infrastructure
    Radio type             : 802.11ax
    Authentication         : WPA2-Personal
    Cipher                 : CCMP
    Connection mode        : Auto Connect
    Band                   : 5 GHz
    Channel                : 44
    Receive rate (Mbps)    : 1201
    Transmit rate (Mbps)   : 1201
    Signal                 : 92%
    Profile                : LabNet

    Hosted network status  : Not available
`

const netshDisconnected = `
There is 1 interface on the system:

    Name                   : Wi-Fi
    Description            : RZ616 Wi-Fi 6E 160MHz
    GUID                   : 55c24743-f43b-4977-abe3-2aaec01526e3
    Physical address       : 3c:0a:f3:d9:97:df
    Interface type         : Primary
    State                  : disconnected
    Radio status           : Hardware On
                             Software Off
`

func TestParseNetsh(t *testing.T) {
	w := ParseNetsh(netshSample)
	if w == nil {
		t.Fatal("nil")
	}
	if w.Interface != "Wi-Fi" || w.SSID != "LabNet" || w.BSSID != "04:d4:c4:12:34:56" || w.Band != "5" || w.Channel != 44 {
		t.Fatalf("identity: %+v", w)
	}
	if w.TxMbps != 1201 || w.RxMbps != 1201 || w.PHY != "11ax" || w.SignalDBm != -54 {
		t.Fatalf("link: %+v", w)
	}
	if ParseNetsh(netshDisconnected) != nil {
		t.Fatal("disconnected should be nil")
	}
}

const wdutilSample = `————————————————————————————————————————————————————————————————————
NETWORK
————————————————————————————————————————————————————————————————————
    Primary IPv4         : en0 (Wi-Fi / 1A2B3C4D-1111-2222-3333-444455556666)
————————————————————————————————————————————————————————————————————
WIFI
————————————————————————————————————————————————————————————————————
    MAC Address          : aa:bb:cc:dd:ee:02 (hw=aa:bb:cc:dd:ee:02)
    Interface Name       : en0
    Power                : On [On,On]
    Op Mode              : STA
    SSID                 : LabNet
    BSSID                : 04:d4:c4:12:34:57
    RSSI                 : -48 dBm
    CCA                  : 12 %
    Noise                : -92 dBm
    Tx Rate              : 1200.0 Mbps
    PHY Mode             : 11ax
    MCS Index            : 11
    Guard Interval       : 800
    NSS                  : 2
    Channel              : 6g37/160
    Country Code         : US
————————————————————————————————————————————————————————————————————
BLUETOOTH
————————————————————————————————————————————————————————————————————
    Power                : On
`

func TestParseWdutil(t *testing.T) {
	w := ParseWdutil(wdutilSample)
	if w == nil {
		t.Fatal("nil")
	}
	if w.Interface != "en0" || w.SSID != "LabNet" || w.BSSID != "04:d4:c4:12:34:57" {
		t.Fatalf("identity: %+v", w)
	}
	if w.SignalDBm != -48 || w.NoiseDBm != -92 || w.TxMbps != 1200 || w.PHY != "11ax" || w.Band != "6" || w.Channel != 37 || w.WidthMHz != 160 {
		t.Fatalf("link: %+v", w)
	}
	redacted := ParseWdutil("WIFI\n    SSID : <redacted>\n    BSSID : <redacted>\n    Channel : 5g44/80\n")
	if redacted == nil || redacted.SSID != "" || redacted.BSSID != "" || redacted.Channel != 44 {
		t.Fatalf("redacted: %+v", redacted)
	}
}

// Captured from a MacBook running macOS 26 as a non-root user, trimmed.
const systemProfilerSample = `{
  "SPAirPortDataType" : [
    {
      "spairport_airport_interfaces" : [
        {
          "_name" : "en0",
          "spairport_current_network_information" : {
            "_name" : "<redacted>",
            "spairport_network_channel" : "37 (6GHz, 160MHz)",
            "spairport_network_country_code" : "US",
            "spairport_network_mcs" : 6,
            "spairport_network_phymode" : "802.11ax",
            "spairport_network_rate" : 1152,
            "spairport_network_type" : "spairport_network_type_station",
            "spairport_security_mode" : "spairport_security_mode_wpa3_personal",
            "spairport_signal_noise" : "-52 dBm / -90 dBm"
          },
          "spairport_status_information" : "spairport_status_connected"
        },
        {
          "_name" : "awdl0",
          "spairport_current_network_information" : {
            "spairport_network_type" : "spairport_network_type_station"
          }
        }
      ]
    }
  ]
}`

func TestParseSystemProfiler(t *testing.T) {
	w := ParseSystemProfiler(systemProfilerSample)
	if w == nil {
		t.Fatal("nil")
	}
	if w.Interface != "en0" || w.SSID != "" || w.Band != "6" || w.Channel != 37 || w.WidthMHz != 160 {
		t.Fatalf("identity: %+v", w)
	}
	if w.SignalDBm != -52 || w.NoiseDBm != -90 || w.TxMbps != 1152 || w.PHY != "11ax" {
		t.Fatalf("link: %+v", w)
	}
	if ParseSystemProfiler("not json") != nil {
		t.Fatal("garbage should be nil")
	}
}

func TestParseIpconfigSummary(t *testing.T) {
	ssid, bssid := ParseIpconfigSummary("  BSSID : <redacted>\n  InterfaceType : WiFi\n  SSID : <redacted>\n")
	if ssid != "" || bssid != "" {
		t.Fatalf("redacted: %q %q", ssid, bssid)
	}
	ssid, bssid = ParseIpconfigSummary("  BSSID : 04:D4:C4:12:34:56\n  SSID : LabNet\n  Security : WPA3_SAE\n")
	if ssid != "LabNet" || bssid != "04:d4:c4:12:34:56" {
		t.Fatalf("visible: %q %q", ssid, bssid)
	}
}

func TestChannelHelpers(t *testing.T) {
	cases := []struct {
		freq    int
		channel int
		band    string
	}{
		{2412, 1, "2.4"}, {2437, 6, "2.4"}, {2484, 14, "2.4"},
		{5180, 36, "5"}, {5745, 149, "5"},
		{5955, 1, "6"}, {6135, 37, "6"},
	}
	for _, c := range cases {
		if got := channelForFreq(c.freq); got != c.channel {
			t.Errorf("channelForFreq(%d) = %d, want %d", c.freq, got, c.channel)
		}
		if got := bandForFreq(c.freq); got != c.band {
			t.Errorf("bandForFreq(%d) = %q, want %q", c.freq, got, c.band)
		}
	}
	if percentToDBm(92) != -54 || percentToDBm(0) != -100 || percentToDBm(100) != -50 || percentToDBm(150) != -50 {
		t.Fatal("percentToDBm mapping")
	}
	if v, ok := leadingNumber("-52 dBm"); !ok || v != -52 {
		t.Fatalf("leadingNumber: %v %v", v, ok)
	}
	if _, ok := leadingNumber("dBm"); ok {
		t.Fatal("no number should fail")
	}
}
