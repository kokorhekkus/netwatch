package wifi

import "testing"

func TestReadLiveRadio(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the local radio")
	}
	s := Read()
	if !s.Supported {
		t.Skip("no Wi-Fi association (Ethernet, radio off, or built without cgo)")
	}

	t.Logf("%s", s)

	// Sanity-check against physics rather than against a fixed value: RSSI is
	// always negative, and a usable link is somewhere between -20 and -95 dBm.
	if s.RSSI >= 0 || s.RSSI < -100 {
		t.Errorf("RSSI = %d dBm, outside any plausible range", s.RSSI)
	}
	if s.Noise >= 0 || s.Noise < -120 {
		t.Errorf("Noise = %d dBm, outside any plausible range", s.Noise)
	}
	if s.SNR() <= 0 {
		t.Errorf("SNR = %d dB; signal should sit above the noise floor on a live link", s.SNR())
	}
	if s.TxRate <= 0 {
		t.Errorf("TxRate = %v, want a positive PHY rate", s.TxRate)
	}
	if s.Channel <= 0 {
		t.Errorf("Channel = %d", s.Channel)
	}
	if s.Band == "" {
		t.Error("band not decoded")
	}
	if s.PHYMode == "" {
		t.Error("PHY mode not decoded")
	}
	if s.Interface == "" {
		t.Error("interface name missing")
	}
}

func TestSNRAndQuality(t *testing.T) {
	for _, tc := range []struct {
		rssi, noise int
		wantSNR     int
		wantQuality string
	}{
		{-50, -93, 43, "excellent"},
		{-65, -95, 30, "good"},
		{-75, -93, 18, "fair"},
		{-85, -93, 8, "poor"},
	} {
		s := Sample{Supported: true, RSSI: tc.rssi, Noise: tc.noise}
		if got := s.SNR(); got != tc.wantSNR {
			t.Errorf("SNR(%d,%d) = %d, want %d", tc.rssi, tc.noise, got, tc.wantSNR)
		}
		if got := s.Quality(); got != tc.wantQuality {
			t.Errorf("Quality(%d,%d) = %q, want %q", tc.rssi, tc.noise, got, tc.wantQuality)
		}
	}
}

// An unassociated or Ethernet-only machine must report nothing rather than a
// plausible-looking 0 dBm, which would otherwise be stored as a real reading.
func TestUnsupportedSampleIsInert(t *testing.T) {
	var s Sample
	if s.SNR() != 0 {
		t.Errorf("SNR = %d on an empty sample, want 0", s.SNR())
	}
	if s.Quality() != "unknown" {
		t.Errorf("Quality = %q on an empty sample, want \"unknown\"", s.Quality())
	}
	if s.String() != "wifi: not available" {
		t.Errorf("String = %q", s.String())
	}
}

func TestDecoders(t *testing.T) {
	if got := widthMHz(4); got != 160 {
		t.Errorf("widthMHz(4) = %d, want 160", got)
	}
	if got := bandName(2); got != "5GHz" {
		t.Errorf("bandName(2) = %q, want 5GHz", got)
	}
	if got := phyName(6); got != "802.11ax" {
		t.Errorf("phyName(6) = %q, want 802.11ax", got)
	}
	// Unknown enum values must not invent a label.
	if bandName(99) != "" || phyName(99) != "" || widthMHz(99) != 0 {
		t.Error("decoders fabricated a value for an unknown enum")
	}
}
