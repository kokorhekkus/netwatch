//go:build darwin && cgo

package wifi

/*
#cgo CFLAGS: -x objective-c -fmodules -fobjc-arc
#cgo LDFLAGS: -framework CoreWLAN -framework Foundation
#import <CoreWLAN/CoreWLAN.h>

typedef struct {
    int    ok;
    int    rssi;
    int    noise;
    double txRate;
    int    channel;
    int    width;
    int    band;
    int    phyMode;
    char   ifname[32];
} nw_wifi;

// nw_wifi_read samples the active Wi-Fi interface.
//
// Every field here is readable without Location Services. Only ssid/bssid are
// gated, and they are deliberately not requested: prompting a background agent
// for location access would be a poor trade for data we do not need.
static nw_wifi nw_wifi_read(void) {
    nw_wifi s;
    memset(&s, 0, sizeof(s));
    @autoreleasepool {
        CWWiFiClient *client = [CWWiFiClient sharedWiFiClient];
        if (!client) return s;
        CWInterface *iface = [client interface];
        if (!iface) return s;

        // A powered-off or unassociated radio reports a zero RSSI; treating
        // that as a real measurement would put a bogus 0 dBm in the history.
        if (![iface powerOn]) return s;

        s.rssi    = (int)[iface rssiValue];
        s.noise   = (int)[iface noiseMeasurement];
        s.txRate  = [iface transmitRate];
        s.phyMode = (int)[iface activePHYMode];

        CWChannel *ch = [iface wlanChannel];
        if (ch) {
            s.channel = (int)[ch channelNumber];
            s.width   = (int)[ch channelWidth];
            s.band    = (int)[ch channelBand];
        }
        NSString *n = [iface interfaceName];
        if (n) strncpy(s.ifname, [n UTF8String], sizeof(s.ifname)-1);

        // phyMode 0 means not associated, whatever the other fields say.
        s.ok = (s.phyMode != 0 && s.rssi != 0) ? 1 : 0;
    }
    return s;
}
*/
import "C"

// Read samples the radio. It costs about 4ms, against the 4.2 seconds that
// `system_profiler SPAirPortDataType` takes - and system_profiler also
// triggers a scan for nearby networks, which can itself cause the latency
// spike being measured.
func Read() Sample {
	c := C.nw_wifi_read()
	if c.ok == 0 {
		return Sample{}
	}
	return Sample{
		Supported: true,
		Interface: C.GoString(&c.ifname[0]),
		RSSI:      int(c.rssi),
		Noise:     int(c.noise),
		TxRate:    float64(c.txRate),
		Channel:   int(c.channel),
		WidthMHz:  widthMHz(int(c.width)),
		Band:      bandName(int(c.band)),
		PHYMode:   phyName(int(c.phyMode)),
	}
}
