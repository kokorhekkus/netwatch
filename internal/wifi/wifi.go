// Package wifi reads the local radio's state.
//
// On a Wi-Fi connection a large share of what feels like "bad internet" is
// really a bad radio link, and the two are indistinguishable from latency
// alone: a weak signal and a congested ISP both show up as jitter and loss.
// Sampling RSSI alongside latency is what makes them tell apart.
package wifi

import "fmt"

// Sample is one reading of the radio.
//
// SSID and BSSID are deliberately absent. macOS gates them behind Location
// Services, and everything here is readable without prompting - which matters
// for something that runs as a background agent. Network identity comes from
// the gateway MAC instead.
type Sample struct {
	Supported bool
	Interface string
	RSSI      int     // dBm, typically -30 (excellent) to -90 (unusable)
	Noise     int     // dBm
	TxRate    float64 // Mbit/s, the negotiated PHY rate
	Channel   int
	WidthMHz  int
	Band      string // "2.4GHz" | "5GHz" | "6GHz"
	PHYMode   string // "802.11ax" etc.
}

// SNR is the margin between signal and noise floor, in dB.
//
// This predicts link quality better than RSSI alone: -60 dBm in a quiet band
// is a good link, while -60 dBm against a -65 dBm noise floor is not.
func (s Sample) SNR() int {
	if !s.Supported || s.RSSI == 0 || s.Noise == 0 {
		return 0
	}
	return s.RSSI - s.Noise
}

// Quality describes the link in the terms someone would actually act on.
func (s Sample) Quality() string {
	snr := s.SNR()
	switch {
	case snr == 0:
		return "unknown"
	case snr >= 40:
		return "excellent"
	case snr >= 25:
		return "good"
	case snr >= 15:
		return "fair"
	default:
		return "poor"
	}
}

func (s Sample) String() string {
	if !s.Supported {
		return "wifi: not available"
	}
	return fmt.Sprintf("%s %d dBm (SNR %d dB, %s) %s ch%d/%dMHz %.0f Mbit/s",
		s.Interface, s.RSSI, s.SNR(), s.Quality(), s.PHYMode, s.Channel, s.WidthMHz, s.TxRate)
}

// Channel width and band enumerations as CoreWLAN reports them.
func bandName(v int) string {
	switch v {
	case 1:
		return "2.4GHz"
	case 2:
		return "5GHz"
	case 3:
		return "6GHz"
	}
	return ""
}

func widthMHz(v int) int {
	switch v {
	case 1:
		return 20
	case 2:
		return 40
	case 3:
		return 80
	case 4:
		return 160
	}
	return 0
}

func phyName(v int) string {
	switch v {
	case 1:
		return "802.11a"
	case 2:
		return "802.11b"
	case 3:
		return "802.11g"
	case 4:
		return "802.11n"
	case 5:
		return "802.11ac"
	case 6:
		return "802.11ax"
	case 7:
		return "802.11be"
	}
	return ""
}
